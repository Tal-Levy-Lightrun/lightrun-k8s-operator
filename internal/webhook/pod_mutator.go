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
// for the full design contract.
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

// Annotation keys under the lightrun.com/ prefix, per the annotation-driven contract in
// .dot-agent-deck/webhook-refactor-context.md, as amended by that doc's "DESIGN CHANGE"
// section: lightrun.com/secret-name + lightrun.com/server-hostname are replaced by
// lightrun.com/agent-pool, which the webhook resolves via a live API read of an AgentPool CR
// in the pod's own namespace.
const (
	annotationAgentType       = "lightrun.com/agent-type"
	annotationAgentPool       = "lightrun.com/agent-pool"
	annotationContainerSel    = "lightrun.com/container-selector"
	annotationAgentEnvVarName = "lightrun.com/agent-env-var-name"
	annotationAgentCliFlags   = "lightrun.com/agent-cli-flags"
	annotationAgentTags       = "lightrun.com/agent-tags"
	annotationAgentName       = "lightrun.com/agent-name"
	annotationAgentConfig     = "lightrun.com/agent-config"
	annotationUseMountedFiles = "lightrun.com/use-secrets-as-mounted-files"
)

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

// agentMutator mutates pod in place for a single, recognized lightrun.com/agent-type value.
// Returning an error denies the admission request (see podMutator.Default). reader is used
// for the live API read that resolves an AgentPool by name; writer is used to create the
// ConfigMap the init container reads its config from.
type agentMutator func(ctx context.Context, reader client.Reader, writer client.Client, pod *corev1.Pod, cfg Config, ann map[string]string) error

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
// create. Pods without a recognized lightrun.com/agent-type annotation are admitted
// unchanged; recognized types are mutated in place, and a non-nil error here denies the
// admission request (surfaced to the caller of e.g. `kubectl create`).
func (m *podMutator) Default(ctx context.Context, obj runtime.Object) error {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("expected a Pod but got a %T", obj)
	}

	agentType := pod.Annotations[annotationAgentType]
	if agentType == "" {
		return nil
	}

	mutate, known := agentMutators[agentType]
	if !known {
		return nil
	}

	return mutate(ctx, m.reader, m.writer, pod, m.cfg, pod.Annotations)
}

// SetupWebhookWithManager wires the Pod mutating webhook onto mgr's webhook server, the same
// way `kubebuilder create webhook --group core --version v1 --kind Pod --defaulting` would
// scaffold it.

//+kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,admissionReviewVersions=v1,groups="",resources=pods,verbs=create,versions=v1,name=mpod.lightrun.com
//+kubebuilder:rbac:groups=agents.lightrun.com,resources=agentpools,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update

func SetupWebhookWithManager(mgr ctrl.Manager, cfg Config) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&corev1.Pod{}).
		WithDefaulter(&podMutator{cfg: cfg, reader: mgr.GetAPIReader(), writer: mgr.GetClient()}).
		Complete()
}

// resolvedPool holds the credential/server config resolved from an AgentPool CR, per the
// "DESIGN CHANGE" section of .dot-agent-deck/webhook-refactor-context.md.
type resolvedPool struct {
	secretName     string
	serverHostname string
}

// resolveAgentPool does a live API read (via reader) of the AgentPool named poolName in
// podNamespace.
func resolveAgentPool(ctx context.Context, reader client.Reader, podNamespace, poolName string) (resolvedPool, error) {
	var pool agentsv1beta.AgentPool
	if err := reader.Get(ctx, client.ObjectKey{Namespace: podNamespace, Name: poolName}, &pool); err != nil {
		return resolvedPool{}, fmt.Errorf("AgentPool %q not found in namespace %q: %w", poolName, podNamespace, err)
	}
	if pool.Spec.SecretRef.Name == "" {
		return resolvedPool{}, fmt.Errorf("AgentPool %q has no secretRef.name configured", poolName)
	}
	return resolvedPool{secretName: pool.Spec.SecretRef.Name, serverHostname: pool.Spec.ServerHostname}, nil
}

// mutateJavaPod implements the "java" branch of the agent-type dispatch. It mirrors the
// injection mechanics of internal/controller/patch_funcs.go's addInitContainer/addVolume/
// patchAppContainers/patchJavaToolEnv, adapted to mutate an incoming admission.Pod directly
// instead of SSA-patching an existing Deployment/StatefulSet.
func mutateJavaPod(ctx context.Context, reader client.Reader, writer client.Client, pod *corev1.Pod, cfg Config, ann map[string]string) error {
	required := []string{
		annotationAgentPool,
		annotationContainerSel,
		annotationAgentEnvVarName,
	}
	for _, key := range required {
		if ann[key] == "" {
			return fmt.Errorf("missing required annotation %q", key)
		}
	}

	pool, err := resolveAgentPool(ctx, reader, pod.Namespace, ann[annotationAgentPool])
	if err != nil {
		return err
	}

	containerNames := splitAndTrim(ann[annotationContainerSel])
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

	agentArg, err := buildAgentPathArg(cfg.SharedVolumeMountPath, ann[annotationAgentCliFlags])
	if err != nil {
		return err
	}

	useMountedFiles := parseUseMountedFiles(ann[annotationUseMountedFiles])

	cmData, err := buildAgentConfigMapData(ann)
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
					SecretName: pool.secretName,
					Items: []corev1.KeyToPath{
						{Key: lightrunSecretKeyLightrunKey, Path: lightrunSecretKeyLightrunKey},
						{Key: lightrunSecretKeyPinnedCertHash, Path: lightrunSecretKeyPinnedCertHash},
					},
					DefaultMode: int32Ptr(0440),
				},
			},
		})
	}

	pod.Spec.InitContainers = append(pod.Spec.InitContainers, buildInitContainer(cfg, ann, pool, useMountedFiles))

	for _, name := range containerNames {
		container := findContainer(pod.Spec.Containers, name)
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:      cfg.SharedVolumeName,
			MountPath: cfg.SharedVolumeMountPath,
		})
		if err := patchContainerEnvVar(container, ann[annotationAgentEnvVarName], agentArg); err != nil {
			return err
		}
	}

	return nil
}

// buildInitContainer builds the lightrun-installer init container that stages the agent
// binary into the shared volume, mirroring
// internal/controller/patch_funcs.go's addInitContainer.
func buildInitContainer(cfg Config, ann map[string]string, pool resolvedPool, useMountedFiles bool) corev1.Container {
	volumeMounts := []corev1.VolumeMount{
		{Name: cfg.SharedVolumeName, MountPath: "/tmp/"},
		{Name: cmVolumeName, MountPath: cmMountPath},
	}

	envVars := []corev1.EnvVar{
		{Name: "LIGHTRUN_SERVER", Value: pool.serverHostname},
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
						LocalObjectReference: corev1.LocalObjectReference{Name: pool.secretName},
						Key:                  lightrunSecretKeyLightrunKey,
					},
				},
			},
			corev1.EnvVar{
				Name: "PINNED_CERT",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: pool.secretName},
						Key:                  lightrunSecretKeyPinnedCertHash,
					},
				},
			},
		)
	}

	if tags := ann[annotationAgentTags]; tags != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "LIGHTRUN_AGENT_TAGS", Value: tags})
	}
	if name := ann[annotationAgentName]; name != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "LIGHTRUN_AGENT_NAME", Value: name})
	}
	if config := ann[annotationAgentConfig]; config != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "LIGHTRUN_AGENT_CONFIG", Value: config})
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

// parseUseMountedFiles parses the lightrun.com/use-secrets-as-mounted-files annotation,
// mirroring the old CRD field's kubebuilder default of true: absent or unparseable values
// default to true (mount the secret as a volume), matching today's out-of-the-box behavior.
func parseUseMountedFiles(raw string) bool {
	if raw == "" {
		return true
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return true
	}
	return parsed
}

func boolPtr(b bool) *bool { return &b }

func int32Ptr(i int32) *int32 { return &i }
