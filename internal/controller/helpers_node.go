package controller

import (
	"context"

	agentv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func (r *LightrunNodeAgentReconciler) mapDeploymentToNodeAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	deployment := obj.(*appsv1.Deployment)
	var agents agentv1beta.LightrunNodeAgentList
	if err := r.List(ctx, &agents,
		client.InNamespace(deployment.Namespace),
		client.MatchingFields{
			workloadNameIndexField: deployment.Name,
		},
	); err != nil {
		r.Log.Error(err, "failed to list by workloadNameIndexField")
		return nil
	}

	requests := make([]reconcile.Request, len(agents.Items))
	for i, a := range agents.Items {
		requests[i] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&a)}
	}
	return requests
}

func (r *LightrunNodeAgentReconciler) mapStatefulSetToNodeAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	statefulSet := obj.(*appsv1.StatefulSet)

	var agents agentv1beta.LightrunNodeAgentList

	if err := r.List(ctx, &agents,
		client.InNamespace(statefulSet.Namespace),
		client.MatchingFields{workloadNameIndexField: statefulSet.Name},
	); err != nil {
		r.Log.Error(err, "could not list LightrunNodeAgentList. "+
			"change to statefulset will not be reconciled.",
			statefulSet.Name, statefulSet.Namespace)
		return nil
	}

	requests := make([]reconcile.Request, len(agents.Items))

	for i, agent := range agents.Items {
		requests[i] = reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&agent),
		}
	}
	return requests
}

func (r *LightrunNodeAgentReconciler) mapSecretToNodeAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	secret := obj.(*corev1.Secret)

	var lightrunNodeAgentList agentv1beta.LightrunNodeAgentList

	if err := r.List(ctx, &lightrunNodeAgentList,
		client.InNamespace(secret.Namespace),
		client.MatchingFields{secretNameIndexField: secret.Name},
	); err != nil {
		r.Log.Error(err, "could not list LightrunNodeAgentList. "+
			"change to secret will not be reconciled.",
			secret.Name, secret.Namespace)
		return nil
	}

	requests := make([]reconcile.Request, len(lightrunNodeAgentList.Items))

	for i, lightrunNodeAgent := range lightrunNodeAgentList.Items {
		requests[i] = reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&lightrunNodeAgent),
		}
	}
	return requests
}

func (r *LightrunNodeAgentReconciler) addFinalizer(ctx context.Context, lightrunNodeAgent *agentv1beta.LightrunNodeAgent, finalizerName string) error {
	patch := client.MergeFrom(lightrunNodeAgent.DeepCopy())
	lightrunNodeAgent.ObjectMeta.Finalizers = append(lightrunNodeAgent.ObjectMeta.Finalizers, finalizerName)
	return r.Patch(ctx, lightrunNodeAgent, patch)
}

func (r *LightrunNodeAgentReconciler) removeFinalizer(ctx context.Context, lightrunNodeAgent *agentv1beta.LightrunNodeAgent, finalizerName string) error {
	patch := client.MergeFrom(lightrunNodeAgent.DeepCopy())
	lightrunNodeAgent.ObjectMeta.Finalizers = removeString(lightrunNodeAgent.ObjectMeta.Finalizers, finalizerName)
	return r.Patch(ctx, lightrunNodeAgent, patch)
}

func (r *LightrunNodeAgentReconciler) successStatus(ctx context.Context, instance *agentv1beta.LightrunNodeAgent, reconcileType string) (reconcile.Result, error) {
	condition := metav1.Condition{
		Type:               reconcileType,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: instance.GetGeneration(),
		Reason:             "reconcileSucceeded",
		Status:             metav1.ConditionTrue,
	}
	SetStatusCondition(&instance.Status.Conditions, condition)
	instance.Status.WorkloadStatus = r.findLastConditionType(&instance.Status.Conditions)
	err := r.Status().Update(ctx, instance)
	if err != nil {
		if apierrors.IsConflict(err) {
			r.Log.V(2).Info("unable to update status for", "object version", instance.GetResourceVersion(), "resource version expired, will trigger another reconcile cycle", "")
			return reconcile.Result{Requeue: true}, nil
		}
		r.Log.Error(err, "unable to update status for", "object", instance)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *LightrunNodeAgentReconciler) errorStatus(ctx context.Context, instance *agentv1beta.LightrunNodeAgent, origError error) (reconcile.Result, error) {
	condition := metav1.Condition{
		Type:               reconcileTypeNotProgressing,
		LastTransitionTime: metav1.Now(),
		Message:            origError.Error(),
		ObservedGeneration: instance.GetGeneration(),
		Reason:             "reconcileFailed",
		Status:             metav1.ConditionTrue,
	}
	SetStatusCondition(&instance.Status.Conditions, condition)
	instance.Status.WorkloadStatus = r.findLastConditionType(&instance.Status.Conditions)
	err := r.Status().Update(ctx, instance)
	if err != nil {
		if apierrors.IsConflict(err) {
			r.Log.Info("unable to update status for", "object version", instance.GetResourceVersion(), "resource version expired, will trigger another reconcile cycle", "")
			return reconcile.Result{Requeue: true}, nil
		}
		r.Log.Error(err, "unable to update status for", "object", instance)
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, origError
}

func (r *LightrunNodeAgentReconciler) findLastConditionType(conditions *[]metav1.Condition) string {
	index := -1
	var ts metav1.Time
	for i, cond := range *conditions {
		if index == -1 {
			index = i
			ts = cond.LastTransitionTime
			continue
		}
		if ts.Before(&cond.LastTransitionTime) {
			ts = cond.LastTransitionTime
			index = i
		}
	}
	return (*conditions)[index].Type
}
