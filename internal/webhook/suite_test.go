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

package webhook

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

// These tests use Ginkgo + envtest, mirroring internal/controller/suite_test.go, but
// additionally boot envtest's built-in webhook server support and register a
// MutatingWebhookConfiguration for Pods against it, per the annotation-driven mutating
// webhook design in .dot-agent-deck/webhook-refactor-context.md.
//
// Per the "DESIGN CHANGE" section of webhook-refactor-context.md (2026-07-14):
// pod_mutator.go resolves an AgentPool CR (namespaced only) named by lightrun.com/agent-pool,
// instead of reading lightrun.com/secret-name + lightrun.com/server-hostname straight off the Pod.
// CRDDirectoryPaths below installs that CRD (config/crd/bases/agents.lightrun.com_agentpools.yaml)
// so specs can create pool CRs.
//
// Contract this suite assumes the coder will implement (see pod_mutator_test.go for the
// full annotation/behavior contract):
//   - Config{SharedVolumeName, SharedVolumeMountPath, InitContainerImage,
//     InitContainerImagePullPolicy} -- operator-level defaults (would come from Helm
//     values / webhook manager flags in production, passed in directly here for tests).
//   - SetupWebhookWithManager(mgr ctrl.Manager, cfg Config) error -- wires a mutating
//     admission.CustomDefaulter for corev1.Pod onto mgr.GetWebhookServer(), the same way
//     `kubebuilder create webhook --group core --version v1 --kind Pod --defaulting`
//     would scaffold it. The generated path for a core/v1 Pod defaulting webhook is
//     "/mutate--v1-pod" (see sigs.k8s.io/controller-runtime/pkg/builder.generateMutatePath),
//     which is why that literal path is used below to build the MutatingWebhookConfiguration.

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

// testNamespace is created once in BeforeSuite; individual specs create their Pods (and
// any Secrets/AgentPools they reference) inside it.
const testNamespace = "lightrun"

// Shared operator-level defaults passed into SetupWebhookWithManager. Test files assert
// against these same constants, so keep them in sync with any values used in expectations.
const (
	sharedVolumeName      = "lightrun-agent"
	sharedVolumeMountPath = "/lightrun"
	initContainerImage    = "lightruncom/lightrun-init-agent:latest"
)

func TestWebhook(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Pod mutating webhook Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment with webhook support")

	mutatePodPath := "/mutate--v1-pod"
	failurePolicy := admissionregistrationv1.Ignore // webhook unavailability must not block pod scheduling
	sideEffects := admissionregistrationv1.SideEffectClassNone

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			MutatingWebhooks: []*admissionregistrationv1.MutatingWebhookConfiguration{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "lightrun-pod-mutator"},
					Webhooks: []admissionregistrationv1.MutatingWebhook{
						{
							Name:                    "mpod.lightrun.com",
							AdmissionReviewVersions: []string{"v1"},
							SideEffects:             &sideEffects,
							FailurePolicy:           &failurePolicy,
							ClientConfig: admissionregistrationv1.WebhookClientConfig{
								Service: &admissionregistrationv1.ServiceReference{
									Name:      "webhook-service",
									Namespace: "system",
									Path:      &mutatePodPath,
								},
							},
							Rules: []admissionregistrationv1.RuleWithOperations{
								{
									Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
									Rule: admissionregistrationv1.Rule{
										APIGroups:   []string{""},
										APIVersions: []string{"v1"},
										Resources:   []string{"pods"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = agentsv1beta.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	webhookInstallOptions := &testEnv.WebhookInstallOptions

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    webhookInstallOptions.LocalServingHost,
			Port:    webhookInstallOptions.LocalServingPort,
			CertDir: webhookInstallOptions.LocalServingCertDir,
		}),
		LeaderElection: false,
		Metrics:        metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	// This is the mutating webhook under test -- not implemented yet (RED phase).
	err = SetupWebhookWithManager(k8sManager, Config{
		SharedVolumeName:      sharedVolumeName,
		SharedVolumeMountPath: sharedVolumeMountPath,
		InitContainerImage:    initContainerImage,
	})
	Expect(err).NotTo(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "failed to run manager")
	}()

	// Wait for the webhook server to be reachable before any spec tries to create a Pod.
	dialer := &net.Dialer{Timeout: time.Second}
	addrPort := fmt.Sprintf("%s:%d", webhookInstallOptions.LocalServingHost, webhookInstallOptions.LocalServingPort)
	Eventually(func() error {
		conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true}) // nolint:gosec // local envtest cert
		if err != nil {
			return err
		}
		return conn.Close()
	}).Should(Succeed())

	By("creating the test namespace")
	Expect(k8sClient.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
	})).Should(Succeed())
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
