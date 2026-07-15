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

// Package webhook implements a mutating admission webhook that injects the Lightrun agent
// into Pods carrying lightrun.com/* annotations, replacing the old controller-based
// Deployment/StatefulSet patching mechanism. See .dot-agent-deck/webhook-refactor-context.md
// for the full design contract, in particular "DESIGN CHANGE v3" (2026-07-15), which this file
// implements.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

// Annotation keys under the lightrun.com/ prefix. Per "DESIGN CHANGE v3" of
// .dot-agent-deck/webhook-refactor-context.md (2026-07-15): lightrun.com/inject-java is now
// the trigger annotation (annotationAgentType survives only as an optional per-pod override of
// *which* agent type to dispatch to, defaulting to "java"); annotationAgentPool survives only
// as the legacy (pre-v3, 2026-07-14 "DESIGN CHANGE") trigger+pool-name annotation, still
// honored when a pod uses neither its own nor its namespace's lightrun.com/inject-java
// annotation, so pods/workloads written against that earlier contract keep working unchanged.
const (
	annotationAgentType       = "lightrun.com/agent-type"
	annotationAgentPool       = "lightrun.com/agent-pool"
	annotationInjectJava      = "lightrun.com/inject-java"
	annotationAgentPoolKind   = "lightrun.com/agent-pool-kind"
	annotationContainerSel    = "lightrun.com/container-selector"
	annotationAgentEnvVarName = "lightrun.com/agent-env-var-name"
	annotationAgentCliFlags   = "lightrun.com/agent-cli-flags"
	annotationAgentTags       = "lightrun.com/agent-tags"
	annotationAgentName       = "lightrun.com/agent-name"
	annotationAgentConfig     = "lightrun.com/agent-config"
	annotationUseMountedFiles = "lightrun.com/use-secrets-as-mounted-files"
)

// Pool kinds a pod may resolve to, per lightrun.com/agent-pool-kind (default AgentPool).
const (
	poolKindAgentPool        = "AgentPool"
	poolKindClusterAgentPool = "ClusterAgentPool"
)

// defaultPoolName is the pool name lightrun.com/inject-java: "true" resolves to.
const defaultPoolName = "default"

// lightrunInitContainerName and lightrunSecretVolumeName mirror the names the old
// internal/controller/patch_funcs.go mechanism used (initContainerName / "lightrun-secret");
// kept identical since the injection mechanics carry over conceptually.
const (
	lightrunInitContainerName = "lightrun-installer"
	lightrunSecretVolumeName  = "lightrun-secret"
)

// cmVolumeName and cmMountPath mirror internal/controller/patch_funcs.go's cmVolumeName /
// "/tmp/cm/" mount path -- the real lightruncom/k8s-operator-init-java-agent-* init container
// image's entrypoint reads agent.config (via awk) and agent.metadata.json from this exact
// path, regardless of which mechanism (controller or webhook) staged the ConfigMap.
const (
	cmVolumeName       = "lightrunagent-config"
	cmMountPath        = "/tmp/cm/"
	cmNamePrefix       = "lightrun-agent-cm-"
	cmDataKeyConfig    = "config"
	cmDataKeyMetadata  = "metadata"
	cmItemPathConfig   = "agent.config"
	cmItemPathMetadata = "agent.metadata.json"
)

const (
	lightrunSecretKeyLightrunKey    = "lightrun_key"
	lightrunSecretKeyPinnedCertHash = "pinned_cert_hash"
)

// Config holds operator-level defaults for the injected init container and shared volume.
// In production these come from Helm values / webhook manager flags; tests pass them in
// directly.
type Config struct {
	// SharedVolumeName is the name of the emptyDir volume shared between the init container
	// and the selected app containers.
	SharedVolumeName string
	// SharedVolumeMountPath is where the shared volume is mounted in the selected app
	// containers (the init container itself stages the agent under /tmp/).
	SharedVolumeMountPath string
	// InitContainerImage is the image used for the lightrun-installer init container.
	InitContainerImage string
	// InitContainerImagePullPolicy is optional; when empty the cluster default is used.
	InitContainerImagePullPolicy corev1.PullPolicy
}

// agentMutator mutates pod in place for a single, recognized agent-type value. Returning an
// error denies the admission request (see podMutator.Default). reader is used for the live
// API reads that resolve the pool/namespace; writer is used to create the ConfigMap the init
// container reads its config from.
type agentMutator func(ctx context.Context, reader client.Reader, writer client.Client, pod *corev1.Pod, cfg Config, decision *injectDecision) error

// agentMutators is the agent-type dispatch registry. Only "java" is implemented today; a
// future "nodejs"/"python" mutator is added here without touching pod-targeting logic.
var agentMutators = map[string]agentMutator{
	"java": mutateJavaPod,
}

// podMutator implements admission.CustomDefaulter for corev1.Pod.
type podMutator struct {
	cfg    Config
	reader client.Reader
	writer client.Client
}

var _ admission.CustomDefaulter = &podMutator{}

// Default is invoked by the generated "/mutate--v1-pod" admission handler for every Pod
// create. Pods that don't opt into a recognized, implemented agent type (see decideInjection)
// are admitted unchanged; a non-nil error here denies the admission request (surfaced to the
// caller of e.g. `kubectl create`).
func (m *podMutator) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected a Pod but got a %T", obj)
	}

	decision, err := decideInjection(ctx, m.reader, pod)
	if err != nil {
		return err
	}
	if decision == nil {
		return nil
	}

	mutate, known := agentMutators[decision.agentType]
	if !known {
		return nil
	}

	return mutate(ctx, m.reader, m.writer, pod, m.cfg, decision)
}

// SetupWebhookWithManager wires the Pod mutating webhook onto mgr's webhook server, the same
// way `kubebuilder create webhook --group core --version v1 --kind Pod --defaulting` would
// scaffold it.

//+kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,admissionReviewVersions=v1,groups="",resources=pods,verbs=create,versions=v1,name=mpod.lightrun.com
//+kubebuilder:rbac:groups=agents.lightrun.com,resources=agentpools,verbs=get;list;watch
//+kubebuilder:rbac:groups=agents.lightrun.com,resources=clusteragentpools,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=namespaces,verbs=get
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update

func SetupWebhookWithManager(mgr ctrl.Manager, cfg Config) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(&podMutator{cfg: cfg, reader: mgr.GetAPIReader(), writer: mgr.GetClient()}).
		Complete()
}

// injectDecision is the outcome of decideInjection: whether (and how) a pod should be
// injected, before any pool has actually been resolved.
type injectDecision struct {
	// agentType is the resolved agent-type dispatch key (e.g. "java").
	agentType string
	// poolKind is either poolKindAgentPool or poolKindClusterAgentPool. Ignored when
	// tryBothKinds is set.
	poolKind string
	// poolName is the pool to resolve within poolKind (or, when tryBothKinds is set, the
	// name to try under both kinds).
	poolName string
	// tryBothKinds is set only for lightrun.com/inject-java: "true", which per the design
	// doc tries a namespaced AgentPool named "default" first, then a cluster-scoped
	// ClusterAgentPool named "default", regardless of any lightrun.com/agent-pool-kind
	// annotation.
	tryBothKinds bool
}

// decideInjection determines whether pod should be injected and, if so, which agent type and
// pool it should resolve against -- without doing the actual pool resolution (see
// resolvePoolConfig). Returns a nil decision (and nil error) when the pod should be admitted
// unchanged.
//
// Precedence (highest first) -- fixed per the "HIGH" audit finding, which flagged that a
// namespace-level default previously won over a pod's own EXPLICIT legacy-style trigger
// whenever the pod omitted lightrun.com/inject-java, letting a namespace admin silently
// redirect an existing workload's agent credentials/server config to a pool the admin
// controls, without the workload owner's knowledge or consent:
//  1. The pod's own lightrun.com/inject-java annotation (new-style), per "DESIGN CHANGE v3"
//     section 1 of .dot-agent-deck/webhook-refactor-context.md:
//     - "" or "false" -> explicit opt-out, no injection.
//     - "true" -> inject using the pool literally named "default" (AgentPool first, then
//     ClusterAgentPool).
//     - "<name>" -> inject using the pool named <name>, kind per lightrun.com/agent-pool-kind
//     (default AgentPool).
//  2. The pod's own legacy-style trigger (pre-v3, 2026-07-14 "DESIGN CHANGE"):
//     lightrun.com/agent-type present (non-empty) is the trigger, and lightrun.com/agent-pool
//     (always a namespaced AgentPool) names the pool -- required once triggered, hence the
//     error below rather than falling through to the namespace default.
//  3. The namespace-level lightrun.com/inject-java default (a live read of the pod's own
//     Namespace object), applying the same "" /"false"/"true"/"<name>" rules as (1). Only
//     consulted when the pod itself sets NEITHER trigger (1) nor (2).
//  4. No injection.
//
// lightrun.com/agent-type is also an optional override of *which* agent type to dispatch to
// when (1) or (3) resolves via inject-java (default "java"); an unrecognized value means no
// injection, even if inject-java names a valid pool.
func decideInjection(ctx context.Context, reader client.Reader, pod *corev1.Pod) (*injectDecision, error) {
	if injectVal, hasInject := pod.Annotations[annotationInjectJava]; hasInject {
		return decisionFromInjectValue(pod, injectVal), nil
	}

	if legacyAgentType := pod.Annotations[annotationAgentType]; legacyAgentType != "" {
		return legacyDecision(pod, legacyAgentType)
	}

	nsVal, nsHas, err := namespaceInjectJavaDefault(ctx, reader, pod.Namespace)
	if err != nil {
		return nil, err
	}
	if nsHas {
		return decisionFromInjectValue(pod, nsVal), nil
	}

	return nil, nil
}

// decisionFromInjectValue interprets injectVal (the resolved lightrun.com/inject-java value,
// from either the pod itself or its namespace's default) per "DESIGN CHANGE v3" section 1.
func decisionFromInjectValue(pod *corev1.Pod, injectVal string) *injectDecision {
	if injectVal == "" || injectVal == "false" {
		return nil
	}

	agentType := pod.Annotations[annotationAgentType]
	if agentType == "" {
		agentType = "java"
	}
	if _, known := agentMutators[agentType]; !known {
		return nil
	}

	if injectVal == "true" {
		return &injectDecision{agentType: agentType, poolName: defaultPoolName, tryBothKinds: true}
	}

	kind := pod.Annotations[annotationAgentPoolKind]
	if kind == "" {
		kind = poolKindAgentPool
	}
	return &injectDecision{agentType: agentType, poolKind: kind, poolName: injectVal}
}

// legacyDecision implements the pre-v3 (2026-07-14 "DESIGN CHANGE") trigger contract: pod's own
// legacyAgentType (lightrun.com/agent-type, guaranteed non-empty by the caller) is the trigger,
// and lightrun.com/agent-pool (always a namespaced AgentPool) names the pool.
func legacyDecision(pod *corev1.Pod, legacyAgentType string) (*injectDecision, error) {
	if _, known := agentMutators[legacyAgentType]; !known {
		return nil, nil
	}
	poolName := pod.Annotations[annotationAgentPool]
	if poolName == "" {
		return nil, fmt.Errorf("missing required annotation %q", annotationAgentPool)
	}
	return &injectDecision{agentType: legacyAgentType, poolKind: poolKindAgentPool, poolName: poolName}, nil
}

// namespaceInjectJavaDefault does a live API read of pod's own Namespace object to resolve a
// namespace-level lightrun.com/inject-java default, per "DESIGN CHANGE v3" section 1.
func namespaceInjectJavaDefault(ctx context.Context, reader client.Reader, namespace string) (string, bool, error) {
	var ns corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return "", false, fmt.Errorf("reading namespace %q: %w", namespace, err)
	}
	val, ok := ns.Annotations[annotationInjectJava]
	return val, ok, nil
}

// resolvedConfig holds the fully-merged (pool defaults + per-field pod annotation overrides)
// configuration used to actually inject the agent, per "DESIGN CHANGE v3" section 2. secretName
// is always a Secret name in the pod's own namespace: for an AgentPool that's its own
// secretRef.name; for a ClusterAgentPool it's the LOCAL mirrored Secret's deterministic name
// (see api/v1beta.ClusterAgentPoolMirrorSecretName), never the cross-namespace source Secret.
type resolvedConfig struct {
	secretName               string
	serverHostname           string
	containerSelector        []string
	agentEnvVarName          string
	agentCliFlags            string
	agentTags                []string
	agentName                string
	agentConfig              map[string]string
	useSecretsAsMountedFiles *bool
}

// resolvePoolConfig resolves decision's pool reference (an AgentPool in podNamespace, or a
// ClusterAgentPool subject to its allowedNamespaces gate) into its shared-default
// resolvedConfig, per "DESIGN CHANGE v3" section 3.
func resolvePoolConfig(ctx context.Context, reader client.Reader, podNamespace string, decision *injectDecision) (resolvedConfig, error) {
	if decision.tryBothKinds {
		pool, err := getAgentPool(ctx, reader, podNamespace, decision.poolName)
		if err == nil {
			return agentPoolConfig(pool)
		}
		if !apierrors.IsNotFound(err) {
			return resolvedConfig{}, fmt.Errorf("resolving AgentPool %q in namespace %q: %w", decision.poolName, podNamespace, err)
		}

		clusterPool, clusterErr := getClusterAgentPool(ctx, reader, decision.poolName)
		if clusterErr == nil {
			return clusterAgentPoolConfig(clusterPool, podNamespace)
		}
		if !apierrors.IsNotFound(clusterErr) {
			return resolvedConfig{}, fmt.Errorf("resolving ClusterAgentPool %q: %w", decision.poolName, clusterErr)
		}

		return resolvedConfig{}, fmt.Errorf("no AgentPool or ClusterAgentPool named %q found (checked namespace %q and cluster scope)", decision.poolName, podNamespace)
	}

	if decision.poolKind == poolKindClusterAgentPool {
		clusterPool, err := getClusterAgentPool(ctx, reader, decision.poolName)
		if err != nil {
			return resolvedConfig{}, fmt.Errorf("ClusterAgentPool %q not found: %w", decision.poolName, err)
		}
		return clusterAgentPoolConfig(clusterPool, podNamespace)
	}

	pool, err := getAgentPool(ctx, reader, podNamespace, decision.poolName)
	if err != nil {
		return resolvedConfig{}, fmt.Errorf("AgentPool %q not found in namespace %q: %w", decision.poolName, podNamespace, err)
	}
	return agentPoolConfig(pool)
}

func getAgentPool(ctx context.Context, reader client.Reader, namespace, name string) (*agentsv1beta.AgentPool, error) {
	var pool agentsv1beta.AgentPool
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pool); err != nil {
		return nil, err
	}
	return &pool, nil
}

func getClusterAgentPool(ctx context.Context, reader client.Reader, name string) (*agentsv1beta.ClusterAgentPool, error) {
	var pool agentsv1beta.ClusterAgentPool
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, &pool); err != nil {
		return nil, err
	}
	return &pool, nil
}

func agentPoolConfig(pool *agentsv1beta.AgentPool) (resolvedConfig, error) {
	if pool.Spec.SecretRef.Name == "" {
		return resolvedConfig{}, fmt.Errorf("AgentPool %q has no secretRef.name configured", pool.Name)
	}
	return resolvedConfig{
		secretName:               pool.Spec.SecretRef.Name,
		serverHostname:           pool.Spec.ServerHostname,
		containerSelector:        pool.Spec.ContainerSelector,
		agentEnvVarName:          pool.Spec.AgentEnvVarName,
		agentCliFlags:            pool.Spec.AgentCliFlags,
		agentTags:                pool.Spec.AgentTags,
		agentName:                pool.Spec.AgentName,
		agentConfig:              pool.Spec.AgentConfig,
		useSecretsAsMountedFiles: pool.Spec.UseSecretsAsMountedFiles,
	}, nil
}

// clusterAgentPoolConfig resolves pool's shared defaults, gating on podNamespace being listed
// in pool.Spec.AllowedNamespaces (the "multi-tenant safety gate" of "DESIGN CHANGE v3" section
// 3) and pointing secretName at the LOCAL mirrored Secret the SecretMirrorReconciler
// (internal/controller/clusteragentpool) maintains in that namespace -- never the
// cross-namespace source Secret in pool.Spec.SecretRef.
func clusterAgentPoolConfig(pool *agentsv1beta.ClusterAgentPool, podNamespace string) (resolvedConfig, error) {
	allowed := false
	for _, ns := range pool.Spec.AllowedNamespaces {
		if ns == podNamespace {
			allowed = true
			break
		}
	}
	if !allowed {
		return resolvedConfig{}, fmt.Errorf("namespace %q is not permitted to use ClusterAgentPool %q (not in allowedNamespaces)", podNamespace, pool.Name)
	}
	if pool.Spec.SecretRef.Name == "" {
		return resolvedConfig{}, fmt.Errorf("ClusterAgentPool %q has no secretRef.name configured", pool.Name)
	}
	return resolvedConfig{
		secretName:               agentsv1beta.ClusterAgentPoolMirrorSecretName(pool.Name),
		serverHostname:           pool.Spec.ServerHostname,
		containerSelector:        pool.Spec.ContainerSelector,
		agentEnvVarName:          pool.Spec.AgentEnvVarName,
		agentCliFlags:            pool.Spec.AgentCliFlags,
		agentTags:                pool.Spec.AgentTags,
		agentName:                pool.Spec.AgentName,
		agentConfig:              pool.Spec.AgentConfig,
		useSecretsAsMountedFiles: pool.Spec.UseSecretsAsMountedFiles,
	}, nil
}

// mergeWithPodOverrides applies pod's per-field lightrun.com/* override annotations on top of
// base (the resolved pool's shared defaults), per "DESIGN CHANGE v3" section 2: each field is
// overridden independently when the pod's corresponding annotation is present, and otherwise
// falls through to the pool's value.
func mergeWithPodOverrides(base resolvedConfig, ann map[string]string) (resolvedConfig, error) {
	merged := base
	if v := ann[annotationContainerSel]; v != "" {
		merged.containerSelector = splitAndTrim(v)
	}
	if v := ann[annotationAgentEnvVarName]; v != "" {
		merged.agentEnvVarName = v
	}
	if v := ann[annotationAgentCliFlags]; v != "" {
		merged.agentCliFlags = v
	}
	if v := ann[annotationAgentTags]; v != "" {
		merged.agentTags = splitAndTrim(v)
	}
	if v := ann[annotationAgentName]; v != "" {
		merged.agentName = v
	}
	if v := ann[annotationAgentConfig]; v != "" {
		cfg := map[string]string{}
		if err := json.Unmarshal([]byte(v), &cfg); err != nil {
			return resolvedConfig{}, fmt.Errorf("invalid %s (must be a JSON object of strings): %w", annotationAgentConfig, err)
		}
		merged.agentConfig = cfg
	}
	if v := ann[annotationUseMountedFiles]; v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return resolvedConfig{}, fmt.Errorf("invalid %s (must be a bool): %w", annotationUseMountedFiles, err)
		}
		merged.useSecretsAsMountedFiles = &parsed
	}
	return merged, nil
}

// mutateJavaPod implements the "java" branch of the agent-type dispatch. It mirrors the
// injection mechanics of internal/controller/patch_funcs.go's addInitContainer/addVolume/
// patchAppContainers/patchJavaToolEnv, adapted to mutate an incoming admission.Pod directly
// instead of SSA-patching an existing Deployment/StatefulSet, resolving its configuration from
// decision's pool (merged with any per-field pod annotation overrides) instead of a flat
// annotation set.
func mutateJavaPod(ctx context.Context, reader client.Reader, writer client.Client, pod *corev1.Pod, cfg Config, decision *injectDecision) error {
	poolCfg, err := resolvePoolConfig(ctx, reader, pod.Namespace, decision)
	if err != nil {
		return err
	}
	resolved, err := mergeWithPodOverrides(poolCfg, pod.Annotations)
	if err != nil {
		return err
	}

	containerNames := resolved.containerSelector
	if len(containerNames) == 0 {
		return fmt.Errorf("unable to find matching container to patch: %q resolves to zero container names", annotationContainerSel)
	}

	var missing []string
	for _, name := range containerNames {
		if findContainer(pod.Spec.Containers, name) == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("unable to find container(s) %s in pod spec to patch", strings.Join(missing, ", "))
	}

	agentArg, err := buildAgentPathArg(cfg.SharedVolumeMountPath, resolved.agentCliFlags)
	if err != nil {
		return err
	}

	useMountedFiles := resolveUseMountedFiles(resolved.useSecretsAsMountedFiles)

	cmData, err := buildAgentConfigMapData(configMapAnnotations(resolved))
	if err != nil {
		return err
	}
	cmName := agentConfigMapName(cmData)
	if err := ensureAgentConfigMap(ctx, writer, pod.Namespace, cmName, cmData); err != nil {
		return err
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: cfg.SharedVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	})

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: cmVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				Items: []corev1.KeyToPath{
					{Key: cmDataKeyConfig, Path: cmItemPathConfig},
					{Key: cmDataKeyMetadata, Path: cmItemPathMetadata},
				},
			},
		},
	})

	if useMountedFiles {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: lightrunSecretVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: resolved.secretName,
					Items: []corev1.KeyToPath{
						{Key: lightrunSecretKeyLightrunKey, Path: lightrunSecretKeyLightrunKey},
						{Key: lightrunSecretKeyPinnedCertHash, Path: lightrunSecretKeyPinnedCertHash},
					},
					DefaultMode: int32Ptr(0440),
				},
			},
		})
	}

	pod.Spec.InitContainers = append(pod.Spec.InitContainers, buildInitContainer(cfg, resolved, useMountedFiles))

	for _, name := range containerNames {
		container := findContainer(pod.Spec.Containers, name)
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      cfg.SharedVolumeName,
			MountPath: cfg.SharedVolumeMountPath,
		})
		if err := patchContainerEnvVar(container, resolved.agentEnvVarName, agentArg); err != nil {
			return err
		}
	}

	return nil
}

// resolveUseMountedFiles mirrors the old CRD field's kubebuilder default of true: absent on
// both the pod annotation and the resolved pool (nil) defaults to true (mount the secret as a
// volume), matching today's out-of-the-box behavior.
func resolveUseMountedFiles(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

// configMapAnnotations projects resolved's tags/name/config fields back into the
// lightrun.com/agent-tags, lightrun.com/agent-name and lightrun.com/agent-config annotation
// shape buildAgentConfigMapData expects, so the ConfigMap's agent.config/agent.metadata.json
// content reflects the fully-resolved (pool defaults + pod overrides) values even when they
// came entirely from the pool rather than a pod annotation.
func configMapAnnotations(resolved resolvedConfig) map[string]string {
	ann := map[string]string{}
	if resolved.agentName != "" {
		ann[annotationAgentName] = resolved.agentName
	}
	if len(resolved.agentTags) > 0 {
		ann[annotationAgentTags] = strings.Join(resolved.agentTags, ",")
	}
	if len(resolved.agentConfig) > 0 {
		b, _ := json.Marshal(resolved.agentConfig) // map[string]string always marshals cleanly
		ann[annotationAgentConfig] = string(b)
	}
	return ann
}

// buildInitContainer builds the lightrun-installer init container that stages the agent
// binary into the shared volume, mirroring
// internal/controller/patch_funcs.go's addInitContainer.
func buildInitContainer(cfg Config, resolved resolvedConfig, useMountedFiles bool) corev1.Container {
	volumeMounts := []corev1.VolumeMount{
		{Name: cfg.SharedVolumeName, MountPath: "/tmp/"},
		{Name: cmVolumeName, MountPath: cmMountPath},
	}

	envVars := []corev1.EnvVar{
		{Name: "LIGHTRUN_SERVER", Value: resolved.serverHostname},
	}

	if useMountedFiles {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      lightrunSecretVolumeName,
			MountPath: "/etc/lightrun/secret",
			ReadOnly:  true,
		})
	} else {
		envVars = append(envVars,
			corev1.EnvVar{
				Name: "LIGHTRUN_KEY",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: resolved.secretName},
						Key:                  lightrunSecretKeyLightrunKey,
					},
				},
			},
			corev1.EnvVar{
				Name: "PINNED_CERT",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: resolved.secretName},
						Key:                  lightrunSecretKeyPinnedCertHash,
					},
				},
			},
		)
	}

	if len(resolved.agentTags) > 0 {
		envVars = append(envVars, corev1.EnvVar{Name: "LIGHTRUN_AGENT_TAGS", Value: strings.Join(resolved.agentTags, ",")})
	}
	if resolved.agentName != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "LIGHTRUN_AGENT_NAME", Value: resolved.agentName})
	}
	if len(resolved.agentConfig) > 0 {
		b, _ := json.Marshal(resolved.agentConfig) // map[string]string always marshals cleanly
		envVars = append(envVars, corev1.EnvVar{Name: "LIGHTRUN_AGENT_CONFIG", Value: string(b)})
	}

	container := corev1.Container{
		Name:         lightrunInitContainerName,
		Image:        cfg.InitContainerImage,
		VolumeMounts: volumeMounts,
		Env:          envVars,
		SecurityContext: &corev1.SecurityContext{
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			RunAsNonRoot:             boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.BinarySI),
				corev1.ResourceMemory: *resource.NewScaledQuantity(64, resource.Scale(6)),
			},
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(50, resource.BinarySI),
				corev1.ResourceMemory: *resource.NewScaledQuantity(64, resource.Scale(6)),
			},
		},
	}
	if cfg.InitContainerImagePullPolicy != "" {
		container.ImagePullPolicy = cfg.InitContainerImagePullPolicy
	}
	return container
}

// agentMetadataTag and agentMetadata mirror internal/controller/agent_config.go's Tag /
// AgentMetadata structs -- the real init container's entrypoint reads this exact JSON shape
// from agent.metadata.json.
type agentMetadataTag struct {
	Name string `json:"name"`
}

type agentMetadataRegistration struct {
	DisplayName string             `json:"displayName"`
	Tags        []agentMetadataTag `json:"tags"`
}

type agentMetadata struct {
	Registration agentMetadataRegistration `json:"registration"`
}

// buildAgentConfigMapData builds the ConfigMap Data that internal/controller/agent_config.go's
// createAgentConfig used to build: a "config" key (agent.config -- sorted "key=value\n" lines
// parsed from the lightrun.com/agent-config JSON annotation, mirroring
// internal/controller/helpers.go's parseAgentConfig) and a "metadata" key (agent.metadata.json
// -- agent-tags/agent-name JSON-encoded, mirroring populateTags). The real
// lightruncom/k8s-operator-init-java-agent-* init container image's entrypoint reads both
// files via awk at cmMountPath; without this ConfigMap it crashes on startup.
//
// ann need not be the Pod's own annotations verbatim -- mutateJavaPod passes in the
// fully-resolved (pool defaults + pod overrides) tags/name/config projected back into this same
// annotation shape via configMapAnnotations, so the ConfigMap content is correct even when
// those values came entirely from the resolved pool.
func buildAgentConfigMapData(ann map[string]string) (map[string]string, error) {
	agentConfig := map[string]string{}
	if raw := ann[annotationAgentConfig]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &agentConfig); err != nil {
			return nil, fmt.Errorf("invalid %s (must be a JSON object of strings): %w", annotationAgentConfig, err)
		}
	}

	keys := make([]string, 0, len(agentConfig))
	for k := range agentConfig {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var configContent strings.Builder
	for _, k := range keys {
		configContent.WriteString(k)
		configContent.WriteByte('=')
		configContent.WriteString(agentConfig[k])
		configContent.WriteByte('\n')
	}

	tags := make([]agentMetadataTag, 0)
	for _, t := range splitAndTrim(ann[annotationAgentTags]) {
		tags = append(tags, agentMetadataTag{Name: t})
	}
	metadataJSON, err := json.Marshal(agentMetadata{Registration: agentMetadataRegistration{
		DisplayName: ann[annotationAgentName],
		Tags:        tags,
	}})
	if err != nil {
		return nil, fmt.Errorf("marshaling agent metadata: %w", err)
	}

	return map[string]string{
		cmDataKeyConfig:   configContent.String(),
		cmDataKeyMetadata: string(metadataJSON),
	}, nil
}

// agentConfigMapName derives a deterministic ConfigMap name from a hash of data's content,
// mirroring the spirit of internal/controller/patch_funcs.go's configMapDataHash. The webhook
// mutates a Pod before it's persisted (no Pod UID exists yet for an owner reference, and there
// is no reconciling controller to track/clean up a per-pod ConfigMap), so a content-derived
// name is used instead: pods with identical config content naturally converge on the same
// ConfigMap (dedup, no unbounded growth per pod), and ensureAgentConfigMap below treats
// "already exists" under this name as "already correct" rather than re-writing it.
func agentConfigMapName(data map[string]string) string {
	h := fnv.New64a()
	h.Write([]byte(data[cmDataKeyConfig]))
	h.Write([]byte{0})
	h.Write([]byte(data[cmDataKeyMetadata]))
	return fmt.Sprintf("%s%x", cmNamePrefix, h.Sum64())
}

// ensureAgentConfigMap creates the ConfigMap named name in namespace with the given data if
// it doesn't already exist. Because name is derived from data's content (see
// agentConfigMapName), an AlreadyExists error means a prior pod already created the identical
// ConfigMap -- nothing to update.
func ensureAgentConfigMap(ctx context.Context, writer client.Client, namespace, name string, data map[string]string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
	}
	if err := writer.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ConfigMap %q in namespace %q: %w", name, namespace, err)
	}
	return nil
}

// patchContainerEnvVar appends agentArg to targetEnvVar on container, setting it if absent,
// or appending (space-separated) if already set -- mirroring
// internal/controller/patch_funcs.go's patchJavaToolEnv, minus the annotation-tracked
// idempotent-rollback bookkeeping that only matters for a reconciling controller. Denies with
// an error if the combined value (pre-existing value + appended agentArg) would exceed 1024
// chars, the same hard Java limitation patchJavaToolEnv enforced on the combined value, not
// just the newly appended argument in isolation.
func patchContainerEnvVar(container *corev1.Container, targetEnvVar string, agentArg string) error {
	for i := range container.Env {
		if container.Env[i].Name != targetEnvVar {
			continue
		}
		if strings.Contains(container.Env[i].Value, agentArg) {
			return nil
		}
		newValue := agentArg
		if container.Env[i].Value != "" {
			newValue = container.Env[i].Value + " " + agentArg
		}
		if len(newValue) > 1024 {
			return fmt.Errorf("%s has more than 1024 chars. This is a limitation of Java", targetEnvVar)
		}
		container.Env[i].Value = newValue
		return nil
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: targetEnvVar, Value: agentArg})
	return nil
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func boolPtr(b bool) *bool { return &b }

func int32Ptr(i int32) *int32 { return &i }
