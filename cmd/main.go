/*
Copyright 2022 Lightrun

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	zaplog "go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
	"github.com/lightrun-platform/lightrun-k8s-operator/internal/controller"
	"github.com/lightrun-platform/lightrun-k8s-operator/internal/controller/clusteragentpool"
	lightrunwebhook "github.com/lightrun-platform/lightrun-k8s-operator/internal/webhook"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	//+kubebuilder:scaffold:imports
)

// webhookCAName and webhookCAOrganization identify the self-signed CA the cert-controller
// library (github.com/open-policy-agent/cert-controller, the same one Gatekeeper uses)
// mints to bootstrap the webhook's TLS serving cert -- replacing cert-manager entirely, per
// "DESIGN CHANGE v3" section 4 of .dot-agent-deck/webhook-refactor-context.md.
const (
	webhookCAName         = "lightrun-k8s-operator-ca"
	webhookCAOrganization = "lightrun"
)

var (
	scheme               = runtime.NewScheme()
	setupLog             = ctrl.Log.WithName("setup")
	watchNamespaceEnvVar = "WATCH_NAMESPACE"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(agentsv1beta.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

// newMirrorSecretCache builds a Secret cache dedicated to
// clusteragentpool.SecretMirrorReconciler, separate from mgr's own shared cache (used by
// mgr.GetClient() everywhere else, in particular LightrunJavaAgentReconciler's unrelated,
// pre-existing, unscoped Secret reads by name) so this change can't affect that controller.
//
// Per the "MEDIUM-HIGH" audit finding ("the process holds plaintext of every Secret in the
// cluster in memory"): this cache still watches/lists every Secret cluster-wide (it must, since
// a ClusterAgentPool's spec.secretRef can name an arbitrary, unlabeled Secret in any namespace,
// and detecting that Secret's rotation requires observing it -- see
// SecretMirrorReconciler.mapSecretToClusterAgentPools), but its Transform scrubs .Data/
// .StringData from every Secret that does NOT carry api/v1beta.ClusterAgentPoolMirrorLabelKey
// before it's committed to the in-memory store. So this controller's own contribution to the
// manager's plaintext-Secret footprint is limited to the mirrors it actually manages -- source
// Secrets and unrelated cluster Secrets are cached as metadata/labels only (enough to detect
// their existence and route reconciles), never their key material. The reconciler always reads
// the real source Secret content via mgr.GetAPIReader() (direct, uncached, never scrubbed) --
// see SecretMirrorReconciler.sourceReader -- so a scrubbed cache entry is never mistaken for
// real secret data.
func newMirrorSecretCache(mgr ctrl.Manager) (cache.Cache, error) {
	return cache.New(mgr.GetConfig(), cache.Options{
		HTTPClient: mgr.GetHTTPClient(),
		Scheme:     mgr.GetScheme(),
		Mapper:     mgr.GetRESTMapper(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Transform: scrubUnmanagedSecretData},
		},
	})
}

// scrubUnmanagedSecretData is the mirror Secret cache's transform (see newMirrorSecretCache):
// it nils out .Data/.StringData on any Secret that doesn't carry
// api/v1beta.ClusterAgentPoolMirrorLabelKey, before the object is committed to that cache's
// in-memory store.
func scrubUnmanagedSecretData(obj interface{}) (interface{}, error) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return obj, nil
	}
	if _, managed := secret.Labels[agentsv1beta.ClusterAgentPoolMirrorLabelKey]; managed {
		return secret, nil
	}
	scrubbed := secret.DeepCopy()
	scrubbed.Data = nil
	scrubbed.StringData = nil
	return scrubbed, nil
}

func getWatchNamespaces() ([]string, error) {
	var namespaces []string
	ns, found := os.LookupEnv(watchNamespaceEnvVar)
	if !found || (found && ns == "") {
		return nil, errors.New(watchNamespaceEnvVar + " Env Var not set or empty")
	}
	nsSlice := strings.Split(ns, ",")
	for _, ns := range nsSlice {
		if ns != "" {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces, nil
}

// RBAC needed by the cert-controller library's CertRotator (github.com/open-policy-agent/
// cert-controller/pkg/rotator), which manages its own Secret and patches the caBundle on the
// MutatingWebhookConfiguration named by --webhook-mutating-webhook-configuration-name. Secret
// verbs beyond get/list/watch (create/update/delete) are also required by
// internal/controller/clusteragentpool.SecretMirrorReconciler -- see that package's own
// kubebuilder markers for the full justification; merged here into one rule by controller-gen.
//
//+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;update
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;delete

func main() {
	var metricsAddr string
	var probeAddr string
	var pprofAddr string
	var enableLeaderElection bool
	var webhookEnabled bool
	var webhookPort int
	var webhookCertDir string
	var webhookSharedVolumeName string
	var webhookSharedVolumeMountPath string
	var webhookInitContainerImage string
	var webhookInitContainerImagePullPolicy string
	var webhookNamespace string
	var webhookServiceName string
	var webhookCertSecretName string
	var webhookMutatingWebhookConfigurationName string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&pprofAddr, "pprof-bind-address", "0", "The address the pprof endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&webhookEnabled, "webhook-enabled", false,
		"Enable the pod-mutating admission webhook. When false (the default), no webhook server is "+
			"started and SetupWebhookWithManager is never called -- required so a default install "+
			"(webhook.enabled=false in the Helm chart, no TLS cert volume mounted) doesn't crash-loop "+
			"trying to watch a nonexistent cert directory.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the pod-mutating webhook server binds to.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs",
		"The directory containing the webhook server's TLS certificate (tls.crt) and key (tls.key).")
	flag.StringVar(&webhookSharedVolumeName, "webhook-shared-volume-name", "lightrun-agent",
		"Name of the emptyDir volume the pod-mutating webhook shares between the lightrun-installer init container and the patched app containers.")
	flag.StringVar(&webhookSharedVolumeMountPath, "webhook-shared-volume-mount-path", "/lightrun",
		"Mount path of the shared agent volume inside the patched app containers.")
	flag.StringVar(&webhookInitContainerImage, "webhook-init-container-image", "lightruncom/k8s-operator-init-java-agent-linux:latest",
		"Image used for the lightrun-installer init container injected by the pod-mutating webhook.")
	flag.StringVar(&webhookInitContainerImagePullPolicy, "webhook-init-container-image-pull-policy", "",
		"Image pull policy for the lightrun-installer init container. Empty uses the cluster default.")
	flag.StringVar(&webhookNamespace, "webhook-namespace", "",
		"Namespace the operator itself (and its webhook Service/cert Secret) runs in. Required when --webhook-enabled.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", "webhook-service",
		"Name of the Service fronting the webhook server, used to build its cert's DNS name.")
	flag.StringVar(&webhookCertSecretName, "webhook-cert-secret-name", "webhook-server-cert",
		"Name of the Secret the self-managed webhook serving certificate (cert-controller, no cert-manager) is stored in.")
	flag.StringVar(&webhookMutatingWebhookConfigurationName, "webhook-mutating-webhook-configuration-name", "",
		"Name of the MutatingWebhookConfiguration whose caBundle is kept in sync with the managed cert. Required when --webhook-enabled.")

	opts := zap.Options{
		Development:     false,
		ZapOpts:         []zaplog.Option{zaplog.AddCaller(), zaplog.AddCallerSkip(-1)},
		StacktraceLevel: zapcore.DPanicLevel,
		TimeEncoder:     zapcore.ISO8601TimeEncoder,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog.Info("Log verbosity", "V", opts.Level)

	options := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "5b425f09.lightrun.com",
		PprofBindAddress:       pprofAddr,

		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	}

	if webhookEnabled {
		options.WebhookServer = webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		})
	}

	watchNamespaces, err := getWatchNamespaces()
	if err != nil {
		setupLog.Info("Controller will watch and manage resources in all namespaces")
	} else {
		setupLog.Info("Controller will watch following namespaces", "namespaces", watchNamespaces)
		options.Cache.DefaultNamespaces = make(map[string]cache.Config)
		for _, namespace := range watchNamespaces {
			options.Cache.DefaultNamespaces[namespace] = cache.Config{}
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)

	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.LightrunJavaAgentReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Log:    ctrl.Log.WithName("controllers").WithName("LightrunJavaAgent"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "LightrunJavaAgent")
		os.Exit(1)
	}

	mirrorSecretCache, err := newMirrorSecretCache(mgr)
	if err != nil {
		setupLog.Error(err, "unable to create scoped Secret cache for ClusterAgentPoolSecretMirror controller")
		os.Exit(1)
	}
	if err := mgr.Add(mirrorSecretCache); err != nil {
		setupLog.Error(err, "unable to register scoped Secret cache")
		os.Exit(1)
	}

	if err = (&clusteragentpool.SecretMirrorReconciler{
		Client:      mgr.GetClient(),
		APIReader:   mgr.GetAPIReader(),
		MirrorCache: mirrorSecretCache,
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorderFor("clusteragentpool-secret-mirror"),
		Log:         ctrl.Log.WithName("controllers").WithName("ClusterAgentPoolSecretMirror"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterAgentPoolSecretMirror")
		os.Exit(1)
	}

	if webhookEnabled {
		if webhookNamespace == "" {
			setupLog.Error(errors.New("--webhook-namespace is required"), "unable to set up webhook cert rotation")
			os.Exit(1)
		}
		if webhookMutatingWebhookConfigurationName == "" {
			setupLog.Error(errors.New("--webhook-mutating-webhook-configuration-name is required"), "unable to set up webhook cert rotation")
			os.Exit(1)
		}

		// Bootstrap the webhook's TLS serving cert ourselves (github.com/open-policy-agent/
		// cert-controller, the same library Gatekeeper uses) instead of depending on
		// cert-manager, per "DESIGN CHANGE v3" section 4 of
		// .dot-agent-deck/webhook-refactor-context.md. AddRotator registers the CertRotator
		// (and its own Secret/MutatingWebhookConfiguration-watching reconciler) as manager
		// Runnables that start with mgr.Start() below; certReady closes once a valid cert has
		// been written to webhookCertDir on disk.
		dnsName := fmt.Sprintf("%s.%s.svc", webhookServiceName, webhookNamespace)
		certReady := make(chan struct{})
		if err := rotator.AddRotator(mgr, &rotator.CertRotator{
			SecretKey:      types.NamespacedName{Namespace: webhookNamespace, Name: webhookCertSecretName},
			CertDir:        webhookCertDir,
			CAName:         webhookCAName,
			CAOrganization: webhookCAOrganization,
			DNSName:        dnsName,
			ExtraDNSNames:  []string{dnsName + ".cluster.local"},
			IsReady:        certReady,
			Webhooks: []rotator.WebhookInfo{
				{Name: webhookMutatingWebhookConfigurationName, Type: rotator.Mutating},
			},
		}); err != nil {
			setupLog.Error(err, "unable to set up webhook cert rotation")
			os.Exit(1)
		}

		// SetupWebhookWithManager is deferred until the cert exists (IsReady) -- this is what
		// avoids the manager-crash-loop class of bug: the webhook server is only added to the
		// manager (via mgr.GetWebhookServer()) once its TLS cert is actually on disk, instead
		// of racing the CertRotator at mgr.Start() time. Runnables (including the webhook
		// server) added to an already-started manager are started immediately, so it's safe
		// for this to run concurrently with (and typically after) mgr.Start() below.
		go func() {
			<-certReady
			if err := lightrunwebhook.SetupWebhookWithManager(mgr, lightrunwebhook.Config{
				SharedVolumeName:             webhookSharedVolumeName,
				SharedVolumeMountPath:        webhookSharedVolumeMountPath,
				InitContainerImage:           webhookInitContainerImage,
				InitContainerImagePullPolicy: corev1.PullPolicy(webhookInitContainerImagePullPolicy),
			}); err != nil {
				setupLog.Error(err, "unable to create webhook", "webhook", "Pod")
				os.Exit(1)
			}
		}()
	} else {
		setupLog.Info("Pod-mutating webhook disabled (--webhook-enabled=false)")
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
