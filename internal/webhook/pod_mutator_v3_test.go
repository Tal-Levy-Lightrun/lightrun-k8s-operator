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

// This file covers "DESIGN CHANGE v3" (.dot-agent-deck/webhook-refactor-context.md,
// 2026-07-15), which supersedes the annotation contract exercised in pod_mutator_test.go
// (that file's own "DESIGN CHANGE" section, 2026-07-14) wherever they conflict. Kept as a
// separate file rather than edited into pod_mutator_test.go so the currently-passing
// contract there stays intact and reviewable on its own, while this file's specs express the
// NEW contract and are expected to fail (RED) against the current implementation -- either
// via compile error (new pool-spec fields don't exist as pod-mutator inputs yet, or fail via
// admission behavior (the webhook doesn't recognize lightrun.com/inject-java, doesn't read a
// Namespace, and doesn't do ClusterAgentPool resolution at all yet).
//
// Per the design doc, all of the following are still true and unchanged from
// pod_mutator_test.go: the injection mechanics (init container/volume/agentpath), the
// ConfigMap dedup-by-content-hash mechanism, and the 1024-char limit.

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

// Annotation keys added/changed by DESIGN CHANGE v3. lightrun.com/agent-type,
// lightrun.com/agent-pool, and the per-field override annotations
// (annContainerSel/annAgentEnvVarName/annAgentCliFlags/annAgentTags/annAgentName/
// annAgentConfig/annUseMountedFiles) are all still defined in pod_mutator_test.go and reused
// here unchanged -- only the trigger annotation and the pool-kind disambiguator are new.
const (
	v3AnnInjectJava    = "lightrun.com/inject-java"
	v3AnnAgentPoolKind = "lightrun.com/agent-pool-kind"
)

func newNamespace(name string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns
}

func newNamespaceWithAnnotations(name string, annotations map[string]string) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns
}

// newSecretIn is newSecret (pod_mutator_test.go) generalized to an explicit namespace, needed
// here since v3 specs create their own namespaces rather than sharing testNamespace.
func newSecretIn(namespace, name string) *corev1.Secret {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		StringData: map[string]string{
			secretKeyLightrunKey:    "shhh-this-is-the-lightrun-key",
			secretKeyPinnedCertHash: "deadbeefcafe",
		},
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	return secret
}

func newAgentPoolIn(namespace, name string, spec agentsv1beta.AgentPoolSpec) *agentsv1beta.AgentPool {
	pool := &agentsv1beta.AgentPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
	Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	return pool
}

func newClusterAgentPool(name string, spec agentsv1beta.ClusterAgentPoolSpec) *agentsv1beta.ClusterAgentPool {
	pool := &agentsv1beta.ClusterAgentPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	return pool
}

// newPodIn is newPod (pod_mutator_test.go) generalized to an explicit namespace.
func newPodIn(namespace, name string, annotations map[string]string, containers []corev1.Container) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

var _ = Describe("DESIGN CHANGE v3: hierarchical config + ClusterAgentPool", func() {

	// --- v3 item 1: lightrun.com/inject-java replaces lightrun.com/agent-type as the
	//     trigger, and item 5: lightrun.com/agent-type absent now defaults to "java". ---
	Context("lightrun.com/inject-java as the trigger annotation", func() {
		It("injects using the pool named by inject-java:<name>, with no lightrun.com/agent-type annotation at all (defaults to java)", func() {
			ns := uniqueName("v3-trigger-ns")
			newNamespace(ns)
			secretName := uniqueName("v3-trigger-secret")
			newSecretIn(ns, secretName)
			poolName := uniqueName("v3-trigger-pool")
			newAgentPoolIn(ns, poolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: secretName},
				ServerHostname:    "lightrun.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})

			podName := uniqueName("v3-trigger-pod")
			pod := newPodIn(ns, podName, map[string]string{
				v3AnnInjectJava: poolName,
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			Expect(created.Spec.InitContainers).To(HaveLen(1))
			app := containerByName(created.Spec.Containers, "app")
			Expect(app).NotTo(BeNil())
			val, ok := envValue(*app, "JAVA_TOOL_OPTIONS")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("-agentpath:" + sharedVolumeMountPath + "/agent/lightrun_agent.so"))
		})

		It("does not inject when neither the pod nor its namespace set lightrun.com/inject-java", func() {
			ns := uniqueName("v3-notrigger-ns")
			newNamespace(ns)
			podName := uniqueName("v3-notrigger-pod")
			pod := newPodIn(ns, podName, nil, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(BeEmpty())
		})

		It("does not inject when lightrun.com/agent-type is overridden to an unimplemented value, even though inject-java names a valid pool", func() {
			ns := uniqueName("v3-agenttype-ns")
			newNamespace(ns)
			secretName := uniqueName("v3-agenttype-secret")
			newSecretIn(ns, secretName)
			poolName := uniqueName("v3-agenttype-pool")
			newAgentPoolIn(ns, poolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: secretName},
				ServerHostname:    "lightrun.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "NODE_OPTIONS",
			})

			podName := uniqueName("v3-agenttype-pod")
			pod := newPodIn(ns, podName, map[string]string{
				v3AnnInjectJava: poolName,
				annAgentType:    "nodejs",
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(BeEmpty())
		})
	})

	// --- v3 item 1: namespace-level lightrun.com/inject-java default. ---
	Context("namespace-level lightrun.com/inject-java default", func() {
		It("injects a pod with no inject-java annotation of its own, using the namespace's default pool", func() {
			ns := uniqueName("v3-nsdefault")
			secretName := uniqueName("v3-nsdefault-secret")
			poolName := uniqueName("v3-nsdefault-pool")

			newNamespaceWithAnnotations(ns, map[string]string{v3AnnInjectJava: poolName})
			newSecretIn(ns, secretName)
			newAgentPoolIn(ns, poolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: secretName},
				ServerHostname:    "lightrun.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})

			podName := uniqueName("v3-nsdefault-pod")
			pod := newPodIn(ns, podName, nil, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(HaveLen(1))
		})

		It("lets a pod override the namespace default with its own inject-java: \"false\"", func() {
			ns := uniqueName("v3-nsdefault-off")
			secretName := uniqueName("v3-nsdefault-off-secret")
			poolName := uniqueName("v3-nsdefault-off-pool")

			newNamespaceWithAnnotations(ns, map[string]string{v3AnnInjectJava: poolName})
			newSecretIn(ns, secretName)
			newAgentPoolIn(ns, poolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: secretName},
				ServerHostname:    "lightrun.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})

			podName := uniqueName("v3-nsdefault-off-pod")
			pod := newPodIn(ns, podName, map[string]string{v3AnnInjectJava: "false"}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(BeEmpty())
		})

		It("lets a pod override the namespace default with a different pool name", func() {
			ns := uniqueName("v3-nsdefault-swap")
			defaultPoolName := uniqueName("v3-nsdefault-swap-defaultpool")
			otherPoolName := uniqueName("v3-nsdefault-swap-otherpool")
			defaultSecret := uniqueName("v3-nsdefault-swap-defaultsecret")
			otherSecret := uniqueName("v3-nsdefault-swap-othersecret")

			newNamespaceWithAnnotations(ns, map[string]string{v3AnnInjectJava: defaultPoolName})
			newSecretIn(ns, defaultSecret)
			newSecretIn(ns, otherSecret)
			newAgentPoolIn(ns, defaultPoolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: defaultSecret},
				ServerHostname:    "default.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})
			newAgentPoolIn(ns, otherPoolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: otherSecret},
				ServerHostname:    "other.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})

			podName := uniqueName("v3-nsdefault-swap-pod")
			pod := newPodIn(ns, podName, map[string]string{v3AnnInjectJava: otherPoolName}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(HaveLen(1))
			val, ok := envValue(created.Spec.InitContainers[0], "LIGHTRUN_SERVER")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("other.example.com"), "pod-level inject-java override should win over the namespace default pool")
		})

		It("still lets the pod's own legacy-style (agent-type+agent-pool) trigger win over a namespace-level inject-java default pointing at a different pool", func() {
			// Covers the "HIGH" audit finding on decideInjection's precedence: a
			// namespace admin setting a lightrun.com/inject-java default must never
			// silently redirect a pod that already opted in via its own EXPLICIT
			// legacy-style trigger (lightrun.com/agent-type + lightrun.com/agent-pool)
			// to a pool the admin controls, without the workload owner's consent.
			ns := uniqueName("v3-legacy-vs-nsdefault")
			nsDefaultPoolName := uniqueName("v3-legacy-vs-nsdefault-nspool")
			legacyPoolName := uniqueName("v3-legacy-vs-nsdefault-legacypool")
			nsDefaultSecret := uniqueName("v3-legacy-vs-nsdefault-nssecret")
			legacySecret := uniqueName("v3-legacy-vs-nsdefault-legacysecret")

			newNamespaceWithAnnotations(ns, map[string]string{v3AnnInjectJava: nsDefaultPoolName})
			newSecretIn(ns, nsDefaultSecret)
			newSecretIn(ns, legacySecret)
			newAgentPoolIn(ns, nsDefaultPoolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: nsDefaultSecret},
				ServerHostname:    "ns-default.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})
			newAgentPoolIn(ns, legacyPoolName, agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: legacySecret},
				ServerHostname:    "legacy.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})

			podName := uniqueName("v3-legacy-vs-nsdefault-pod")
			pod := newPodIn(ns, podName, map[string]string{
				annAgentType: "java",
				annAgentPool: legacyPoolName,
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			Expect(created.Spec.InitContainers).To(HaveLen(1))
			val, ok := envValue(created.Spec.InitContainers[0], "LIGHTRUN_SERVER")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("legacy.example.com"), "the pod's own legacy-style trigger must win over the namespace-level inject-java default")
		})
	})

	// --- v3 item 1: lightrun.com/inject-java: "true" resolves the pool literally named
	//     "default" (AgentPool first, then ClusterAgentPool). ---
	Context("lightrun.com/inject-java: \"true\" resolves the pool named \"default\"", func() {
		It("injects using the namespaced AgentPool named \"default\" when it exists", func() {
			ns := uniqueName("v3-truedefault")
			newNamespace(ns)
			secretName := uniqueName("v3-truedefault-secret")
			newSecretIn(ns, secretName)
			newAgentPoolIn(ns, "default", agentsv1beta.AgentPoolSpec{
				SecretRef:         agentsv1beta.AgentPoolSecretRef{Name: secretName},
				ServerHostname:    "lightrun.example.com",
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})

			podName := uniqueName("v3-truedefault-pod")
			pod := newPodIn(ns, podName, map[string]string{v3AnnInjectJava: "true"}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(HaveLen(1))
		})

		It("denies the pod when inject-java: \"true\" and no pool named \"default\" exists (namespaced or cluster-scoped)", func() {
			ns := uniqueName("v3-truemissing")
			newNamespace(ns)
			podName := uniqueName("v3-truemissing-pod")
			pod := newPodIn(ns, podName, map[string]string{v3AnnInjectJava: "true"}, []corev1.Container{containerNamed("app")})

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("default"))
		})
	})

	// --- v3 item 2: shared config moves into the pool CRDs, with per-field pod annotation
	//     overrides applied on top -- proving per-field (not all-or-nothing) resolution by
	//     overriding exactly one field per spec and asserting a *different* field still falls
	//     through to the pool default. ---
	Context("per-field pod annotation overrides on top of pool defaults", func() {
		var ns, secretName, poolName string

		BeforeEach(func() {
			ns = uniqueName("v3-fields-ns")
			newNamespace(ns)
			secretName = uniqueName("v3-fields-secret")
			newSecretIn(ns, secretName)
			poolName = uniqueName("v3-fields-pool")
			newAgentPoolIn(ns, poolName, agentsv1beta.AgentPoolSpec{
				SecretRef:                agentsv1beta.AgentPoolSecretRef{Name: secretName},
				ServerHostname:           "lightrun.example.com",
				ContainerSelector:        []string{"app"},
				AgentEnvVarName:          "JAVA_TOOL_OPTIONS",
				AgentCliFlags:            "--pool-flag",
				AgentTags:                []string{"pool-tag"},
				AgentName:                "pool-agent",
				AgentConfig:              map[string]string{"pool_key": "pool_val"},
				UseSecretsAsMountedFiles: boolPtr(true),
			})
		})

		It("resolves every field from the pool when the pod overrides none of them", func() {
			podName := uniqueName("v3-fields-pod-none")
			pod := newPodIn(ns, podName, map[string]string{v3AnnInjectJava: poolName}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			app := containerByName(created.Spec.Containers, "app")
			Expect(app).NotTo(BeNil())
			val, ok := envValue(*app, "JAVA_TOOL_OPTIONS")
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal("-agentpath:" + sharedVolumeMountPath + "/agent/lightrun_agent.so=--pool-flag"))

			Expect(created.Spec.InitContainers).To(HaveLen(1))
			init := created.Spec.InitContainers[0]
			tags, ok := envValue(init, "LIGHTRUN_AGENT_TAGS")
			Expect(ok).To(BeTrue())
			Expect(tags).To(Equal("pool-tag"))
			name, ok := envValue(init, "LIGHTRUN_AGENT_NAME")
			Expect(ok).To(BeTrue())
			Expect(name).To(Equal("pool-agent"))
		})

		It("lets the pod override just agent-tags while agent-name still falls through to the pool default", func() {
			podName := uniqueName("v3-fields-pod-tags")
			pod := newPodIn(ns, podName, map[string]string{
				v3AnnInjectJava: poolName,
				annAgentTags:    "pod-tag-override",
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(HaveLen(1))
			init := created.Spec.InitContainers[0]

			tags, ok := envValue(init, "LIGHTRUN_AGENT_TAGS")
			Expect(ok).To(BeTrue())
			Expect(tags).To(Equal("pod-tag-override"), "the pod's own agent-tags annotation must win over the pool default")

			name, ok := envValue(init, "LIGHTRUN_AGENT_NAME")
			Expect(ok).To(BeTrue())
			Expect(name).To(Equal("pool-agent"), "agent-name must still fall through to the pool default since the pod didn't override it")
		})

		It("lets the pod override just container-selector while agent-env-var-name still falls through to the pool default", func() {
			podName := uniqueName("v3-fields-pod-selector")
			pod := newPodIn(ns, podName, map[string]string{
				v3AnnInjectJava: poolName,
				annContainerSel: "sidecar",
			}, []corev1.Container{containerNamed("app"), containerNamed("sidecar")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			app := containerByName(created.Spec.Containers, "app")
			Expect(app).NotTo(BeNil())
			Expect(volumeMount(*app, sharedVolumeName)).To(BeNil(), "container-selector override to 'sidecar' means 'app' must NOT be patched")

			sidecar := containerByName(created.Spec.Containers, "sidecar")
			Expect(sidecar).NotTo(BeNil())
			Expect(volumeMount(*sidecar, sharedVolumeName)).NotTo(BeNil(), "container-selector override to 'sidecar' means 'sidecar' must be patched")
			val, ok := envValue(*sidecar, "JAVA_TOOL_OPTIONS")
			Expect(ok).To(BeTrue(), "agent-env-var-name must still fall through to the pool default (JAVA_TOOL_OPTIONS) since the pod didn't override it")
			Expect(val).To(ContainSubstring("-agentpath:"))
		})

		It("lets the pod override just use-secrets-as-mounted-files while agent-name still falls through to the pool default", func() {
			podName := uniqueName("v3-fields-pod-mounted")
			pod := newPodIn(ns, podName, map[string]string{
				v3AnnInjectJava:    poolName,
				annUseMountedFiles: "false", // pool default is true (mounted)
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())

			for _, v := range created.Spec.Volumes {
				Expect(v.Name).NotTo(Equal(secretVolumeName), "use-secrets-as-mounted-files:false override means no secret volume should be mounted")
			}

			Expect(created.Spec.InitContainers).To(HaveLen(1))
			init := created.Spec.InitContainers[0]
			name, ok := envValue(init, "LIGHTRUN_AGENT_NAME")
			Expect(ok).To(BeTrue())
			Expect(name).To(Equal("pool-agent"), "agent-name must still fall through to the pool default since the pod didn't override it")
		})
	})

	// --- v3 item 3: ClusterAgentPool.spec.allowedNamespaces gate. ---
	Context("ClusterAgentPool allowedNamespaces gate", func() {
		It("denies the pod when its namespace is not in the ClusterAgentPool's allowedNamespaces", func() {
			sourceNs := uniqueName("v3-cap-source")
			newNamespace(sourceNs)
			sourceSecret := uniqueName("v3-cap-secret")
			newSecretIn(sourceNs, sourceSecret)

			allowedNs := uniqueName("v3-cap-allowed")
			newNamespace(allowedNs)
			deniedNs := uniqueName("v3-cap-denied")
			newNamespace(deniedNs)

			poolName := uniqueName("v3-cap-pool")
			newClusterAgentPool(poolName, agentsv1beta.ClusterAgentPoolSpec{
				SecretRef:         agentsv1beta.ClusterAgentPoolSecretRef{Name: sourceSecret, Namespace: sourceNs},
				ServerHostname:    "lightrun.example.com",
				AllowedNamespaces: []string{allowedNs},
				ContainerSelector: []string{"app"},
				AgentEnvVarName:   "JAVA_TOOL_OPTIONS",
			})

			podName := uniqueName("v3-cap-denied-pod")
			pod := newPodIn(deniedNs, podName, map[string]string{
				v3AnnInjectJava:    poolName,
				v3AnnAgentPoolKind: "ClusterAgentPool",
			}, []corev1.Container{containerNamed("app")})

			err := k8sClient.Create(ctx, pod)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not permitted"))
			Expect(err.Error()).To(ContainSubstring(deniedNs))
		})

		It("admits a pod in an allowed namespace, wiring the LOCAL mirrored secret rather than the cross-namespace source", func() {
			sourceNs := uniqueName("v3-cap-ok-source")
			newNamespace(sourceNs)
			sourceSecret := uniqueName("v3-cap-ok-secret")
			newSecretIn(sourceNs, sourceSecret)

			allowedNs := uniqueName("v3-cap-ok-allowed")
			newNamespace(allowedNs)

			poolName := uniqueName("v3-cap-ok-pool")
			newClusterAgentPool(poolName, agentsv1beta.ClusterAgentPoolSpec{
				SecretRef:                agentsv1beta.ClusterAgentPoolSecretRef{Name: sourceSecret, Namespace: sourceNs},
				ServerHostname:           "lightrun.example.com",
				AllowedNamespaces:        []string{allowedNs},
				ContainerSelector:        []string{"app"},
				AgentEnvVarName:          "JAVA_TOOL_OPTIONS",
				UseSecretsAsMountedFiles: boolPtr(false), // simplest to assert env-from-secret wiring below
			})

			podName := uniqueName("v3-cap-ok-pod")
			pod := newPodIn(allowedNs, podName, map[string]string{
				v3AnnInjectJava:    poolName,
				v3AnnAgentPoolKind: "ClusterAgentPool",
			}, []corev1.Container{containerNamed("app")})

			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			var created corev1.Pod
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), &created)).To(Succeed())
			Expect(created.Spec.InitContainers).To(HaveLen(1))
			init := created.Spec.InitContainers[0]

			var keyEnv *corev1.EnvVar
			for i := range init.Env {
				if init.Env[i].Name == "LIGHTRUN_KEY" {
					keyEnv = &init.Env[i]
				}
			}
			Expect(keyEnv).NotTo(BeNil())
			Expect(keyEnv.ValueFrom).NotTo(BeNil())
			Expect(keyEnv.ValueFrom.SecretKeyRef).NotTo(BeNil())

			referencedSecretName := keyEnv.ValueFrom.SecretKeyRef.Name
			Expect(referencedSecretName).NotTo(Equal(sourceSecret), "must reference a LOCAL mirrored secret, never the cross-namespace source Secret directly (core v1 SecretKeySelector can't cross namespaces anyway, but the resolved name must not merely coincide with the source name either)")
			Expect(referencedSecretName).To(ContainSubstring(poolName), "the mirrored secret name should be deterministically derived from the ClusterAgentPool's name, per the design doc's '<clusterAgentPool-name>-mirror' example")
		})
	})
})
