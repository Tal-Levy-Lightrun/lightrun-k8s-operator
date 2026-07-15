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

package clusteragentpool

// Covers "DESIGN CHANGE v3" section #3 of
// .dot-agent-deck/webhook-refactor-context.md (2026-07-15): a new, separate controller (not
// inline in the webhook's admission path) that watches ClusterAgentPool objects and their
// source Secret, and mirrors a copy of that Secret into every namespace listed in
// spec.allowedNamespaces, keeping it in sync on rotation, with an ownerReference to the
// ClusterAgentPool for GC.
//
// Discovery contract this suite assumes (tester's placeholder pick, coder's to finalize):
// each mirrored Secret carries the label mirrorPoolLabelKey=<ClusterAgentPool name>, which
// specs use to find it without assuming an exact name (the design doc suggests
// "<clusterAgentPool-name>-mirror" as an example naming scheme, not a hard requirement).

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

// mirrorPoolLabelKey is the label key specs use to find a namespace's mirrored Secret for a
// given ClusterAgentPool, without assuming coder's exact mirror-secret naming scheme.
const mirrorPoolLabelKey = "lightrun.com/cluster-agent-pool"

var nameCounter int

func uniqueName(prefix string) string {
	nameCounter++
	return fmt.Sprintf("%s-%d", prefix, nameCounter)
}

func findMirroredSecret(namespace, poolName string) (*corev1.Secret, error) {
	var list corev1.SecretList
	if err := k8sClient.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{mirrorPoolLabelKey: poolName}); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, nil
	}
	return &list.Items[0], nil
}

var _ = Describe("ClusterAgentPool secret-mirroring controller", func() {

	It("mirrors the source Secret into every namespace listed in allowedNamespaces", func() {
		sourceNs := uniqueName("mirror-src")
		newNamespace(sourceNs)
		allowedA := uniqueName("mirror-ns-a")
		newNamespace(allowedA)
		allowedB := uniqueName("mirror-ns-b")
		newNamespace(allowedB)

		sourceSecretName := uniqueName("mirror-source-secret")
		sourceSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sourceSecretName, Namespace: sourceNs},
			StringData: map[string]string{
				"lightrun_key":     "shhh-this-is-the-lightrun-key",
				"pinned_cert_hash": "deadbeefcafe",
			},
		}
		Expect(k8sClient.Create(ctx, sourceSecret)).To(Succeed())

		poolName := uniqueName("mirror-pool")
		pool := &agentsv1beta.ClusterAgentPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: agentsv1beta.ClusterAgentPoolSpec{
				SecretRef:         agentsv1beta.ClusterAgentPoolSecretRef{Name: sourceSecretName, Namespace: sourceNs},
				ServerHostname:    "lightrun.example.com",
				AllowedNamespaces: []string{allowedA, allowedB},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		for _, ns := range []string{allowedA, allowedB} {
			Eventually(func() (*corev1.Secret, error) {
				return findMirroredSecret(ns, poolName)
			}).ShouldNot(BeNil(), "expected a mirrored Secret labeled %s=%s to appear in namespace %s", mirrorPoolLabelKey, poolName, ns)
		}

		mirrorA, err := findMirroredSecret(allowedA, poolName)
		Expect(err).NotTo(HaveOccurred())
		Expect(mirrorA).NotTo(BeNil())
		Expect(mirrorA.Data["lightrun_key"]).To(Equal([]byte("shhh-this-is-the-lightrun-key")))
		Expect(mirrorA.Data["pinned_cert_hash"]).To(Equal([]byte("deadbeefcafe")))

		mirrorB, err := findMirroredSecret(allowedB, poolName)
		Expect(err).NotTo(HaveOccurred())
		Expect(mirrorB).NotTo(BeNil())
		Expect(mirrorB.Data).To(Equal(mirrorA.Data), "both mirrors must match the source Secret's data")
	})

	It("updates the mirrors when the source Secret's data changes", func() {
		sourceNs := uniqueName("mirror-update-src")
		newNamespace(sourceNs)
		allowedNs := uniqueName("mirror-update-ns")
		newNamespace(allowedNs)

		sourceSecretName := uniqueName("mirror-update-secret")
		sourceSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sourceSecretName, Namespace: sourceNs},
			StringData: map[string]string{"lightrun_key": "v1-key", "pinned_cert_hash": "v1-cert"},
		}
		Expect(k8sClient.Create(ctx, sourceSecret)).To(Succeed())

		poolName := uniqueName("mirror-update-pool")
		pool := &agentsv1beta.ClusterAgentPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: agentsv1beta.ClusterAgentPoolSpec{
				SecretRef:         agentsv1beta.ClusterAgentPoolSecretRef{Name: sourceSecretName, Namespace: sourceNs},
				ServerHostname:    "lightrun.example.com",
				AllowedNamespaces: []string{allowedNs},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		Eventually(func() (*corev1.Secret, error) {
			return findMirroredSecret(allowedNs, poolName)
		}).ShouldNot(BeNil())

		var toUpdate corev1.Secret
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sourceSecret), &toUpdate)).To(Succeed())
		toUpdate.Data = map[string][]byte{"lightrun_key": []byte("v2-key"), "pinned_cert_hash": []byte("v2-cert")}
		Expect(k8sClient.Update(ctx, &toUpdate)).To(Succeed())

		Eventually(func() ([]byte, error) {
			mirror, err := findMirroredSecret(allowedNs, poolName)
			if err != nil || mirror == nil {
				return nil, err
			}
			return mirror.Data["lightrun_key"], nil
		}).Should(Equal([]byte("v2-key")), "mirror must pick up the source Secret's rotated data")
	})

	It("sets an ownerReference to the ClusterAgentPool on each mirrored Secret (so real-cluster GC would clean it up on delete)", func() {
		// NOTE / documented limitation (per task instructions): envtest does not run
		// kube-controller-manager's garbage collector, so deleting the ClusterAgentPool
		// here will NOT actually cascade-delete the mirrored Secret in this suite. This
		// spec instead asserts the ownerReference itself is correct (Kind/Name/UID,
		// cluster-scoped owner of a namespaced object, which Kubernetes GC does support),
		// which is the part that makes real-cluster GC work. Live k3d validation is the
		// right place to prove the actual cascade-delete end-to-end.
		sourceNs := uniqueName("mirror-owner-src")
		newNamespace(sourceNs)
		allowedNs := uniqueName("mirror-owner-ns")
		newNamespace(allowedNs)

		sourceSecretName := uniqueName("mirror-owner-secret")
		sourceSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sourceSecretName, Namespace: sourceNs},
			StringData: map[string]string{"lightrun_key": "owner-key", "pinned_cert_hash": "owner-cert"},
		}
		Expect(k8sClient.Create(ctx, sourceSecret)).To(Succeed())

		poolName := uniqueName("mirror-owner-pool")
		pool := &agentsv1beta.ClusterAgentPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: agentsv1beta.ClusterAgentPoolSpec{
				SecretRef:         agentsv1beta.ClusterAgentPoolSecretRef{Name: sourceSecretName, Namespace: sourceNs},
				ServerHostname:    "lightrun.example.com",
				AllowedNamespaces: []string{allowedNs},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		var mirror *corev1.Secret
		Eventually(func() (*corev1.Secret, error) {
			var err error
			mirror, err = findMirroredSecret(allowedNs, poolName)
			return mirror, err
		}).ShouldNot(BeNil())

		var owned bool
		for _, ref := range mirror.OwnerReferences {
			if ref.Kind == "ClusterAgentPool" && ref.Name == poolName && ref.UID == pool.UID {
				owned = true
			}
		}
		Expect(owned).To(BeTrue(), "mirrored Secret must carry an ownerReference to its ClusterAgentPool (Kind=ClusterAgentPool, Name=%s, matching UID) for real-cluster GC on ClusterAgentPool delete", poolName)
	})

	It("refuses to adopt/overwrite a pre-existing Secret at the mirror's deterministic name that lacks this pool's management label, surfacing a status condition instead of clobbering it", func() {
		// Covers the "CRITICAL" audit finding on mirrorInto (secret_mirror_controller.go):
		// an unrelated Secret that happens to collide with the deterministic
		// "<clusterAgentPool-name>-mirror" name must never be silently overwritten and
		// owner-reference-adopted (which would later GC-cascade-delete someone else's
		// Secret when the ClusterAgentPool is deleted).
		sourceNs := uniqueName("mirror-collision-src")
		newNamespace(sourceNs)
		allowedNs := uniqueName("mirror-collision-allowed")
		newNamespace(allowedNs)

		sourceSecretName := uniqueName("mirror-collision-secret")
		sourceSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sourceSecretName, Namespace: sourceNs},
			StringData: map[string]string{"lightrun_key": "should-never-land-here", "pinned_cert_hash": "should-never-land-here"},
		}
		Expect(k8sClient.Create(ctx, sourceSecret)).To(Succeed())

		poolName := uniqueName("mirror-collision-pool")
		mirrorName := agentsv1beta.ClusterAgentPoolMirrorSecretName(poolName)

		// Pre-create an unrelated Secret at the mirror's deterministic name, in the allowed
		// namespace, WITHOUT the management label -- simulating a pre-existing, unrelated
		// object that just happens to collide with the naming scheme.
		unrelated := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: mirrorName, Namespace: allowedNs},
			StringData: map[string]string{"totally": "unrelated-data"},
		}
		Expect(k8sClient.Create(ctx, unrelated)).To(Succeed())

		pool := &agentsv1beta.ClusterAgentPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: agentsv1beta.ClusterAgentPoolSpec{
				SecretRef:         agentsv1beta.ClusterAgentPoolSecretRef{Name: sourceSecretName, Namespace: sourceNs},
				ServerHostname:    "lightrun.example.com",
				AllowedNamespaces: []string{allowedNs},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		Eventually(func() (metav1.ConditionStatus, error) {
			var got agentsv1beta.ClusterAgentPool
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &got); err != nil {
				return "", err
			}
			for _, cond := range got.Status.Conditions {
				if cond.Type == mirrorConditionType {
					return cond.Status, nil
				}
			}
			return "", nil
		}).Should(Equal(metav1.ConditionFalse), "expected a SecretMirrorSynced=False condition surfacing the name-collision refusal")

		var got agentsv1beta.ClusterAgentPool
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &got)).To(Succeed())
		var cond *metav1.Condition
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == mirrorConditionType {
				cond = &got.Status.Conditions[i]
			}
		}
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("MirroringFailed"))
		Expect(cond.Message).To(ContainSubstring("refusing to adopt/overwrite"))

		Consistently(func() (map[string][]byte, error) {
			var s corev1.Secret
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: allowedNs, Name: mirrorName}, &s); err != nil {
				return nil, err
			}
			return s.Data, nil
		}).Should(HaveKeyWithValue("totally", []byte("unrelated-data")), "the pre-existing unrelated Secret's data must never be overwritten")

		var stillUnrelated corev1.Secret
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: allowedNs, Name: mirrorName}, &stillUnrelated)).To(Succeed())
		Expect(stillUnrelated.Labels[agentsv1beta.ClusterAgentPoolMirrorLabelKey]).To(BeEmpty(), "the unrelated Secret must not be adopted (labeled) by this ClusterAgentPool")
		Expect(stillUnrelated.OwnerReferences).To(BeEmpty(), "the unrelated Secret must not gain an ownerReference (which would later GC-cascade-delete it on ClusterAgentPool deletion)")
	})

	It("never mirrors into a namespace that isn't listed in allowedNamespaces", func() {
		sourceNs := uniqueName("mirror-scope-src")
		newNamespace(sourceNs)
		allowedNs := uniqueName("mirror-scope-allowed")
		newNamespace(allowedNs)
		notAllowedNs := uniqueName("mirror-scope-not-allowed")
		newNamespace(notAllowedNs)

		sourceSecretName := uniqueName("mirror-scope-secret")
		sourceSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sourceSecretName, Namespace: sourceNs},
			StringData: map[string]string{"lightrun_key": "scope-key", "pinned_cert_hash": "scope-cert"},
		}
		Expect(k8sClient.Create(ctx, sourceSecret)).To(Succeed())

		poolName := uniqueName("mirror-scope-pool")
		pool := &agentsv1beta.ClusterAgentPool{
			ObjectMeta: metav1.ObjectMeta{Name: poolName},
			Spec: agentsv1beta.ClusterAgentPoolSpec{
				SecretRef:         agentsv1beta.ClusterAgentPoolSecretRef{Name: sourceSecretName, Namespace: sourceNs},
				ServerHostname:    "lightrun.example.com",
				AllowedNamespaces: []string{allowedNs},
			},
		}
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())

		Eventually(func() (*corev1.Secret, error) {
			return findMirroredSecret(allowedNs, poolName)
		}).ShouldNot(BeNil())

		Consistently(func() (*corev1.Secret, error) {
			return findMirroredSecret(notAllowedNs, poolName)
		}).Should(BeNil(), "must never mirror into a namespace absent from allowedNamespaces")
	})
})
