package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NOTE: LightrunNodeAgent has no controller/reconciler wired up yet (RED phase of TDD).
// This spec exercises the same fixtures/flows as lightrunjavaagent_controller_test.go, adapted
// for Node.js. It is expected to FAIL until the coder implements:
//   - internal/controller/lightrunnodeagent_controller.go (LightrunNodeAgentReconciler)
//   - internal/controller/patch_funcs_node.go (patchNodeOptionsEnv/unpatchNodeOptionsEnv, ConfigMap/volume/init-container wiring)
//   - registration of the reconciler in suite_test.go's BeforeSuite (and cmd/main.go for the real binary)
//
// Annotation-key/volume-name choices below (nodeAnnotationAgentName, nodeCmVolumeName, nodeAnnotationPatchedEnvName/Value)
// are this test's proposed contract for the Node reconciler: distinct from Java's equivalents so that a workload could -
// in principle - be owned by one LightrunJavaAgent and one LightrunNodeAgent at once without annotation collisions. This
// wasn't spelled out in the design brief and should be confirmed with the coder/reviewer rather than assumed correct.
var _ = Describe("LightrunNodeAgent controller", func() {

	// Define utility constants for object names and testing timeouts/durations and intervals.
	const (
		lrnagent1Name   = "lrnagent"
		nodeDeployment  = "node-app-deployment"
		nodeStatefulset = "node-app-statefulset"
		nodeSecretName  = "node-agent-secret"
		nodeServer      = "example.lightrun.com"
		nodeAgentName   = "coolio-node-agent"
		nodeTimeout     = time.Second * 10
		nodeDuration    = time.Second * 10
		nodeInterval    = time.Millisecond * 250
		nodeWrongNS     = "wrong-namespace-node"
		nodeInitImage   = "lightruncom/lightrun-init-agent-node:latest"
		nodeInitVolume  = "lightrun-agent-init-node"
		nodeEnvVarName  = "NODE_OPTIONS"
		nodeMountPath   = "/lightrun"
		// Expected --require argument built from InitContainer.SharedVolumeMountPath, per the design brief's
		// bootstrap path convention: <mountPath>/agent/lightrun_agent_bootstrap.js
		defaultNodeAgentArg  = "--require " + nodeMountPath + "/agent/lightrun_agent_bootstrap.js"
		nodeAgentCliFlags    = "--some-node-flag=1"
		cliFlagsEnvVarName   = "LIGHTRUN_AGENT_CLI_FLAGS"
		nodeEnvNonEmptyValue = "--max-old-space-size=4096"

		// Proposed Node-specific package-level identifiers the reconciler/patch_funcs_node.go should produce.
		// Deliberately distinct from Java's annotationAgentName/cmVolumeName/annotationPatchedEnvName/-Value
		// (see comment above the Describe block).
		nodeAnnotationAgentName       = "lightrun.com/lightrunnodeagent"
		nodeCmVolumeName              = "lightrunagent-config-node"
		nodeAnnotationPatchedEnvName  = "lightrun.com/patched-env-name-node"
		nodeAnnotationPatchedEnvValue = "lightrun.com/patched-env-value-node"
	)
	var nodeContainerSelector = []string{"app", "app2"}
	var nodeAgentConfig map[string]string = map[string]string{
		"max_log_cpu_cost":        "2",
		"some_config":             "1",
		"some_other_config":       "2",
		"some_yet_another_config": "1",
	}
	var nodeAgentTags []string = []string{"new_tag", "prod"}
	var nodeSecretData map[string]string = map[string]string{
		"LIGHTRUN_KEY":     "some_key",
		"LIGHTRUN_COMPANY": "some_company",
	}

	var patchedNodeDepl appsv1.Deployment
	nodeDeplRequest := types.NamespacedName{
		Name:      nodeDeployment,
		Namespace: testNamespace,
	}

	var patchedNodeDepl2 appsv1.Deployment
	nodeDeplRequest2 := types.NamespacedName{
		Name:      nodeDeployment + "-2",
		Namespace: testNamespace,
	}

	var patchedNodeDepl3 appsv1.Deployment
	nodeDeplRequest3 := types.NamespacedName{
		Name:      nodeDeployment + "-3",
		Namespace: nodeWrongNS,
	}

	var patchedNodeDepl4 appsv1.Deployment
	nodeDeplRequest4 := types.NamespacedName{
		Name:      nodeDeployment + "-4",
		Namespace: testNamespace,
	}

	var nodeCm corev1.ConfigMap
	nodeCmRequest := types.NamespacedName{
		Name:      cmNamePrefix + lrnagent1Name,
		Namespace: testNamespace,
	}

	var lrnAgent agentsv1beta.LightrunNodeAgent
	lrnAgentRequest := types.NamespacedName{
		Name:      lrnagent1Name,
		Namespace: testNamespace,
	}

	var lrnAgent2 agentsv1beta.LightrunNodeAgent
	lrnAgentRequest2 := types.NamespacedName{
		Name:      "lrnagent2",
		Namespace: testNamespace,
	}

	var lrnAgent3 agentsv1beta.LightrunNodeAgent
	lrnAgentRequest3 := types.NamespacedName{
		Name:      "duplicate-node",
		Namespace: testNamespace,
	}

	var lrnAgent4 agentsv1beta.LightrunNodeAgent
	lrnAgentRequest4 := types.NamespacedName{
		Name:      "wrong-namespace-node",
		Namespace: nodeWrongNS,
	}

	var lrnAgent5 agentsv1beta.LightrunNodeAgent
	lrnAgentRequest5 := types.NamespacedName{
		Name:      "change-node-agent-arg",
		Namespace: testNamespace,
	}

	var patchedNodeSts appsv1.StatefulSet
	nodeStsRequest := types.NamespacedName{
		Name:      nodeStatefulset,
		Namespace: testNamespace,
	}

	var lrnAgentSts agentsv1beta.LightrunNodeAgent
	lrnAgentStsRequest := types.NamespacedName{
		Name:      "lrnagent-sts",
		Namespace: testNamespace,
	}

	ctx := context.Background()

	Context("When setting up the test environment for LightrunNodeAgent", func() {
		It("Should create a wrong Namespace for node tests", func() {
			By("Creating a Namespace")
			ns := corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nodeWrongNS,
				},
			}
			Expect(k8sClient.Create(ctx, &ns)).Should(Succeed())
		})

		It("Should create LightrunNodeAgent custom resources", func() {
			By("Creating a first LightrunNodeAgent resource")
			lrnAgent := agentsv1beta.LightrunNodeAgent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lrnagent1Name,
					Namespace: testNamespace,
				},
				Spec: agentsv1beta.LightrunNodeAgentSpec{
					WorkloadName:      nodeDeployment,
					WorkloadType:      agentsv1beta.WorkloadTypeDeployment,
					SecretName:        nodeSecretName,
					ServerHostname:    nodeServer,
					AgentName:         nodeAgentName,
					AgentTags:         nodeAgentTags,
					AgentConfig:       nodeAgentConfig,
					AgentCliFlags:     nodeAgentCliFlags,
					AgentEnvVarName:   nodeEnvVarName,
					ContainerSelector: nodeContainerSelector,
					InitContainer: agentsv1beta.InitContainer{
						Image:                 nodeInitImage,
						SharedVolumeName:      nodeInitVolume,
						SharedVolumeMountPath: nodeMountPath,
					},
				},
			}
			Expect(k8sClient.Create(ctx, &lrnAgent)).Should(Succeed())

			By("Creating a second LightrunNodeAgent resource")
			lrnAgent2 := agentsv1beta.LightrunNodeAgent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "lrnagent2",
					Namespace: testNamespace,
				},
				Spec: agentsv1beta.LightrunNodeAgentSpec{
					WorkloadName:      nodeDeployment + "-2",
					WorkloadType:      agentsv1beta.WorkloadTypeDeployment,
					SecretName:        nodeSecretName,
					ServerHostname:    nodeServer,
					AgentName:         nodeAgentName,
					AgentTags:         nodeAgentTags,
					AgentConfig:       nodeAgentConfig,
					AgentEnvVarName:   nodeEnvVarName,
					ContainerSelector: nodeContainerSelector,
					InitContainer: agentsv1beta.InitContainer{
						Image:                 nodeInitImage,
						SharedVolumeName:      nodeInitVolume,
						SharedVolumeMountPath: nodeMountPath,
					},
				},
			}
			Expect(k8sClient.Create(ctx, &lrnAgent2)).Should(Succeed())

			By("Creating a secret")
			secret := corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      nodeSecretName,
					Namespace: testNamespace,
				},
				StringData: nodeSecretData,
			}
			Expect(k8sClient.Create(ctx, &secret)).Should(Succeed())

			By("Creating a StatefulSet-targeting LightrunNodeAgent resource")
			lrnAgentSts := agentsv1beta.LightrunNodeAgent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "lrnagent-sts",
					Namespace: testNamespace,
				},
				Spec: agentsv1beta.LightrunNodeAgentSpec{
					WorkloadName:      nodeStatefulset,
					WorkloadType:      agentsv1beta.WorkloadTypeStatefulSet,
					SecretName:        nodeSecretName,
					ServerHostname:    nodeServer,
					AgentName:         nodeAgentName,
					AgentTags:         nodeAgentTags,
					AgentConfig:       nodeAgentConfig,
					AgentCliFlags:     nodeAgentCliFlags,
					AgentEnvVarName:   nodeEnvVarName,
					ContainerSelector: nodeContainerSelector,
					InitContainer: agentsv1beta.InitContainer{
						Image:                 nodeInitImage,
						SharedVolumeName:      nodeInitVolume,
						SharedVolumeMountPath: nodeMountPath,
					},
				},
			}
			Expect(k8sClient.Create(ctx, &lrnAgentSts)).Should(Succeed())
		})
	})

	It("Should create Node Deployment", func() {
		By("Creating deployment")
		ctx := context.Background()

		depl := appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodeDeployment,
				Namespace: testNamespace,
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "node-app"},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": "node-app"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "app",
								Image: "node:18-alpine",
							},
							{
								Name:  "app2",
								Image: "node:18-alpine",
								Env: []corev1.EnvVar{
									{
										Name:  nodeEnvVarName,
										Value: nodeEnvNonEmptyValue,
									},
								},
							},
							{
								Name:  "no-patch",
								Image: "node:18-alpine",
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, &depl)).Should(Succeed())
	})

	Context("When patching Node Deployment matched by CRD", func() {
		It("Should add init Container", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				return len(patchedNodeDepl.Spec.Template.Spec.InitContainers) != 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should patch NODE_OPTIONS of containers by appending --require idempotently", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				for _, container := range patchedNodeDepl.Spec.Template.Spec.Containers {
					for _, envVar := range container.Env {
						if envVar.Name == nodeEnvVarName {
							if container.Name == "app" {
								if envVar.Value != defaultNodeAgentArg {
									return false
								}
							} else if container.Name == "app2" {
								if envVar.Value != nodeEnvNonEmptyValue+" "+defaultNodeAgentArg {
									return false
								}
							}
						}
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should pass AgentCliFlags via LIGHTRUN_AGENT_CLI_FLAGS instead of concatenating into NODE_OPTIONS", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				for _, container := range patchedNodeDepl.Spec.Template.Spec.Containers {
					if container.Name != "app" {
						continue
					}
					foundCliFlagsVar := false
					for _, envVar := range container.Env {
						if envVar.Name == nodeEnvVarName && strings.Contains(envVar.Value, nodeAgentCliFlags) {
							// AgentCliFlags must NOT be concatenated into NODE_OPTIONS
							return false
						}
						if envVar.Name == cliFlagsEnvVarName && envVar.Value == nodeAgentCliFlags {
							foundCliFlagsVar = true
						}
					}
					return foundCliFlagsVar
				}
				return false
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should add VolumeMount to Containers", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				var flag int
				for _, container := range patchedNodeDepl.Spec.Template.Spec.Containers {
					flag = -1
					if container.Name != "no-patch" {
						for _, volume := range container.VolumeMounts {
							if volume.Name == nodeInitVolume {
								flag = 1
							}
						}
						if flag == -1 {
							return false
						}
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should not patch 3rd container that not mentioned in CRD", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				for _, container := range patchedNodeDepl.Spec.Template.Spec.Containers {
					if container.Name == "no-patch" {
						for _, envVar := range container.Env {
							if envVar.Name == nodeEnvVarName || envVar.Name == cliFlagsEnvVarName {
								return false
							}
						}
						for _, volume := range container.VolumeMounts {
							if volume.Name == nodeInitVolume {
								return false
							}
						}
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should add volumes to the deployment", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				var desiredVolumes int = 0
				for _, volume := range patchedNodeDepl.Spec.Template.Spec.Volumes {
					if volume.Name == nodeInitVolume || volume.Name == nodeCmVolumeName {
						desiredVolumes += 1
					}
				}
				return desiredVolumes == 2
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should create config map", func() {
			Expect(k8sClient.Get(ctx, nodeCmRequest, &nodeCm)).Should(Succeed())
		})

		It("Should add ownership annotation and configmap-hash annotation to deployment", func() {
			Eventually(func() bool {
				flag := 0
				for k, v := range patchedNodeDepl.ObjectMeta.Annotations {
					if k == nodeAnnotationAgentName && v == lrnagent1Name {
						flag += 1
					}
				}
				for k := range patchedNodeDepl.Spec.Template.Annotations {
					if k == annotationConfigMapHash {
						flag += 1
					}
				}
				return flag == 2
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should match hash of the configmap in the deployment metadata (forces rollout on config change)", func() {
			Eventually(func() bool {
				expectedHash := configMapDataHash(nodeCm.Data)
				return patchedNodeDepl.Spec.Template.Annotations[annotationConfigMapHash] == fmt.Sprint(expectedHash)
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should add finalizer to first CRD", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest, &lrnAgent); err != nil {
					return false
				}
				return len(lrnAgent.ObjectMeta.Finalizers) != 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should not add finalizer to second CRD (workload not created yet)", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest2, &lrnAgent2); err != nil {
					return false
				}
				return len(lrnAgent2.ObjectMeta.Finalizers) == 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})
	})

	Context("When deleting first Node CRD", func() {
		It("Should delete CRD", func() {
			lrnAgent := agentsv1beta.LightrunNodeAgent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      lrnagent1Name,
					Namespace: testNamespace,
				},
			}
			Expect(k8sClient.Delete(ctx, &lrnAgent)).Should(Succeed())
		})

		It("Should remove volumes from the deployment", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				for _, volume := range patchedNodeDepl.Spec.Template.Spec.Volumes {
					if volume.Name == nodeInitVolume || volume.Name == nodeCmVolumeName {
						return false
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should remove annotations from deployment", func() {
			Eventually(func() bool {
				for k := range patchedNodeDepl.ObjectMeta.Annotations {
					if strings.Contains(k, "lightrun.com") {
						return false
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should remove init container from the deployment", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				return len(patchedNodeDepl.Spec.Template.Spec.InitContainers) == 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should rollback "+nodeEnvVarName+" env var and remove "+cliFlagsEnvVarName, func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				for _, container := range patchedNodeDepl.Spec.Template.Spec.Containers {
					if container.Name == "no-patch" {
						continue
					}
					for _, envVar := range container.Env {
						if envVar.Name == cliFlagsEnvVarName {
							return false
						}
						if container.Name == "app" && envVar.Name == nodeEnvVarName {
							return false
						}
						if container.Name == "app2" && envVar.Name == nodeEnvVarName {
							if envVar.Value != nodeEnvNonEmptyValue {
								return false
							}
						}
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should delete config map", func() {
			Expect(k8sClient.Get(ctx, nodeCmRequest, &nodeCm)).Error()
		})

		It("Should remove Volume mounts from containers in the deployment", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest, &patchedNodeDepl); err != nil {
					return false
				}
				for _, container := range patchedNodeDepl.Spec.Template.Spec.Containers {
					if container.Name != "no-patch" {
						if len(container.VolumeMounts) != 0 {
							return false
						}
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})
	})

	// Create and delete deployment matching 2nd CRD to check that finalizer was removed
	Context("When deleting deployment before removing Node CRD", func() {
		It("prepare deployment for 2nd CRD", func() {
			By("Creating deployment")
			depl := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      nodeDeployment + "-2",
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "node-app"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "node-app"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "node:18-alpine",
								},
								{
									Name:  "app2",
									Image: "node:18-alpine",
									Env: []corev1.EnvVar{
										{
											Name:  nodeEnvVarName,
											Value: nodeEnvNonEmptyValue,
										},
									},
								},
								{
									Name:  "no-patch",
									Image: "node:18-alpine",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, &depl)).Should(Succeed())
		})

		It("Should add finalizer to second CRD", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest2, &lrnAgent2); err != nil {
					return false
				}
				return len(lrnAgent2.ObjectMeta.Finalizers) != 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should patch Env Vars of containers with default agent arg (no CLI flags set)", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest2, &patchedNodeDepl2); err != nil {
					return false
				}
				for _, container := range patchedNodeDepl2.Spec.Template.Spec.Containers {
					for _, envVar := range container.Env {
						if envVar.Name == nodeEnvVarName {
							if container.Name == "app" {
								if envVar.Value != defaultNodeAgentArg {
									return false
								}
							} else if container.Name == "app2" {
								if envVar.Value != nodeEnvNonEmptyValue+" "+defaultNodeAgentArg {
									return false
								}
							}
						}
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should delete deployment", func() {
			depl := appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      nodeDeployment + "-2",
					Namespace: testNamespace,
				},
			}
			Expect(k8sClient.Delete(ctx, &depl)).Should(Succeed())
		})

		It("Should remove finalizer from the second CRD", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest2, &lrnAgent2); err != nil {
					return false
				}
				return len(lrnAgent2.ObjectMeta.Finalizers) == 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})
	})

	Context("When creating Node CR with deployment already patched by another Node CR", func() {
		It("Should create Deployment", func() {
			By("Creating deployment")
			depl := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      nodeDeployment + "-2",
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "node-app"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "node-app"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "node:18-alpine",
								},
								{
									Name:  "app2",
									Image: "node:18-alpine",
									Env: []corev1.EnvVar{
										{
											Name:  nodeEnvVarName,
											Value: nodeEnvNonEmptyValue,
										},
									},
								},
								{
									Name:  "no-patch",
									Image: "node:18-alpine",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, &depl)).Should(Succeed())
		})

		It("Should have successful status of existing CR", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest2, &lrnAgent2); err != nil {
					return false
				}
				return lrnAgent2.Status.WorkloadStatus == "Ready"
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("prepare new Node CR with patched deployment", func() {
			By("Creating new CR")
			lrnAgent3 := agentsv1beta.LightrunNodeAgent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "duplicate-node",
					Namespace: testNamespace,
				},
				Spec: agentsv1beta.LightrunNodeAgentSpec{
					WorkloadName:      nodeDeployment + "-2",
					WorkloadType:      agentsv1beta.WorkloadTypeDeployment,
					SecretName:        nodeSecretName,
					ServerHostname:    nodeServer,
					AgentName:         nodeAgentName,
					AgentTags:         nodeAgentTags,
					AgentConfig:       nodeAgentConfig,
					AgentEnvVarName:   nodeEnvVarName,
					ContainerSelector: nodeContainerSelector,
					InitContainer: agentsv1beta.InitContainer{
						Image:                 nodeInitImage,
						SharedVolumeName:      nodeInitVolume,
						SharedVolumeMountPath: nodeMountPath,
					},
				},
			}
			Expect(k8sClient.Create(ctx, &lrnAgent3)).Should(Succeed())
		})

		It("Should have failed status of CR (double-patch protection)", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest3, &lrnAgent3); err != nil {
					return false
				}
				return lrnAgent3.Status.WorkloadStatus == "ReconcileFailed"
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should not add finalizer to the duplicate CR", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest3, &lrnAgent3); err != nil {
					return false
				}
				return len(lrnAgent3.ObjectMeta.Finalizers) == 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should keep deployment annotation of the original CR", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest2, &patchedNodeDepl2); err != nil {
					return false
				}
				return patchedNodeDepl2.Annotations[nodeAnnotationAgentName] == lrnAgent2.Name
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})
	})

	Context("When trying to patch a Node deployment in the wrong namespace", func() {
		It("Should create Deployment", func() {
			By("Creating deployment")
			depl := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      nodeDeployment + "-3",
					Namespace: nodeWrongNS,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "node-app"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "node-app"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "node:18-alpine",
								},
								{
									Name:  "no-patch",
									Image: "node:18-alpine",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, &depl)).Should(Succeed())
		})

		It("Should create CR in the wrong namespace", func() {
			By("Creating new CR")
			lrnAgent4 := agentsv1beta.LightrunNodeAgent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wrong-namespace-node",
					Namespace: nodeWrongNS,
				},
				Spec: agentsv1beta.LightrunNodeAgentSpec{
					WorkloadName:      nodeDeployment + "-3",
					WorkloadType:      agentsv1beta.WorkloadTypeDeployment,
					SecretName:        nodeSecretName,
					ServerHostname:    nodeServer,
					AgentName:         nodeAgentName,
					AgentTags:         nodeAgentTags,
					AgentConfig:       nodeAgentConfig,
					AgentEnvVarName:   nodeEnvVarName,
					ContainerSelector: nodeContainerSelector,
					InitContainer: agentsv1beta.InitContainer{
						Image:                 nodeInitImage,
						SharedVolumeName:      nodeInitVolume,
						SharedVolumeMountPath: nodeMountPath,
					},
				},
			}
			Expect(k8sClient.Create(ctx, &lrnAgent4)).Should(Succeed())
		})

		It("Should not change the CR status", func() {
			Consistently(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest4, &lrnAgent4); err != nil {
					return false
				}
				return lrnAgent4.Status.WorkloadStatus == "" && lrnAgent4.Status.Conditions == nil
			}, nodeDuration, nodeInterval).Should(BeTrue())
		})

		It("Should not patch the deployment", func() {
			Consistently(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest3, &patchedNodeDepl3); err != nil {
					return false
				}
				if _, ok := patchedNodeDepl3.Annotations[nodeAnnotationAgentName]; !ok && len(patchedNodeDepl3.Finalizers) == 0 {
					return true
				}
				return false
			}, nodeDuration, nodeInterval).Should(BeTrue())
		})
	})

	Context("When changing the agent arg (update flow)", func() {
		It("Should create Deployment", func() {
			By("Creating deployment")
			depl := appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      nodeDeployment + "-4",
					Namespace: testNamespace,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "node-app"},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": "node-app"},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "app",
									Image: "node:18-alpine",
								},
								{
									Name:  "app2",
									Image: "node:18-alpine",
									Env: []corev1.EnvVar{
										{
											Name:  nodeEnvVarName,
											Value: nodeEnvNonEmptyValue,
										},
									},
								},
								{
									Name:  "no-patch",
									Image: "node:18-alpine",
								},
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, &depl)).Should(Succeed())
		})

		It("Should create CR targeting this deployment", func() {
			By("Creating new CR")
			lrnAgent5 := agentsv1beta.LightrunNodeAgent{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "change-node-agent-arg",
					Namespace: testNamespace,
				},
				Spec: agentsv1beta.LightrunNodeAgentSpec{
					WorkloadName:      nodeDeployment + "-4",
					WorkloadType:      agentsv1beta.WorkloadTypeDeployment,
					SecretName:        nodeSecretName,
					ServerHostname:    nodeServer,
					AgentName:         nodeAgentName,
					AgentTags:         nodeAgentTags,
					AgentConfig:       nodeAgentConfig,
					AgentEnvVarName:   nodeEnvVarName,
					ContainerSelector: nodeContainerSelector,
					InitContainer: agentsv1beta.InitContainer{
						Image:                 nodeInitImage,
						SharedVolumeName:      nodeInitVolume,
						SharedVolumeMountPath: nodeMountPath,
					},
				},
			}
			Expect(k8sClient.Create(ctx, &lrnAgent5)).Should(Succeed())
		})

		It("Should patch Env Vars of containers with default agent arg", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeDeplRequest4, &patchedNodeDepl4); err != nil {
					return false
				}
				for _, container := range patchedNodeDepl4.Spec.Template.Spec.Containers {
					for _, envVar := range container.Env {
						if envVar.Name == nodeEnvVarName {
							if container.Name == "app" {
								if envVar.Value != defaultNodeAgentArg {
									return false
								}
							} else if container.Name == "app2" {
								if envVar.Value != nodeEnvNonEmptyValue+" "+defaultNodeAgentArg {
									return false
								}
							}
						}
					}
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should track the patched env name/value via annotations", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, lrnAgentRequest5, &lrnAgent5); err != nil {
					return false
				}
				if patchedNodeDepl4.Annotations[nodeAnnotationAgentName] != lrnAgent5.Name {
					return false
				}
				if patchedNodeDepl4.Annotations[nodeAnnotationPatchedEnvName] != nodeEnvVarName {
					return false
				}
				if patchedNodeDepl4.Annotations[nodeAnnotationPatchedEnvValue] != defaultNodeAgentArg {
					return false
				}
				return true
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		Context("When changing the mount path (changes the --require agent arg)", func() {
			const changedMountPath = "/lightrun-v2"
			changedAgentArg := "--require " + changedMountPath + "/agent/lightrun_agent_bootstrap.js"

			It("Should update the CR's SharedVolumeMountPath", func() {
				Eventually(func() bool {
					if err := k8sClient.Get(ctx, lrnAgentRequest5, &lrnAgent5); err != nil {
						return false
					}
					lrnAgent5.Spec.InitContainer.SharedVolumeMountPath = changedMountPath
					err = k8sClient.Update(ctx, &lrnAgent5)
					return err == nil
				}, nodeTimeout, nodeInterval).Should(BeTrue())
			})

			It("Should swap the NODE_OPTIONS value rather than duplicating it", func() {
				Eventually(func() bool {
					if err := k8sClient.Get(ctx, nodeDeplRequest4, &patchedNodeDepl4); err != nil {
						return false
					}
					if patchedNodeDepl4.Annotations[nodeAnnotationPatchedEnvValue] != changedAgentArg {
						return false
					}
					for _, container := range patchedNodeDepl4.Spec.Template.Spec.Containers {
						for _, envVar := range container.Env {
							if envVar.Name != nodeEnvVarName {
								continue
							}
							if strings.Contains(envVar.Value, defaultNodeAgentArg) {
								// old value must have been removed, not left alongside the new one
								return false
							}
							if container.Name == "app" && envVar.Value != changedAgentArg {
								return false
							}
							if container.Name == "app2" && envVar.Value != nodeEnvNonEmptyValue+" "+changedAgentArg {
								return false
							}
						}
					}
					return true
				}, nodeTimeout, nodeInterval).Should(BeTrue())
			})
		})
	})

	It("Should create Node StatefulSet", func() {
		By("Creating StatefulSet")
		ctx := context.Background()

		sts := appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "StatefulSet"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodeStatefulset,
				Namespace: testNamespace,
			},
			Spec: appsv1.StatefulSetSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "node-stateful-app"},
				},
				ServiceName: "node-stateful-app-service",
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{"app": "node-stateful-app"},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "app",
								Image: "node:18-alpine",
							},
							{
								Name:  "app2",
								Image: "node:18-alpine",
								Env: []corev1.EnvVar{
									{
										Name:  nodeEnvVarName,
										Value: nodeEnvNonEmptyValue,
									},
								},
							},
							{
								Name:  "no-patch",
								Image: "node:18-alpine",
							},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, &sts)).Should(Succeed())
	})

	Context("When patching Node StatefulSet matched by CRD", func() {
		It("Should add init Container to StatefulSet", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeStsRequest, &patchedNodeSts); err != nil {
					return false
				}
				return len(patchedNodeSts.Spec.Template.Spec.InitContainers) != 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should add volumes to StatefulSet", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeStsRequest, &patchedNodeSts); err != nil {
					return false
				}
				// 3 volumes: shared init volume, configmap volume, and secret volume (useSecretsAsMountedFiles defaults to true)
				if len(patchedNodeSts.Spec.Template.Spec.Volumes) == 3 {
					hasInitVolume := false
					hasSecretVolume := false
					for _, v := range patchedNodeSts.Spec.Template.Spec.Volumes {
						if v.Name == nodeInitVolume {
							hasInitVolume = true
						}
						if v.Name == "lightrun-secret" {
							hasSecretVolume = true
						}
					}
					return hasInitVolume && hasSecretVolume
				}
				return false
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should patch StatefulSet containers with VolumeMounts", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeStsRequest, &patchedNodeSts); err != nil {
					return false
				}
				for _, c := range patchedNodeSts.Spec.Template.Spec.Containers {
					if c.Name == "app" {
						for _, v := range c.VolumeMounts {
							if v.Name == nodeInitVolume {
								return true
							}
						}
					}
				}
				return false
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should patch StatefulSet NODE_OPTIONS environment variable", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeStsRequest, &patchedNodeSts); err != nil {
					return false
				}

				for _, c := range patchedNodeSts.Spec.Template.Spec.Containers {
					if c.Name == "app" {
						for _, e := range c.Env {
							if e.Name == nodeEnvVarName && strings.Contains(e.Value, defaultNodeAgentArg) {
								return true
							}
						}
					}
				}
				return false
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should set LIGHTRUN_AGENT_CLI_FLAGS on StatefulSet containers, not concatenated into NODE_OPTIONS", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeStsRequest, &patchedNodeSts); err != nil {
					return false
				}

				for _, c := range patchedNodeSts.Spec.Template.Spec.Containers {
					if c.Name == "app" {
						foundCliFlags := false
						for _, e := range c.Env {
							if e.Name == nodeEnvVarName && strings.Contains(e.Value, nodeAgentCliFlags) {
								return false
							}
							if e.Name == cliFlagsEnvVarName && e.Value == nodeAgentCliFlags {
								foundCliFlags = true
							}
						}
						return foundCliFlags
					}
				}
				return false
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should append to existing NODE_OPTIONS value on a container that already has one", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeStsRequest, &patchedNodeSts); err != nil {
					return false
				}

				for _, c := range patchedNodeSts.Spec.Template.Spec.Containers {
					if c.Name == "app2" {
						for _, e := range c.Env {
							if e.Name == nodeEnvVarName && strings.Contains(e.Value, defaultNodeAgentArg) && strings.Contains(e.Value, nodeEnvNonEmptyValue) {
								return true
							}
						}
					}
				}
				return false
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})
	})

	Context("When deleting LightrunNodeAgent for StatefulSet", func() {
		It("Should remove the finalizer from StatefulSet-targeting LightrunNodeAgent", func() {
			err := k8sClient.Get(ctx, lrnAgentStsRequest, &lrnAgentSts)
			Expect(err).ToNot(HaveOccurred())

			err = k8sClient.Delete(ctx, &lrnAgentSts)
			Expect(err).ToNot(HaveOccurred())

			// Verify the finalizer gets removed
			Eventually(func() bool {
				err := k8sClient.Get(ctx, lrnAgentStsRequest, &lrnAgentSts)
				if err != nil {
					return client.IgnoreNotFound(err) == nil
				}
				return len(lrnAgentSts.Finalizers) == 0
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})

		It("Should restore StatefulSet to original state", func() {
			Eventually(func() bool {
				if err := k8sClient.Get(ctx, nodeStsRequest, &patchedNodeSts); err != nil {
					return false
				}

				// Check that the initContainer is removed
				hasInitContainer := len(patchedNodeSts.Spec.Template.Spec.InitContainers) > 0

				// Check agent environment variables are removed
				hasAgentEnv := false
				for _, c := range patchedNodeSts.Spec.Template.Spec.Containers {
					if c.Name == "app" {
						for _, e := range c.Env {
							if e.Name == nodeEnvVarName && strings.Contains(e.Value, defaultNodeAgentArg) {
								hasAgentEnv = true
								break
							}
							if e.Name == cliFlagsEnvVarName {
								hasAgentEnv = true
								break
							}
						}
					}
				}

				// Check lightrun ownership annotation is removed
				_, hasAnnotation := patchedNodeSts.Annotations[nodeAnnotationAgentName]

				// All should be false for a restored statefulset
				return !hasInitContainer && !hasAgentEnv && !hasAnnotation
			}, nodeTimeout, nodeInterval).Should(BeTrue())
		})
	})
})
