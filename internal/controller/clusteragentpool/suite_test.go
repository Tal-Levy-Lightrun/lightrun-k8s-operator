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

// Package clusteragentpool contains the RED-phase envtest coverage (and, once coder
// implements it, the reconciler itself) for the ClusterAgentPool secret-mirroring controller
// described in "DESIGN CHANGE v3" section #3 of
// .dot-agent-deck/webhook-refactor-context.md (2026-07-15). Deliberately kept in its own
// package/test binary (tester's placement call) rather than folded into the existing
// internal/controller suite: that suite's manager restricts its cache to a single fixed
// testNamespace (see internal/controller/suite_test.go), which is incompatible with a
// controller that must watch/write Secrets across an arbitrary, CR-driven set of namespaces
// (ClusterAgentPool.spec.allowedNamespaces). Keeping this isolated also means this package's
// compile failure (SecretMirrorReconciler does not exist yet -- expected RED) does not block
// the currently-passing internal/controller suite from running.
package clusteragentpool

import (
	"context"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestClusterAgentPoolSecretMirror(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ClusterAgentPool secret-mirroring controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
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

	// Deliberately no Cache.DefaultNamespaces restriction: this controller must watch/write
	// Secrets across whatever namespaces a ClusterAgentPool's allowedNamespaces names, which
	// isn't known until the CR is read, so a fixed namespace allow-list at manager-startup
	// time doesn't fit this controller the way it does LightrunJavaAgentReconciler's.
	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).ToNot(HaveOccurred())

	// This is the controller under test -- not implemented yet (RED phase). Expected
	// placeholder shape (coder's to finalize): a Reconciler watching ClusterAgentPool
	// objects and their source Secret, mirroring create/update/delete into every namespace
	// listed in spec.allowedNamespaces. See
	// clusteragentpool_secret_mirror_controller_test.go for the full behavioral contract.
	err = (&SecretMirrorReconciler{
		Client: k8sManager.GetClient(),
		Scheme: k8sManager.GetScheme(),
		Log:    k8sManager.GetLogger(),
	}).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "failed to run manager")
	}()
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

// newNamespace creates (if absent) a Namespace named name, ignoring AlreadyExists so specs
// can share a small set of namespaces across the suite.
func newNamespace(name string) {
	ns := &corev1.Namespace{}
	ns.Name = name
	err := k8sClient.Create(ctx, ns)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}
