package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	agentv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

// Node-specific identifiers. Deliberately distinct from Java's equivalents (annotationAgentName,
// cmVolumeName, annotationPatchedEnvName/-Value in patch_funcs.go) so a workload could in principle
// be targeted by one LightrunJavaAgent and one LightrunNodeAgent simultaneously without collisions.
// cmNamePrefix, initContainerName's shared constants (annotationConfigMapHash) are intentionally
// reused as-is from patch_funcs.go: the ConfigMap machinery and configmap-hash rollout trigger are
// generic, not language-specific.
const (
	nodeCmVolumeName              = "lightrunagent-config-node"
	nodeInitContainerName         = "lightrun-installer-node"
	nodeAnnotationPatchedEnvName  = "lightrun.com/patched-env-name-node"
	nodeAnnotationPatchedEnvValue = "lightrun.com/patched-env-value-node"
	nodeAnnotationAgentName       = "lightrun.com/lightrunnodeagent"
	cliFlagsEnvVarName            = "LIGHTRUN_AGENT_CLI_FLAGS"
)

// nodeAgentEnvVarArgument builds the NODE_OPTIONS --require argument. Unlike Java's
// agentEnvVarArgument, AgentCliFlags is never concatenated here (see patchAppContainersNode).
func nodeAgentEnvVarArgument(mountPath string) (string, error) {
	agentArg := "--require " + mountPath + "/agent/lightrun_agent_bootstrap.js"
	if len(agentArg) > 1024 {
		return "", errors.New("node agent --require argument has more than 1024 chars")
	}
	return agentArg, nil
}

func (r *LightrunNodeAgentReconciler) createAgentConfig(lightrunNodeAgent *agentv1beta.LightrunNodeAgent) (corev1.ConfigMap, error) {
	metadata := newAgentMetadata()
	populateTags(lightrunNodeAgent.Spec.AgentTags, lightrunNodeAgent.Spec.AgentName, &metadata)
	jsonString, err := json.Marshal(metadata)
	if err != nil {
		return corev1.ConfigMap{}, err
	}
	configMap := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1.SchemeGroupVersion.String(), Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      (cmNamePrefix + lightrunNodeAgent.Name),
			Namespace: lightrunNodeAgent.Namespace,
		},
		Data: map[string]string{
			"config":   parseAgentConfig(lightrunNodeAgent.Spec.AgentConfig),
			"metadata": string(jsonString),
		},
	}

	if err := ctrl.SetControllerReference(lightrunNodeAgent, &configMap, r.Scheme); err != nil {
		return configMap, err
	}
	return configMap, nil
}

func (r *LightrunNodeAgentReconciler) patchNodeDeployment(lightrunNodeAgent *agentv1beta.LightrunNodeAgent, secret *corev1.Secret, origDeployment *appsv1.Deployment, deploymentApplyConfig *appsv1ac.DeploymentApplyConfiguration, cmDataHash uint64) error {
	deploymentApplyConfig.WithSpec(
		appsv1ac.DeploymentSpec().WithTemplate(
			corev1ac.PodTemplateSpec().WithSpec(
				corev1ac.PodSpec(),
			).WithAnnotations(map[string]string{
				annotationConfigMapHash: fmt.Sprint(cmDataHash),
			}),
		),
	).WithAnnotations(map[string]string{
		nodeAnnotationAgentName: lightrunNodeAgent.Name,
	})
	r.addNodeVolume(deploymentApplyConfig, lightrunNodeAgent)
	r.addNodeInitContainer(deploymentApplyConfig, lightrunNodeAgent, secret)
	return r.patchNodeAppContainers(lightrunNodeAgent, origDeployment, deploymentApplyConfig)
}

func (r *LightrunNodeAgentReconciler) addNodeVolume(deploymentApplyConfig *appsv1ac.DeploymentApplyConfiguration, lightrunNodeAgent *agentv1beta.LightrunNodeAgent) {
	volumes := []*corev1ac.VolumeApplyConfiguration{
		corev1ac.Volume().
			WithName(lightrunNodeAgent.Spec.InitContainer.SharedVolumeName).
			WithEmptyDir(
				corev1ac.EmptyDirVolumeSource(),
			),
		corev1ac.Volume().
			WithName(nodeCmVolumeName).
			WithConfigMap(
				corev1ac.ConfigMapVolumeSource().
					WithName(cmNamePrefix+lightrunNodeAgent.Name).
					WithItems(
						corev1ac.KeyToPath().WithKey("config").WithPath("agent.config"),
						corev1ac.KeyToPath().WithKey("metadata").WithPath("agent.metadata.json"),
					),
			),
	}

	if lightrunNodeAgent.Spec.UseSecretsAsMountedFiles {
		volumes = append(volumes,
			corev1ac.Volume().WithName("lightrun-secret").
				WithSecret(corev1ac.SecretVolumeSource().
					WithSecretName(lightrunNodeAgent.Spec.SecretName).
					WithItems(
						corev1ac.KeyToPath().WithKey("lightrun_key").WithPath("lightrun_key"),
						corev1ac.KeyToPath().WithKey("pinned_cert_hash").WithPath("pinned_cert_hash"),
					).
					WithDefaultMode(0440)),
		)
	}

	deploymentApplyConfig.Spec.Template.Spec.WithVolumes(volumes...)
}

func (r *LightrunNodeAgentReconciler) addNodeInitContainer(deploymentApplyConfig *appsv1ac.DeploymentApplyConfiguration, lightrunNodeAgent *agentv1beta.LightrunNodeAgent, secret *corev1.Secret) {
	spec := lightrunNodeAgent.Spec
	isImagePullPolicyConfigured := spec.InitContainer.ImagePullPolicy != ""

	volumeMounts := []*corev1ac.VolumeMountApplyConfiguration{
		corev1ac.VolumeMount().WithName(spec.InitContainer.SharedVolumeName).WithMountPath("/tmp/"),
		corev1ac.VolumeMount().WithName(nodeCmVolumeName).WithMountPath("/tmp/cm/"),
	}
	if spec.UseSecretsAsMountedFiles {
		volumeMounts = append(volumeMounts,
			corev1ac.VolumeMount().WithName("lightrun-secret").WithMountPath("/etc/lightrun/secret").WithReadOnly(true),
		)
	}

	envVars := []*corev1ac.EnvVarApplyConfiguration{
		corev1ac.EnvVar().WithName("LIGHTRUN_SERVER").WithValue(spec.ServerHostname),
	}
	if !spec.UseSecretsAsMountedFiles {
		envVars = append(envVars,
			corev1ac.EnvVar().WithName("LIGHTRUN_KEY").WithValueFrom(
				corev1ac.EnvVarSource().WithSecretKeyRef(
					corev1ac.SecretKeySelector().WithName(secret.Name).WithKey("lightrun_key"),
				),
			),
			corev1ac.EnvVar().WithName("PINNED_CERT").WithValueFrom(
				corev1ac.EnvVarSource().WithSecretKeyRef(
					corev1ac.SecretKeySelector().WithName(secret.Name).WithKey("pinned_cert_hash"),
				),
			),
		)
	}

	initContainer := corev1ac.Container().
		WithName(nodeInitContainerName).
		WithImage(spec.InitContainer.Image).
		WithVolumeMounts(volumeMounts...).
		WithEnv(envVars...).
		WithSecurityContext(
			corev1ac.SecurityContext().
				WithCapabilities(
					corev1ac.Capabilities().WithDrop(corev1.Capability("ALL")),
				).
				WithRunAsNonRoot(true).
				WithAllowPrivilegeEscalation(false).
				WithSeccompProfile(
					corev1ac.SeccompProfile().
						WithType(corev1.SeccompProfileTypeRuntimeDefault),
				),
		).
		WithResources(
			corev1ac.ResourceRequirements().
				WithLimits(
					corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(50), resource.BinarySI),
						corev1.ResourceMemory: *resource.NewScaledQuantity(int64(64), resource.Scale(6)),
					},
				).WithRequests(
				corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(50), resource.BinarySI),
					corev1.ResourceMemory: *resource.NewScaledQuantity(int64(64), resource.Scale(6)),
				},
			),
		)
	if isImagePullPolicyConfigured {
		initContainer.WithImagePullPolicy(spec.InitContainer.ImagePullPolicy)
	}
	deploymentApplyConfig.Spec.Template.Spec.WithInitContainers(initContainer)
}

// patchNodeAppContainers SSA-patches the volume mount onto every selected container, and, when
// AgentCliFlags is set, LIGHTRUN_AGENT_CLI_FLAGS as a plain env var (never concatenated into
// NODE_OPTIONS). Because this env var is applied via SSA under nodeFieldManager (not the client-side
// merge patch used for NODE_OPTIONS), it is automatically pruned by the empty-apply unpatch step on
// deletion, same as the volume mount.
func (r *LightrunNodeAgentReconciler) patchNodeAppContainers(lightrunNodeAgent *agentv1beta.LightrunNodeAgent, origDeployment *appsv1.Deployment, deploymentApplyConfig *appsv1ac.DeploymentApplyConfiguration) error {
	var found bool
	for _, container := range origDeployment.Spec.Template.Spec.Containers {
		for _, targetContainer := range lightrunNodeAgent.Spec.ContainerSelector {
			if targetContainer == container.Name {
				found = true
				containerApplyConfig := corev1ac.Container().
					WithName(container.Name).
					WithImage(container.Image).
					WithVolumeMounts(
						corev1ac.VolumeMount().WithMountPath(lightrunNodeAgent.Spec.InitContainer.SharedVolumeMountPath).WithName(lightrunNodeAgent.Spec.InitContainer.SharedVolumeName),
					)
				if lightrunNodeAgent.Spec.AgentCliFlags != "" {
					containerApplyConfig.WithEnv(
						corev1ac.EnvVar().WithName(cliFlagsEnvVarName).WithValue(lightrunNodeAgent.Spec.AgentCliFlags),
					)
				}
				deploymentApplyConfig.Spec.Template.Spec.WithContainers(containerApplyConfig)
			}
		}
	}
	if !found {
		return errors.New("unable to find matching container to patch")
	}
	return nil
}

// Client side patch, as we can't update value from 2 sources
func (r *LightrunNodeAgentReconciler) patchNodeOptionsEnv(deplAnnotations map[string]string, container *corev1.Container, targetEnvVar string, agentArg string) error {
	patchedEnv := deplAnnotations[nodeAnnotationPatchedEnvName]
	patchedEnvValue := deplAnnotations[nodeAnnotationPatchedEnvValue]

	if patchedEnv != targetEnvVar || patchedEnvValue != agentArg {
		r.unpatchNodeOptionsEnv(deplAnnotations, container)
	}

	targetEnvVarIndex := findEnvVarIndex(targetEnvVar, container.Env)
	if targetEnvVarIndex == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  targetEnvVar,
			Value: agentArg,
		})
	} else {
		if !strings.Contains(container.Env[targetEnvVarIndex].Value, agentArg) {
			container.Env[targetEnvVarIndex].Value = container.Env[targetEnvVarIndex].Value + " " + agentArg
			if len(container.Env[targetEnvVarIndex].Value) > 1024 {
				return errors.New(targetEnvVar + " has more than 1024 chars")
			}
		}
	}
	return nil
}

func (r *LightrunNodeAgentReconciler) unpatchNodeOptionsEnv(deplAnnotations map[string]string, container *corev1.Container) {
	patchedEnv := deplAnnotations[nodeAnnotationPatchedEnvName]
	patchedEnvValue := deplAnnotations[nodeAnnotationPatchedEnvValue]
	if patchedEnv == "" && patchedEnvValue == "" {
		return
	}

	envVarIndex := findEnvVarIndex(patchedEnv, container.Env)
	if envVarIndex != -1 {
		value := strings.ReplaceAll(container.Env[envVarIndex].Value, patchedEnvValue, "")
		value = strings.TrimSpace(value)
		if value == "" {
			container.Env = slices.Delete(container.Env, envVarIndex, envVarIndex+1)
		} else {
			container.Env[envVarIndex].Value = value
		}
	}
}

func (r *LightrunNodeAgentReconciler) patchNodeStatefulSet(lightrunNodeAgent *agentv1beta.LightrunNodeAgent, secret *corev1.Secret, origStatefulSet *appsv1.StatefulSet, statefulSetApplyConfig *appsv1ac.StatefulSetApplyConfiguration, cmDataHash uint64) error {
	statefulSetApplyConfig.WithSpec(
		appsv1ac.StatefulSetSpec().WithTemplate(
			corev1ac.PodTemplateSpec().WithSpec(
				corev1ac.PodSpec(),
			).WithAnnotations(map[string]string{
				annotationConfigMapHash: fmt.Sprint(cmDataHash),
			}),
		),
	).WithAnnotations(map[string]string{
		nodeAnnotationAgentName: lightrunNodeAgent.Name,
	})

	r.addNodeVolumeToStatefulSet(statefulSetApplyConfig, lightrunNodeAgent)
	r.addNodeInitContainerToStatefulSet(statefulSetApplyConfig, lightrunNodeAgent, secret)
	return r.patchNodeStatefulSetAppContainers(lightrunNodeAgent, origStatefulSet, statefulSetApplyConfig)
}

func (r *LightrunNodeAgentReconciler) addNodeVolumeToStatefulSet(statefulSetApplyConfig *appsv1ac.StatefulSetApplyConfiguration, lightrunNodeAgent *agentv1beta.LightrunNodeAgent) {
	volumes := []*corev1ac.VolumeApplyConfiguration{
		corev1ac.Volume().
			WithName(lightrunNodeAgent.Spec.InitContainer.SharedVolumeName).
			WithEmptyDir(
				corev1ac.EmptyDirVolumeSource(),
			),
		corev1ac.Volume().
			WithName(nodeCmVolumeName).
			WithConfigMap(
				corev1ac.ConfigMapVolumeSource().
					WithName(cmNamePrefix+lightrunNodeAgent.Name).
					WithItems(
						corev1ac.KeyToPath().WithKey("config").WithPath("agent.config"),
						corev1ac.KeyToPath().WithKey("metadata").WithPath("agent.metadata.json"),
					),
			),
	}

	if lightrunNodeAgent.Spec.UseSecretsAsMountedFiles {
		volumes = append(volumes,
			corev1ac.Volume().WithName("lightrun-secret").
				WithSecret(corev1ac.SecretVolumeSource().
					WithSecretName(lightrunNodeAgent.Spec.SecretName).
					WithItems(
						corev1ac.KeyToPath().WithKey("lightrun_key").WithPath("lightrun_key"),
						corev1ac.KeyToPath().WithKey("pinned_cert_hash").WithPath("pinned_cert_hash"),
					).
					WithDefaultMode(0440)),
		)
	}

	statefulSetApplyConfig.Spec.Template.Spec.WithVolumes(volumes...)
}

func (r *LightrunNodeAgentReconciler) addNodeInitContainerToStatefulSet(statefulSetApplyConfig *appsv1ac.StatefulSetApplyConfiguration, lightrunNodeAgent *agentv1beta.LightrunNodeAgent, secret *corev1.Secret) {
	spec := lightrunNodeAgent.Spec
	isImagePullPolicyConfigured := spec.InitContainer.ImagePullPolicy != ""

	volumeMounts := []*corev1ac.VolumeMountApplyConfiguration{
		corev1ac.VolumeMount().WithName(spec.InitContainer.SharedVolumeName).WithMountPath("/tmp/"),
		corev1ac.VolumeMount().WithName(nodeCmVolumeName).WithMountPath("/tmp/cm/"),
	}
	if spec.UseSecretsAsMountedFiles {
		volumeMounts = append(volumeMounts,
			corev1ac.VolumeMount().WithName("lightrun-secret").WithMountPath("/etc/lightrun/secret").WithReadOnly(true),
		)
	}

	envVars := []*corev1ac.EnvVarApplyConfiguration{
		corev1ac.EnvVar().WithName("LIGHTRUN_SERVER").WithValue(spec.ServerHostname),
	}
	if !spec.UseSecretsAsMountedFiles {
		envVars = append(envVars,
			corev1ac.EnvVar().WithName("LIGHTRUN_KEY").WithValueFrom(
				corev1ac.EnvVarSource().WithSecretKeyRef(
					corev1ac.SecretKeySelector().WithName(secret.Name).WithKey("lightrun_key"),
				),
			),
			corev1ac.EnvVar().WithName("PINNED_CERT").WithValueFrom(
				corev1ac.EnvVarSource().WithSecretKeyRef(
					corev1ac.SecretKeySelector().WithName(secret.Name).WithKey("pinned_cert_hash"),
				),
			),
		)
	}

	initContainer := corev1ac.Container().
		WithName(nodeInitContainerName).
		WithImage(spec.InitContainer.Image).
		WithVolumeMounts(volumeMounts...).
		WithEnv(envVars...).
		WithSecurityContext(
			corev1ac.SecurityContext().
				WithCapabilities(
					corev1ac.Capabilities().WithDrop(corev1.Capability("ALL")),
				).
				WithRunAsNonRoot(true).
				WithAllowPrivilegeEscalation(false).
				WithSeccompProfile(
					corev1ac.SeccompProfile().
						WithType(corev1.SeccompProfileTypeRuntimeDefault),
				),
		).
		WithResources(
			corev1ac.ResourceRequirements().
				WithLimits(
					corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(50), resource.BinarySI),
						corev1.ResourceMemory: *resource.NewScaledQuantity(int64(64), resource.Scale(6)),
					},
				).WithRequests(
				corev1.ResourceList{
					corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(50), resource.BinarySI),
					corev1.ResourceMemory: *resource.NewScaledQuantity(int64(64), resource.Scale(6)),
				},
			),
		)
	if isImagePullPolicyConfigured {
		initContainer.WithImagePullPolicy(spec.InitContainer.ImagePullPolicy)
	}
	statefulSetApplyConfig.Spec.Template.Spec.WithInitContainers(initContainer)
}

func (r *LightrunNodeAgentReconciler) patchNodeStatefulSetAppContainers(lightrunNodeAgent *agentv1beta.LightrunNodeAgent, origStatefulSet *appsv1.StatefulSet, statefulSetApplyConfig *appsv1ac.StatefulSetApplyConfiguration) error {
	var found bool
	for _, container := range origStatefulSet.Spec.Template.Spec.Containers {
		for _, targetContainer := range lightrunNodeAgent.Spec.ContainerSelector {
			if targetContainer == container.Name {
				found = true
				containerApplyConfig := corev1ac.Container().
					WithName(container.Name).
					WithImage(container.Image).
					WithVolumeMounts(
						corev1ac.VolumeMount().WithMountPath(lightrunNodeAgent.Spec.InitContainer.SharedVolumeMountPath).WithName(lightrunNodeAgent.Spec.InitContainer.SharedVolumeName),
					)
				if lightrunNodeAgent.Spec.AgentCliFlags != "" {
					containerApplyConfig.WithEnv(
						corev1ac.EnvVar().WithName(cliFlagsEnvVarName).WithValue(lightrunNodeAgent.Spec.AgentCliFlags),
					)
				}
				statefulSetApplyConfig.Spec.Template.Spec.WithContainers(containerApplyConfig)
			}
		}
	}
	if !found {
		return errors.New("unable to find matching container to patch")
	}
	return nil
}
