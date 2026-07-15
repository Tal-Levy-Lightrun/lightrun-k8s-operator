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

// Package clusteragentpool implements SecretMirrorReconciler, the dedicated controller that
// mirrors a ClusterAgentPool's source Secret into every namespace listed in
// spec.allowedNamespaces, per "DESIGN CHANGE v3" section 3 of
// .dot-agent-deck/webhook-refactor-context.md (2026-07-15). Deliberately kept out of the
// webhook's hot admission path (which only ever *references* the local mirrored Secret by
// name, never writes one) so the broad Secret create/update/delete privilege this controller
// needs stays isolated to a single, narrowly-scoped reconciler.
package clusteragentpool

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	agentsv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

// mirrorConditionType is set on ClusterAgentPool.status.conditions to surface the outcome of
// the most recent mirroring reconcile -- in particular, per the "CRITICAL" audit finding, to
// make a refused Secret-name collision (see mirrorInto) visible via `kubectl describe` /
// status rather than only via a returned reconcile error (which is also still returned, so it
// shows up in logs/events/metrics too).
const mirrorConditionType = "SecretMirrorSynced"

// SecretMirrorReconciler watches ClusterAgentPool objects and their source Secret (per
// spec.secretRef.name/.namespace), mirroring a copy of that Secret's data into every namespace
// listed in spec.allowedNamespaces under a deterministic name
// (api/v1beta.ClusterAgentPoolMirrorSecretName), labeled with
// api/v1beta.ClusterAgentPoolMirrorLabelKey to identify it as a managed mirror of that specific
// ClusterAgentPool, with an ownerReference to the ClusterAgentPool for GC.
//
// This reconciler must ONLY ever create/update/delete Secrets carrying
// api/v1beta.ClusterAgentPoolMirrorLabelKey -- it never reads or touches any other Secret in
// the cluster (the source Secret it reads, per spec.secretRef, is read-only from its
// perspective). It never adopts/overwrites a pre-existing Secret at a mirror's deterministic
// name unless that Secret already carries its own management label (see mirrorInto).
//
// APIReader, MirrorCache and Recorder are optional (nil-safe): when unset, Reconcile/
// SetupWithManager fall back to using Client for everything and to an unscoped
// Watches(&corev1.Secret{}, ...) registration, exactly matching this controller's original
// (pre-audit-fix) behavior -- this keeps the existing envtest suite (which constructs this
// reconciler with only Client/Scheme/Log set) working unchanged. Production wiring
// (cmd/main.go) sets all three for the tightened-scope behavior described below.
type SecretMirrorReconciler struct {
	client.Client
	// APIReader is a direct (uncached, never watching) reader used for the one-off read of an
	// arbitrary, unlabeled source Secret named by spec.secretRef -- deliberately never routed
	// through MirrorCache, whose Transform (see cmd/main.go's wiring) scrubs Data for anything
	// without api/v1beta.ClusterAgentPoolMirrorLabelKey.
	APIReader client.Reader
	// MirrorCache is a dedicated cache (separate from the manager's shared cache used by
	// mgr.GetClient(), so it never affects other controllers' Secret reads) used both as this
	// controller's own Secret watch source and for Get/List of managed mirror Secrets. Its
	// Transform (configured by the caller, see cmd/main.go) scrubs .Data/.StringData for any
	// Secret not carrying the mirror label, so this controller's own contribution to the
	// manager's in-memory Secret footprint is limited to the mirrors it actually manages, per
	// the "scope the Secret cache/watch" audit finding -- existence/labels (which is all the
	// ownership check and stale-mirror pruning below need) are still visible for every Secret,
	// since detecting an arbitrary source Secret's rotation requires observing it.
	MirrorCache cache.Cache
	Scheme      *runtime.Scheme
	// Recorder emits a Warning Event on the ClusterAgentPool when mirroring hits a problem
	// (e.g. the name-collision refusal below), alongside the status condition and the
	// returned reconcile error.
	Recorder record.EventRecorder
	Log      logr.Logger
}

// reader returns the Secret reader mirrorInto/pruneStaleMirrors should use: MirrorCache when
// configured (production), Client otherwise (test fallback -- see the struct doc comment).
func (r *SecretMirrorReconciler) reader() client.Reader {
	if r.MirrorCache != nil {
		return r.MirrorCache
	}
	return r.Client
}

// sourceReader returns the reader used for the one-off read of the arbitrary source Secret:
// APIReader when configured (production -- always uncached/ground-truth), Client otherwise
// (test fallback).
func (r *SecretMirrorReconciler) sourceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

//+kubebuilder:rbac:groups=agents.lightrun.com,resources=clusteragentpools,verbs=get;list;watch
//+kubebuilder:rbac:groups=agents.lightrun.com,resources=clusteragentpools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile mirrors req's ClusterAgentPool's source Secret into every namespace listed in its
// spec.allowedNamespaces, and prunes any previously-mirrored Secret whose namespace has since
// been removed from that list.
func (r *SecretMirrorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("clusterAgentPool", req.Name)

	var pool agentsv1beta.ClusterAgentPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if pool.Spec.SecretRef.Name == "" || pool.Spec.SecretRef.Namespace == "" {
		log.Info("ClusterAgentPool has no secretRef configured yet, nothing to mirror")
		return ctrl.Result{}, nil
	}

	var source corev1.Secret
	sourceKey := client.ObjectKey{Namespace: pool.Spec.SecretRef.Namespace, Name: pool.Spec.SecretRef.Name}
	if err := r.sourceReader().Get(ctx, sourceKey, &source); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("source Secret not found yet", "secret", sourceKey)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading source Secret %s: %w", sourceKey, err)
	}

	mirrorName := agentsv1beta.ClusterAgentPoolMirrorSecretName(pool.Name)
	var firstErr error
	for _, ns := range pool.Spec.AllowedNamespaces {
		if err := r.mirrorInto(ctx, &pool, &source, ns, mirrorName); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("mirroring into namespace %q: %w", ns, err)
		}
	}

	if err := r.pruneStaleMirrors(ctx, &pool, mirrorName); err != nil && firstErr == nil {
		firstErr = err
	}

	if statusErr := r.updateMirrorCondition(ctx, &pool, firstErr); statusErr != nil {
		log.Error(statusErr, "updating ClusterAgentPool status condition")
	}

	if firstErr != nil {
		if r.Recorder != nil {
			r.Recorder.Event(&pool, corev1.EventTypeWarning, "SecretMirrorError", firstErr.Error())
		}
		return ctrl.Result{}, firstErr
	}

	return ctrl.Result{}, nil
}

// mirrorInto creates or updates the mirrored Secret named name in namespace so its data
// matches source's, labeled and owned by pool.
//
// Per the "CRITICAL" audit finding: before mutating anything, it Gets whatever currently
// exists at that name and refuses to adopt/overwrite it unless it's either absent or already
// carries this exact pool's own management label. Without this check, an unrelated Secret that
// happens to collide with the deterministic "<pool>-mirror" name would be silently overwritten
// AND owner-reference-adopted -- meaning it would later be deleted via GC cascade when the
// ClusterAgentPool is deleted (cross-tenant data loss). On a collision, mirrorInto errors out
// instead (surfaced by the caller as a per-namespace skip -- see Reconcile's firstErr handling
// -- plus a status condition and Event, not a silent clobber).
func (r *SecretMirrorReconciler) mirrorInto(ctx context.Context, pool *agentsv1beta.ClusterAgentPool, source *corev1.Secret, namespace, name string) error {
	var existing corev1.Secret
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing)
	switch {
	case err == nil:
		if existing.Labels[agentsv1beta.ClusterAgentPoolMirrorLabelKey] != pool.Name {
			return fmt.Errorf(
				"refusing to adopt/overwrite Secret %s/%s: it already exists but is not managed by ClusterAgentPool %q "+
					"(missing or mismatched %q label) -- likely an unrelated Secret that collides with this pool's "+
					"deterministic mirror name",
				namespace, name, pool.Name, agentsv1beta.ClusterAgentPoolMirrorLabelKey,
			)
		}
	case apierrors.IsNotFound(err):
		// Nothing at this name yet -- safe to create.
	default:
		return fmt.Errorf("checking for a pre-existing Secret %s/%s: %w", namespace, name, err)
	}

	mirror := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, mirror, func() error {
		if mirror.Labels == nil {
			mirror.Labels = map[string]string{}
		}
		mirror.Labels[agentsv1beta.ClusterAgentPoolMirrorLabelKey] = pool.Name
		mirror.Type = source.Type
		mirror.Data = source.Data
		return controllerutil.SetOwnerReference(pool, mirror, r.Scheme)
	})
	return err
}

// pruneStaleMirrors deletes every Secret labeled as a mirror of pool (via
// api/v1beta.ClusterAgentPoolMirrorLabelKey) whose namespace is no longer listed in
// pool.Spec.AllowedNamespaces -- per the "MEDIUM" audit finding, a namespace removed from
// allowedNamespaces must have its stale mirrored credential revoked, not left behind
// indefinitely.
func (r *SecretMirrorReconciler) pruneStaleMirrors(ctx context.Context, pool *agentsv1beta.ClusterAgentPool, mirrorName string) error {
	var mirrors corev1.SecretList
	if err := r.reader().List(ctx, &mirrors, client.MatchingLabels{agentsv1beta.ClusterAgentPoolMirrorLabelKey: pool.Name}); err != nil {
		return fmt.Errorf("listing existing mirrors for ClusterAgentPool %q: %w", pool.Name, err)
	}

	allowed := make(map[string]bool, len(pool.Spec.AllowedNamespaces))
	for _, ns := range pool.Spec.AllowedNamespaces {
		allowed[ns] = true
	}

	var firstErr error
	for i := range mirrors.Items {
		mirror := &mirrors.Items[i]
		if allowed[mirror.Namespace] && mirror.Name == mirrorName {
			continue
		}
		if err := r.Delete(ctx, mirror); err != nil && !apierrors.IsNotFound(err) && firstErr == nil {
			firstErr = fmt.Errorf("deleting stale mirror %s/%s: %w", mirror.Namespace, mirror.Name, err)
		}
	}
	return firstErr
}

// updateMirrorCondition sets mirrorConditionType on pool's status to reflect the outcome of the
// mirroring attempt just performed (mirrorErr, nil on full success) -- per the "CRITICAL"
// finding's ask for "a clear condition/event so the mismatch is visible, rather than silently
// clobbering someone else's object", surfaced here for any mirroring failure, not only
// name-collisions.
func (r *SecretMirrorReconciler) updateMirrorCondition(ctx context.Context, pool *agentsv1beta.ClusterAgentPool, mirrorErr error) error {
	cond := metav1.Condition{
		Type:               mirrorConditionType,
		Status:             metav1.ConditionTrue,
		Reason:             "MirroringSucceeded",
		Message:            "all allowedNamespaces are mirrored and no stale mirrors remain",
		ObservedGeneration: pool.Generation,
	}
	if mirrorErr != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "MirroringFailed"
		cond.Message = mirrorErr.Error()
	}

	if !meta.SetStatusCondition(&pool.Status.Conditions, cond) {
		return nil
	}
	return r.Status().Update(ctx, pool)
}

// mapSecretToClusterAgentPools enqueues a reconcile request for every ClusterAgentPool whose
// spec.secretRef points at the Secret obj -- so mirrors pick up source Secret rotations.
func (r *SecretMirrorReconciler) mapSecretToClusterAgentPools(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	var pools agentsv1beta.ClusterAgentPoolList
	if err := r.List(ctx, &pools); err != nil {
		r.Log.Error(err, "listing ClusterAgentPools to map Secret change")
		return nil
	}

	var requests []reconcile.Request
	for i := range pools.Items {
		pool := &pools.Items[i]
		if pool.Spec.SecretRef.Name == secret.Name && pool.Spec.SecretRef.Namespace == secret.Namespace {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: pool.Name}})
		}
	}
	return requests
}

// SetupWithManager wires the reconciler onto mgr, watching ClusterAgentPool objects directly
// and their source Secret (via mapSecretToClusterAgentPools) so credential rotation is picked
// up without waiting for the ClusterAgentPool itself to change.
//
// When r.MirrorCache is set (production wiring, see cmd/main.go), the Secret watch is sourced
// from it instead of the manager's shared cache, per the "scope the Secret cache/watch" audit
// finding -- see the struct doc comment for MirrorCache and the reader()/sourceReader() helpers
// for the full rationale. Falls back to an ordinary, manager-cache-backed Watches(...) when
// MirrorCache is nil (test fallback).
func (r *SecretMirrorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	bldr := ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1beta.ClusterAgentPool{})

	if r.MirrorCache != nil {
		bldr = bldr.WatchesRawSource(
			source.Kind(r.MirrorCache, &corev1.Secret{}),
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToClusterAgentPools),
		)
	} else {
		bldr = bldr.Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToClusterAgentPools),
		)
	}

	return bldr.Complete(r)
}
