package webhook

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

// Annotation keys per the lightrun.com/* contract documented in
// .dot-agent-deck/webhook-refactor-context.md, as amended by that doc's "DESIGN CHANGE"
// section (2026-07-14): lightrun.com/secret-name + lightrun.com/server-hostname are gone
// from the Pod; pods now name an AgentPool CR (namespaced only) instead, and the webhook
// resolves credentials/server hostname from it. Defined locally (not imported from an
// implementation package) so this test file expresses the contract on its own terms.
const (
	annAgentType       = "lightrun.com/agent-type"
	annAgentPool       = "lightrun.com/agent-pool"
	annContainerSel    = "lightrun.com/container-selector"
	annAgentEnvVarName = "lightrun.com/agent-env-var-name"
	annAgentCliFlags   = "lightrun.com/agent-cli-flags"
	annAgentTags       = "lightrun.com/agent-tags"
	annAgentName       = "lightrun.com/agent-name"
	annAgentConfig     = "lightrun.com/agent-config"
	annUseMountedFiles = "lightrun.com/use-secrets-as-mounted-files"
)

// Mirrors internal/controller/patch_funcs.go's initContainerName -- kept identical since
// the injection mechanics carry over conceptually from the old mechanism.
const initContainerName = "lightrun-installer"

// secretVolumeName mirrors the "lightrun-secret" volume name used by the old
// addVolume/addVolumeToStatefulSet when UseSecretsAsMountedFiles is true.
const secretVolumeName = "lightrun-secret"

const (
	secretKeyLightrunKey    = "lightrun_key"
	secretKeyPinnedCertHash = "pinned_cert_hash"
)

var podNameCounter int

// uniqueName returns a fresh name per call so specs don't collide on object names within
// the shared envtest namespace (and, for AgentPool, across the whole namespace too).
func uniqueName(prefix string) string {
	podNameCounter++
	return fmt.Sprintf("%s-%d", prefix, podNameCounter)
}

func newSecret(name string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		StringData: map[string]string{
			secretKeyLightrunKey:    "shhh-this-is-the-lightrun-key",
			secretKeyPinnedCertHash: "deadbeefcafe",
		},
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	return secret
}

// newAgentPool creates a namespaced AgentPool (in testNamespace) referencing secretName
// (also expected to live in testNamespace, per the AgentPool contract).
func newAgentPool(name, secretName, serverHostname string) *agentsv1beta.AgentPool {
	pool := &agentsv1beta.AgentPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: agentsv1beta.AgentPoolSpec{
			SecretRef:      agentsv1beta.AgentPoolSecretRef{Name: secretName},
			ServerHostname: serverHostname,
		},
	}
	Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	return pool
}

func containerNamed(name string, env ...corev1.EnvVar) corev1.Container {
	return corev1.Container{Name: name, Image: "busybox", Env: env}
}

func newPod(name string, annotations map[string]string, containers []corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   testNamespace,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Containers: containers,
		},
	}
}

// volumeMount returns the VolumeMount named volName on container c, or nil if absent.
func volumeMount(c corev1.Container, volName string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == volName {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}

func envValue(c corev1.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func containerByName(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

var _ = Describe("Pod mutating webhook", func() {

	// --- 1. Full annotation set on a recognized agent-type ("java") mutates the pod,
	//        resolving credentials/server hostname from a referenced AgentPool. ---
	Context("when a Pod carries a complete java annotation set referencing an AgentPool", func() {
		var (
			podName        string
			secretName     string
			poolName       string
			serverHostname string
			created        corev1.Pod
		)

		BeforeEach(func() {
			secretName = uniqueName("java-secret")
			newSecret(secretName)

			serverHostname = "lightrun.example.com"
			poolName = uniqueName("java-pool")
			newAgentPool(poolName, secretName, serverHostname)

			podName = uniqueName("java-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "app,sidecar",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
			}, []corev1.Container{
				containerNamed("app"),
				containerNamed("sidecar", corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", Value: "-Xmx512m"}),
				containerNamed("untouched"),
			})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
		})

		It("adds exactly one lightrun-installer init container", func() {
			Expect(created.Spec.InitContainers).To(HaveLen(1))
			Expect(created.Spec.InitContainers[0].Name).To(Equal(initContainerName))
			Expect(created.Spec.InitContainers[0].Image).To(Equal(initContainerImage))
		})

		It("resolves LIGHTRUN_SERVER on the init container from the referenced AgentPool's serverHostname", func() {
			Expect(created.Spec.InitContainers).To(HaveLen(1))
			val, ok := envValue(created.Spec.InitContainers[0], "LIGHTRUN_SERVER")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal(serverHostname))
		})

		It("adds the shared emptyDir volume", func() {
			var shared *corev1.Volume
			for i := range created.Spec.Volumes {
				if created.Spec.Volumes[i].Name == sharedVolumeName {
					shared = &created.Spec.Volumes[i]
				}
			}
			Expect(shared).NotTo(BeNil())
			Expect(shared.EmptyDir).NotTo(BeNil())
		})

		It("mounts the shared volume only into the containers named in container-selector", func() {
			for _, name := range []string{"app", "sidecar"} {
				c := containerByName(created.Spec.Containers, name)
				Expect(c).NotTo(BeNil())
				vm := volumeMount(*c, sharedVolumeName)
				Expect(vm).NotTo(BeNil(), "container %q should have the shared volume mounted", name)
				Expect(vm.MountPath).To(Equal(sharedVolumeMountPath))
			}

			untouched := containerByName(created.Spec.Containers, "untouched")
			Expect(untouched).NotTo(BeNil())
			Expect(volumeMount(*untouched, sharedVolumeName)).To(BeNil())
		})

		It("sets the agent env var to the -agentpath argument on a container with no prior value", func() {
			app := containerByName(created.Spec.Containers, "app")
			Expect(app).NotTo(BeNil())
			val, ok := envValue(*app, "JAVA_TOOL_OPTIONS")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("-agentpath:" + sharedVolumeMountPath + "/agent/lightrun_agent.so"))
		})

		It("appends the agent env var to a container that already sets it", func() {
			sidecar := containerByName(created.Spec.Containers, "sidecar")
			Expect(sidecar).NotTo(BeNil())
			val, ok := envValue(*sidecar, "JAVA_TOOL_OPTIONS")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("-Xmx512m -agentpath:" + sharedVolumeMountPath + "/agent/lightrun_agent.so"))
		})

		It("does not touch env vars or volume mounts of a container outside container-selector", func() {
			untouched := containerByName(created.Spec.Containers, "untouched")
			Expect(untouched).NotTo(BeNil())
			_, ok := envValue(*untouched, "JAVA_TOOL_OPTIONS")
			Expect(ok).To(BeFalse())
			Expect(untouched.VolumeMounts).To(BeEmpty())
		})
	})

	// --- 2. Pod with no lightrun.com/* annotations at all: admitted completely unchanged. ---
	Context("when a Pod has no lightrun.com/* annotations", func() {
		var (
			podName string
			created corev1.Pod
		)

		BeforeEach(func() {
			podName = uniqueName("plain-pod")
			pod := newPod(podName, nil, []corev1.Container{containerNamed("app")})
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
		})

		It("adds no init containers", func() {
			Expect(created.Spec.InitContainers).To(BeEmpty())
		})

		It("adds no volumes", func() {
			Expect(created.Spec.Volumes).To(BeEmpty())
		})

		It("leaves the single container's env and volume mounts untouched", func() {
			app := containerByName(created.Spec.Containers, "app")
			Expect(app).NotTo(BeNil())
			Expect(app.Env).To(BeEmpty())
			Expect(app.VolumeMounts).To(BeEmpty())
		})
	})

	// --- 3. Unrecognized agent-type (future-proofing for e.g. nodejs/python): admitted unchanged. ---
	Context("when lightrun.com/agent-type is set to an unimplemented value", func() {
		var (
			podName string
			created corev1.Pod
		)

		BeforeEach(func() {
			podName = uniqueName("nodejs-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "nodejs",
				annAgentPool:       "irrelevant-pool",
				annContainerSel:    "app",
				annAgentEnvVarName: "NODE_OPTIONS",
			}, []corev1.Container{containerNamed("app")})

			// Note: the named pool intentionally does not exist, and required java-only
			// annotations aren't fully meaningful for nodejs -- since the type isn't
			// implemented at all, the webhook must skip its own annotation validation
			// (and any pool resolution) entirely and admit the Pod as-is (it must not,
			// e.g., reject this Pod for a "missing" pool it doesn't even look up yet).
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
		})

		It("adds no init containers", func() {
			Expect(created.Spec.InitContainers).To(BeEmpty())
		})

		It("adds no volumes and does not touch the container", func() {
			Expect(created.Spec.Volumes).To(BeEmpty())
			app := containerByName(created.Spec.Containers, "app")
			Expect(app).NotTo(BeNil())
			Expect(app.Env).To(BeEmpty())
			Expect(app.VolumeMounts).To(BeEmpty())
		})
	})

	// --- 4. container-selector names a container that doesn't exist in the Pod. ---
	//
	// Design decision (documented here per the task): REJECT the create via admission
	// error rather than silently admitting the Pod unmutated. This mirrors today's
	// controller behavior, which surfaces "unable to find matching container to patch"
	// as a hard failure (via CR status) rather than a silent no-op -- since a
	// misconfigured container-selector means the operator's request literally cannot be
	// fulfilled, failing loudly at admission time (before the Pod starts) is safer than
	// silently starting a Pod nobody is actually instrumenting.
	Context("when container-selector names a container absent from the Pod", func() {
		It("denies the Pod create with an error naming the missing container", func() {
			secretName := uniqueName("missing-container-secret")
			newSecret(secretName)
			poolName := uniqueName("missing-container-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			podName := uniqueName("missing-container-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "does-not-exist",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
			}, []corev1.Container{containerNamed("app")})

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does-not-exist"))
		})
	})

	// --- 5. A required annotation is missing (agent-type present, e.g. agent-pool absent). ---
	//
	// Design decision (documented here per the task): REJECT the create, same rationale
	// as #4 -- once lightrun.com/agent-type opts a Pod into a recognized, implemented
	// agent type, the rest of that type's required annotations (now including
	// lightrun.com/agent-pool, replacing the old secret-name/server-hostname pair) are
	// mandatory and enforced strictly; this is different from #2/#3 where the Pod never
	// opted in (or opted into a type we don't implement) and is therefore left alone.
	Context("when a required annotation is missing for a recognized agent-type", func() {
		It("denies the Pod create with an error naming the missing lightrun.com/agent-pool annotation", func() {
			podName := uniqueName("missing-annotation-pod")
			pod := newPod(podName, map[string]string{
				annAgentType: "java",
				// annAgentPool intentionally omitted
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
			}, []corev1.Container{containerNamed("app")})

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(annAgentPool))
		})
	})

	// --- 6. lightrun.com/agent-pool resolution. ---
	Context("lightrun.com/agent-pool resolution", func() {
		It("denies the Pod create with an error naming the missing pool", func() {
			podName := uniqueName("missing-pool-pod")
			missingPoolName := uniqueName("does-not-exist-pool")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       missingPoolName,
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
			}, []corev1.Container{containerNamed("app")})

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(missingPoolName))
		})
	})

	// --- 7. Optional annotations respected when present, sensible defaults when absent. ---
	Context("optional annotations", func() {
		Context("lightrun.com/agent-cli-flags", func() {
			It("is appended to the -agentpath argument when present", func() {
				secretName := uniqueName("cli-flags-secret")
				newSecret(secretName)
				poolName := uniqueName("cli-flags-pool")
				newAgentPool(poolName, secretName, "lightrun.example.com")

				podName := uniqueName("cli-flags-pod")
				pod := newPod(podName, map[string]string{
					annAgentType:       "java",
					annAgentPool:       poolName,
					annContainerSel:    "app",
					annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
					annAgentCliFlags:   "--lightrun_extra_class_path=<PATH_TO_JAR>",
				}, []corev1.Container{containerNamed("app")})

				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				var created corev1.Pod
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

				app := containerByName(created.Spec.Containers, "app")
				val, ok := envValue(*app, "JAVA_TOOL_OPTIONS")
				Expect(ok).To(BeTrue())
				Expect(val).To(Equal("-agentpath:" + sharedVolumeMountPath + "/agent/lightrun_agent.so=--lightrun_extra_class_path=<PATH_TO_JAR>"))
			})

			It("produces the bare -agentpath argument (no trailing '=') when absent", func() {
				secretName := uniqueName("no-cli-flags-secret")
				newSecret(secretName)
				poolName := uniqueName("no-cli-flags-pool")
				newAgentPool(poolName, secretName, "lightrun.example.com")

				podName := uniqueName("no-cli-flags-pod")
				pod := newPod(podName, map[string]string{
					annAgentType:       "java",
					annAgentPool:       poolName,
					annContainerSel:    "app",
					annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
				}, []corev1.Container{containerNamed("app")})

				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				var created corev1.Pod
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

				app := containerByName(created.Spec.Containers, "app")
				val, ok := envValue(*app, "JAVA_TOOL_OPTIONS")
				Expect(ok).To(BeTrue())
				Expect(val).To(Equal("-agentpath:" + sharedVolumeMountPath + "/agent/lightrun_agent.so"))
			})

			// Preserves the existing 1024-char limit from
			// internal/controller/helpers.go's agentEnvVarArgument (see also
			// Test_buildAgentPathArg in agentpath_test.go for the pure-function case):
			// a -agentpath argument that would exceed 1024 chars is a hard Java
			// limitation, so admission must reject it rather than silently truncating
			// or admitting a broken agent invocation.
			It("denies the Pod create when the resulting -agentpath argument would exceed 1024 chars", func() {
				secretName := uniqueName("too-long-secret")
				newSecret(secretName)
				poolName := uniqueName("too-long-pool")
				newAgentPool(poolName, secretName, "lightrun.example.com")

				podName := uniqueName("too-long-cli-flags-pod")

				longFlags := ""
				for len(longFlags) < 1024 {
					longFlags += "a"
				}

				pod := newPod(podName, map[string]string{
					annAgentType:       "java",
					annAgentPool:       poolName,
					annContainerSel:    "app",
					annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
					annAgentCliFlags:   longFlags,
				}, []corev1.Container{containerNamed("app")})

				err := k8sClient.Create(ctx, pod)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("1024"))
			})
		})

		Context("lightrun.com/agent-tags, lightrun.com/agent-name, lightrun.com/agent-config", func() {
			It("passes agent-tags, agent-name and agent-config through to the init container as env vars when present", func() {
				secretName := uniqueName("metadata-secret")
				newSecret(secretName)
				poolName := uniqueName("metadata-pool")
				newAgentPool(poolName, secretName, "lightrun.example.com")

				podName := uniqueName("metadata-pod")
				pod := newPod(podName, map[string]string{
					annAgentType:       "java",
					annAgentPool:       poolName,
					annContainerSel:    "app",
					annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
					annAgentTags:       "prod,new_tag",
					annAgentName:       "coolio-agent",
					annAgentConfig:     `{"max_log_cpu_cost":"2"}`,
				}, []corev1.Container{containerNamed("app")})

				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				var created corev1.Pod
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

				Expect(created.Spec.InitContainers).To(HaveLen(1))
				init := created.Spec.InitContainers[0]

				tags, ok := envValue(init, "LIGHTRUN_AGENT_TAGS")
				Expect(ok).To(BeTrue())
				Expect(tags).To(Equal("prod,new_tag"))

				name, ok := envValue(init, "LIGHTRUN_AGENT_NAME")
				Expect(ok).To(BeTrue())
				Expect(name).To(Equal("coolio-agent"))

				config, ok := envValue(init, "LIGHTRUN_AGENT_CONFIG")
				Expect(ok).To(BeTrue())
				Expect(config).To(Equal(`{"max_log_cpu_cost":"2"}`))
			})

			It("sets none of LIGHTRUN_AGENT_TAGS/_NAME/_CONFIG on the init container when the annotations are absent", func() {
				secretName := uniqueName("no-metadata-secret")
				newSecret(secretName)
				poolName := uniqueName("no-metadata-pool")
				newAgentPool(poolName, secretName, "lightrun.example.com")

				podName := uniqueName("no-metadata-pod")
				pod := newPod(podName, map[string]string{
					annAgentType:       "java",
					annAgentPool:       poolName,
					annContainerSel:    "app",
					annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
				}, []corev1.Container{containerNamed("app")})

				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				var created corev1.Pod
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

				Expect(created.Spec.InitContainers).To(HaveLen(1))
				init := created.Spec.InitContainers[0]

				_, hasTags := envValue(init, "LIGHTRUN_AGENT_TAGS")
				Expect(hasTags).To(BeFalse())
				_, hasName := envValue(init, "LIGHTRUN_AGENT_NAME")
				Expect(hasName).To(BeFalse())
				_, hasConfig := envValue(init, "LIGHTRUN_AGENT_CONFIG")
				Expect(hasConfig).To(BeFalse())
			})
		})
	})

	// --- 8. Pool-referenced secret wiring: env-from-secret vs. mounted-file volume, never
	//        the raw value -- and only the *reference* (Secret name from the resolved
	//        pool's secretRef), never the Secret's actual value, ever appears in the Pod
	//        spec. ---
	//
	// Design decision (documented here per the task): lightrun.com/use-secrets-as-mounted-files
	// mirrors the old CRD field's kubebuilder default of `true` when absent, so a Pod
	// that omits the annotation gets the secret mounted as a volume, matching today's
	// out-of-the-box behavior.
	Context("resolved pool secretRef wiring", func() {
		It("mounts the secret as a read-only volume on the init container when use-secrets-as-mounted-files is absent (default true)", func() {
			secretName := uniqueName("mounted-secret")
			newSecret(secretName)
			poolName := uniqueName("mounted-secret-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			podName := uniqueName("mounted-secret-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			var secretVol *corev1.Volume
			for i := range created.Spec.Volumes {
				if created.Spec.Volumes[i].Name == secretVolumeName {
					secretVol = &created.Spec.Volumes[i]
				}
			}
			Expect(secretVol).NotTo(BeNil())
			Expect(secretVol.Secret).NotTo(BeNil())
			Expect(secretVol.Secret.SecretName).To(Equal(secretName))

			Expect(created.Spec.InitContainers).To(HaveLen(1))
			init := created.Spec.InitContainers[0]
			vm := volumeMount(init, secretVolumeName)
			Expect(vm).NotTo(BeNil())
			Expect(vm.ReadOnly).To(BeTrue())

			// The secret's actual values must never appear directly in the Pod spec --
			// only the reference (Secret name / volume), resolved from the AgentPool's
			// secretRef, does.
			for _, e := range init.Env {
				Expect(e.Value).NotTo(ContainSubstring("shhh-this-is-the-lightrun-key"))
				Expect(e.Value).NotTo(ContainSubstring("deadbeefcafe"))
			}
		})

		It("wires the secret as env-from-secret on the init container when use-secrets-as-mounted-files is \"false\"", func() {
			secretName := uniqueName("env-secret")
			newSecret(secretName)
			poolName := uniqueName("env-secret-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			podName := uniqueName("env-secret-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
				annUseMountedFiles: "false",
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			for _, v := range created.Spec.Volumes {
				Expect(v.Name).NotTo(Equal(secretVolumeName))
			}

			Expect(created.Spec.InitContainers).To(HaveLen(1))
			init := created.Spec.InitContainers[0]

			var keyEnv, certEnv *corev1.EnvVar
			for i := range init.Env {
				if init.Env[i].Name == "LIGHTRUN_KEY" {
					keyEnv = &init.Env[i]
				}
				if init.Env[i].Name == "PINNED_CERT" {
					certEnv = &init.Env[i]
				}
			}
			Expect(keyEnv).NotTo(BeNil())
			Expect(keyEnv.ValueFrom).NotTo(BeNil())
			Expect(keyEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())
			Expect(keyEnv.ValueFrom.SecretKeyRef.Name).To(Equal(secretName))
			Expect(keyEnv.ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyLightrunKey))
			Expect(keyEnv.Value).To(BeEmpty(), "the literal secret value must not be duplicated into the pod spec")

			Expect(certEnv).NotTo(BeNil())
			Expect(certEnv.ValueFrom).NotTo(BeNil())
			Expect(certEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())
			Expect(certEnv.ValueFrom.SecretKeyRef.Name).To(Equal(secretName))
			Expect(certEnv.ValueFrom.SecretKeyRef.Key).To(Equal(secretKeyPinnedCertHash))
			Expect(certEnv.Value).To(BeEmpty(), "the literal secret value must not be duplicated into the pod spec")
		})
	})

	// --- 9. ConfigMap creation/dedup for the agent.config + agent.metadata.json data the
	//        real init container image reads from cmMountPath -- see pod_mutator.go's
	//        buildAgentConfigMapData/agentConfigMapName/ensureAgentConfigMap. The specs
	//        above only exercise this indirectly (they'd fail if ConfigMap creation errored
	//        out mutation entirely); these assert its actual content and dedup behavior
	//        directly. ---
	Context("agent config ConfigMap", func() {
		// configMapNameFromPod returns the name of the ConfigMap referenced by pod's
		// cmVolumeName volume, or "" if that volume is absent.
		configMapNameFromPod := func(pod *corev1.Pod) string {
			for i := range pod.Spec.Volumes {
				if pod.Spec.Volumes[i].Name == cmVolumeName && pod.Spec.Volumes[i].ConfigMap != nil {
					return pod.Spec.Volumes[i].ConfigMap.Name
				}
			}
			return ""
		}

		It("creates a ConfigMap with the expected agent.config/agent.metadata.json content, referenced by the pod's ConfigMap volume", func() {
			secretName := uniqueName("cm-secret")
			newSecret(secretName)
			poolName := uniqueName("cm-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			podName := uniqueName("cm-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
				annAgentTags:       "prod,new_tag",
				annAgentName:       "coolio-agent",
				annAgentConfig:     `{"max_log_cpu_cost":"2"}`,
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			cmName := configMapNameFromPod(&created)
			Expect(cmName).NotTo(BeEmpty())
			Expect(cmName).To(HavePrefix(cmNamePrefix))

			var cm corev1.ConfigMap
			Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: cmName}, &cm)).To(Succeed())

			Expect(cm.Data).To(HaveKey(cmDataKeyConfig))
			Expect(cm.Data[cmDataKeyConfig]).To(Equal("max_log_cpu_cost=2\n"))

			Expect(cm.Data).To(HaveKey(cmDataKeyMetadata))
			Expect(cm.Data[cmDataKeyMetadata]).To(MatchJSON(`{
				"registration": {
					"displayName": "coolio-agent",
					"tags": [{"name": "prod"}, {"name": "new_tag"}]
				}
			}`))

			var cmVol *corev1.Volume
			for i := range created.Spec.Volumes {
				if created.Spec.Volumes[i].Name == cmVolumeName {
					cmVol = &created.Spec.Volumes[i]
				}
			}
			Expect(cmVol).NotTo(BeNil())
			Expect(cmVol.ConfigMap.Items).To(ConsistOf(
				corev1.KeyToPath{Key: cmDataKeyConfig, Path: cmItemPathConfig},
				corev1.KeyToPath{Key: cmDataKeyMetadata, Path: cmItemPathMetadata},
			))
		})

		It("reuses the same ConfigMap (no duplicate) for two pods with identical config-affecting annotations", func() {
			secretName := uniqueName("cm-dedup-secret")
			newSecret(secretName)
			poolName := uniqueName("cm-dedup-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			annotations := map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
				annAgentTags:       "dedup-tag",
				annAgentName:       "dedup-agent",
				annAgentConfig:     `{"k":"v"}`,
			}

			podAName := uniqueName("cm-dedup-pod-a")
			podA := newPod(podAName, annotations, []corev1.Container{containerNamed("app")})
			Expect(k8sClient.Create(ctx, podA)).To(Succeed())
			var createdA corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(podA), &createdA)).To(Succeed())

			// Second pod's annotations are identical except for the pool name (a fresh
			// pool pointing at the same secret/hostname) -- the pool reference itself
			// doesn't feed into the ConfigMap's content (only agent-config/tags/name do),
			// so this still exercises the "identical config" dedup path realistically
			// while giving each pod its own independently-created AgentPool.
			poolName2 := uniqueName("cm-dedup-pool-2")
			newAgentPool(poolName2, secretName, "lightrun.example.com")
			annotationsB := map[string]string{}
			for k, v := range annotations {
				annotationsB[k] = v
			}
			annotationsB[annAgentPool] = poolName2

			podBName := uniqueName("cm-dedup-pod-b")
			podB := newPod(podBName, annotationsB, []corev1.Container{containerNamed("app")})
			Expect(k8sClient.Create(ctx, podB)).To(Succeed())
			var createdB corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(podB), &createdB)).To(Succeed())

			cmNameA := configMapNameFromPod(&createdA)
			cmNameB := configMapNameFromPod(&createdB)
			Expect(cmNameA).NotTo(BeEmpty())
			Expect(cmNameA).To(Equal(cmNameB), "pods with identical config-affecting annotations must reference the same ConfigMap")

			var cmList corev1.ConfigMapList
			Expect(k8sClient.List(ctx, &cmList, client.InNamespace(testNamespace))).To(Succeed())
			matching := 0
			for _, item := range cmList.Items {
				if item.Name == cmNameA {
					matching++
				}
			}
			Expect(matching).To(Equal(1), "exactly one ConfigMap should exist under the shared content-derived name")
		})

		It("admits the Pod when the ConfigMap already exists (Create/AlreadyExists race handled gracefully)", func() {
			secretName := uniqueName("cm-race-secret")
			newSecret(secretName)
			poolName := uniqueName("cm-race-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			ann := map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
				annAgentName:       "race-agent",
			}

			// Pre-create the exact ConfigMap the webhook would create for these
			// annotations, simulating a concurrent admission request (or a prior pod)
			// having already won the race.
			data, err := buildAgentConfigMapData(ann)
			Expect(err).NotTo(HaveOccurred())
			preexistingName := agentConfigMapName(data)
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: preexistingName, Namespace: testNamespace},
				Data:       data,
			})).To(Succeed())

			podName := uniqueName("cm-race-pod")
			pod := newPod(podName, ann, []corev1.Container{containerNamed("app")})
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(configMapNameFromPod(&created)).To(Equal(preexistingName))
		})
	})

	// --- 10. container-selector present but resolves to zero container names (e.g. only
	//         commas/whitespace) after splitting/trimming. ---
	//
	// Distinct from #4 (a container-selector that names a container absent from the Pod):
	// here the annotation value itself is present and non-empty, but splits/trims down to
	// zero names, so the "missing container" loop in patchContainerEnvVar's caller never
	// even runs and nothing would be patched -- a silent no-op prior to this fix. Same
	// design decision as #4: DENY rather than silently admit unmutated, mirroring the old
	// controller's patchAppContainers "unable to find matching container to patch" error.
	Context("when container-selector resolves to zero matching container names", func() {
		It("denies the Pod create with a clear error instead of silently admitting it unmutated", func() {
			secretName := uniqueName("zero-match-secret")
			newSecret(secretName)
			poolName := uniqueName("zero-match-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			podName := uniqueName("zero-match-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    " , , ",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
			}, []corev1.Container{containerNamed("app")})

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("unable to find matching container to patch"))
		})
	})

	// --- 11. Combined (pre-existing + appended) env var value exceeds 1024 chars. ---
	//
	// Distinct from the isolated agentCliFlags-only 1024 check in section 7
	// (buildAgentPathArg / Test_buildAgentPathArg in agentpath_test.go): the old
	// controller's patchJavaToolEnv checked the *combined* value -- pre-existing content
	// already on the target env var, plus the newly appended -agentpath:... arg -- against
	// the same Java 1024-char limit. This asserts patchContainerEnvVar enforces that
	// combined-length check too, not just the new arg in isolation.
	Context("when the combined env var value would exceed 1024 chars", func() {
		It("denies the Pod create with an error naming the target env var", func() {
			secretName := uniqueName("long-env-secret")
			newSecret(secretName)
			poolName := uniqueName("long-env-pool")
			newAgentPool(poolName, secretName, "lightrun.example.com")

			// buildAgentPathArg("/lightrun", "") == "-agentpath:/lightrun/agent/lightrun_agent.so"
			// (45 chars). A pre-existing value of exactly 1024 chars pushes the combined
			// value (pre-existing + " " + agentArg) well over the limit once appended.
			preExisting := strings.Repeat("x", 1024)

			podName := uniqueName("long-env-pod")
			pod := newPod(podName, map[string]string{
				annAgentType:       "java",
				annAgentPool:       poolName,
				annContainerSel:    "app",
				annAgentEnvVarName: "JAVA_TOOL_OPTIONS",
			}, []corev1.Container{containerNamed("app", corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", Value: preExisting})})

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("JAVA_TOOL_OPTIONS"))
			Expect(err.Error()).To(ContainSubstring("1024"))
		})
	})
})
