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

package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"

	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/go-logr/logr"
	agentv1beta "github.com/lightrun-platform/lightrun-k8s-operator/api/v1beta"
)

// nodeFieldManager identifies SSA-applied fields as owned by the Node reconciler, kept distinct
// from Java's field manager so a workload could in principle be patched by both reconcilers
// without their server-side-apply ownership colliding.
const nodeFieldManager = "lightrun-node-controller"

// LightrunNodeAgentReconciler reconciles a LightrunNodeAgent object
type LightrunNodeAgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=agents.lightrun.com,resources=lightrunnodeagents,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=agents.lightrun.com,resources=lightrunnodeagents/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=agents.lightrun.com,resources=lightrunnodeagents/finalizers,verbs=update

func (r *LightrunNodeAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("lightrunNodeAgent", req.NamespacedName)
	lightrunNodeAgent := &agentv1beta.LightrunNodeAgent{}
	if err := r.Get(ctx, req.NamespacedName, lightrunNodeAgent); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Determine which workload type to reconcile
	workloadType, err := r.determineWorkloadType(lightrunNodeAgent)
	if err != nil {
		log.Error(err, "failed to determine workload type")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}
	switch workloadType {
	case agentv1beta.WorkloadTypeDeployment:
		return r.reconcileDeployment(ctx, lightrunNodeAgent, req.Namespace)
	case agentv1beta.WorkloadTypeStatefulSet:
		return r.reconcileStatefulSet(ctx, lightrunNodeAgent, req.Namespace)
	default:
		return r.errorStatus(ctx, lightrunNodeAgent, fmt.Errorf("unsupported workload type: %s", workloadType))
	}
}

func (r *LightrunNodeAgentReconciler) determineWorkloadType(lightrunNodeAgent *agentv1beta.LightrunNodeAgent) (agentv1beta.WorkloadType, error) {
	spec := lightrunNodeAgent.Spec
	if spec.WorkloadName == "" || spec.WorkloadType == "" {
		return "", errors.New("invalid configuration: workloadName and workloadType must be set")
	}
	return spec.WorkloadType, nil
}

// reconcileDeployment handles the reconciliation logic for Deployment workloads
func (r *LightrunNodeAgentReconciler) reconcileDeployment(ctx context.Context, lightrunNodeAgent *agentv1beta.LightrunNodeAgent, namespace string) (ctrl.Result, error) {
	deploymentName := lightrunNodeAgent.Spec.WorkloadName
	if deploymentName == "" {
		return r.errorStatus(ctx, lightrunNodeAgent, errors.New("unable to reconcile deployment: missing workloadName"))
	}
	log := r.Log.WithValues("lightrunNodeAgent", lightrunNodeAgent.Name, "deployment", deploymentName)

	deplNamespacedObj := client.ObjectKey{
		Name:      deploymentName,
		Namespace: namespace,
	}
	originalDeployment := &appsv1.Deployment{}
	err := r.Get(ctx, deplNamespacedObj, originalDeployment)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("Deployment not found. Verify name/namespace", "Deployment", deploymentName)
			err = r.removeFinalizer(ctx, lightrunNodeAgent, finalizerName)
			if err != nil {
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}
			return r.errorStatus(ctx, lightrunNodeAgent, errors.New("deployment not found: "+deploymentName))
		}
		log.Error(err, "unable to fetch deployment")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	if oldLrnaName, ok := originalDeployment.Annotations[nodeAnnotationAgentName]; ok && oldLrnaName != lightrunNodeAgent.Name {
		log.Error(err, "Deployment already patched by LightrunNodeAgent", "Existing LightrunNodeAgent", oldLrnaName)
		return r.errorStatus(ctx, lightrunNodeAgent, errors.New("deployment already patched: "+deploymentName))
	}

	deploymentApplyConfig, err := appsv1ac.ExtractDeployment(originalDeployment, nodeFieldManager)
	if err != nil {
		log.Error(err, "failed to extract Deployment")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	var secret *corev1.Secret

	if lightrunNodeAgent.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted

		log.V(2).Info("Searching for secret", "Name", lightrunNodeAgent.Spec.SecretName)
		secretNamespacedObj := client.ObjectKey{
			Name:      lightrunNodeAgent.Spec.SecretName,
			Namespace: namespace,
		}
		secret = &corev1.Secret{}
		err = r.Get(ctx, secretNamespacedObj, secret)
		if err != nil {
			if client.IgnoreNotFound(err) == nil {
				log.Error(err, "Secret not found", "Secret", lightrunNodeAgent.Spec.SecretName)
			}
			return r.errorStatus(ctx, lightrunNodeAgent, err)
		}

		if !containsString(lightrunNodeAgent.ObjectMeta.Finalizers, finalizerName) {
			log.Info("Start working on deployment", "Deployment", deploymentName)
			log.Info("Adding finalizer")
			err = r.addFinalizer(ctx, lightrunNodeAgent, finalizerName)
			if err != nil {
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}
		}
	} else {
		// The object is being deleted
		log.Info("LightrunNodeAgent is being deleted", "lightrunNodeAgent", lightrunNodeAgent.Name)
		if containsString(lightrunNodeAgent.ObjectMeta.Finalizers, finalizerName) {
			log.Info("Unpatching deployment", "Deployment", originalDeployment.Name)

			clientSidePatch := client.MergeFrom(originalDeployment.DeepCopy())
			for i, container := range originalDeployment.Spec.Template.Spec.Containers {
				for _, targetContainer := range lightrunNodeAgent.Spec.ContainerSelector {
					if targetContainer == container.Name {
						r.unpatchNodeOptionsEnv(originalDeployment.Annotations, &originalDeployment.Spec.Template.Spec.Containers[i])
					}
				}
			}
			delete(originalDeployment.Annotations, nodeAnnotationPatchedEnvName)
			delete(originalDeployment.Annotations, nodeAnnotationPatchedEnvValue)
			err = r.Patch(ctx, originalDeployment, clientSidePatch)
			if err != nil {
				log.Error(err, "unable to unpatch "+lightrunNodeAgent.Spec.AgentEnvVarName)
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}

			// Remove Volumes, init container and CLI-flags env (all SSA-owned by this field manager)
			emptyApplyConfig := appsv1ac.Deployment(deplNamespacedObj.Name, deplNamespacedObj.Namespace)
			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(emptyApplyConfig)
			if err != nil {
				log.Error(err, "failed to convert Deployment to unstructured")
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}
			patch := &unstructured.Unstructured{
				Object: obj,
			}
			err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
				FieldManager: nodeFieldManager,
				Force:        pointer.Bool(true),
			})
			if err != nil {
				log.Error(err, "failed to unpatch deployment")
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}

			log.Info("Removing finalizer")
			err = r.removeFinalizer(ctx, lightrunNodeAgent, finalizerName)
			if err != nil {
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}

			log.Info("Deployment returned to original state", "Deployment", deploymentName)
			return r.successStatus(ctx, lightrunNodeAgent, reconcileTypeProgressing)
		}
		return r.successStatus(ctx, lightrunNodeAgent, reconcileTypeProgressing)
	}

	// Verify that env var won't exceed sanity length cap
	agentArg, err := nodeAgentEnvVarArgument(lightrunNodeAgent.Spec.InitContainer.SharedVolumeMountPath)
	if err != nil {
		log.Error(err, "nodeAgentEnvVarArgument exceeds 1024 chars")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	log.V(2).Info("Reconciling config map with agent configuration")
	configMap, err := r.createAgentConfig(lightrunNodeAgent)
	if err != nil {
		log.Error(err, "unable to create configMap")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}
	applyOpts := []client.PatchOption{client.ForceOwnership, client.FieldOwner(nodeFieldManager)}

	err = r.Patch(ctx, &configMap, client.Apply, applyOpts...)
	if err != nil {
		log.Error(err, "unable to create configMap")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	cmNamespacedObj := client.ObjectKey{
		Name:      configMap.Name,
		Namespace: configMap.Namespace,
	}
	cm := &corev1.ConfigMap{}
	err = r.Get(ctx, cmNamespacedObj, cm)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Error(err, "ConfigMap not found", "CM", configMap.Name)
		}
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}
	cmDataHash := configMapDataHash(cm.Data)

	log.V(2).Info("Patching deployment, SSA", "Deployment", deploymentName, "LightrunNodeAgent", lightrunNodeAgent.Name)
	err = r.patchNodeDeployment(lightrunNodeAgent, secret, originalDeployment, deploymentApplyConfig, cmDataHash)
	if err != nil {
		log.Error(err, "unable to patch deployment")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deploymentApplyConfig)
	if err != nil {
		log.Error(err, "failed to convert Deployment to unstructured")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	patch := &unstructured.Unstructured{
		Object: obj,
	}

	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: nodeFieldManager,
		Force:        pointer.Bool(true),
	})
	if err != nil {
		log.Error(err, "failed to patch deployment")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	// Client side patch (we can't rollback NODE_OPTIONS env with server side apply)
	log.V(2).Info("Patching Node Env", "Deployment", deploymentName, "LightrunNodeAgent", lightrunNodeAgent.Name)
	originalDeployment = &appsv1.Deployment{}
	err = r.Get(ctx, deplNamespacedObj, originalDeployment)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("Deployment not found", "Deployment", deploymentName)
			err = r.removeFinalizer(ctx, lightrunNodeAgent, finalizerName)
			if err != nil {
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}
			return r.errorStatus(ctx, lightrunNodeAgent, errors.New("deployment not found"))
		}
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}
	clientSidePatch := client.MergeFrom(originalDeployment.DeepCopy())
	for i, container := range originalDeployment.Spec.Template.Spec.Containers {
		for _, targetContainer := range lightrunNodeAgent.Spec.ContainerSelector {
			if targetContainer == container.Name {
				err = r.patchNodeOptionsEnv(originalDeployment.Annotations, &originalDeployment.Spec.Template.Spec.Containers[i], lightrunNodeAgent.Spec.AgentEnvVarName, agentArg)
				if err != nil {
					log.Error(err, "failed to patch "+lightrunNodeAgent.Spec.AgentEnvVarName)
					return r.errorStatus(ctx, lightrunNodeAgent, err)
				}
			}
		}
	}
	originalDeployment.Annotations[nodeAnnotationPatchedEnvName] = lightrunNodeAgent.Spec.AgentEnvVarName
	originalDeployment.Annotations[nodeAnnotationPatchedEnvValue] = agentArg
	err = r.Patch(ctx, originalDeployment, clientSidePatch)
	if err != nil {
		log.Error(err, "failed to patch "+lightrunNodeAgent.Spec.AgentEnvVarName)
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	log.V(1).Info("Reconciling finished successfully", "Deployment", deploymentName, "LightrunNodeAgent", lightrunNodeAgent.Name)
	return r.successStatus(ctx, lightrunNodeAgent, reconcileTypeReady)
}

// reconcileStatefulSet handles the reconciliation logic for StatefulSet workloads
func (r *LightrunNodeAgentReconciler) reconcileStatefulSet(ctx context.Context, lightrunNodeAgent *agentv1beta.LightrunNodeAgent, namespace string) (ctrl.Result, error) {
	log := r.Log.WithValues("lightrunNodeAgent", lightrunNodeAgent.Name, "statefulSet", lightrunNodeAgent.Spec.WorkloadName)
	statefulSetName := lightrunNodeAgent.Spec.WorkloadName
	if statefulSetName == "" {
		return r.errorStatus(ctx, lightrunNodeAgent, errors.New("unable to reconcile statefulset: missing workloadName field"))
	}
	stsNamespacedObj := client.ObjectKey{
		Name:      lightrunNodeAgent.Spec.WorkloadName,
		Namespace: namespace,
	}
	originalStatefulSet := &appsv1.StatefulSet{}
	err := r.Get(ctx, stsNamespacedObj, originalStatefulSet)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("StatefulSet not found. Verify name/namespace", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName)
			err = r.removeFinalizer(ctx, lightrunNodeAgent, finalizerName)
			if err != nil {
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}
			return r.errorStatus(ctx, lightrunNodeAgent, errors.New("statefulset not found: "+statefulSetName))
		}
		log.Error(err, "unable to fetch statefulset")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	if !lightrunNodeAgent.ObjectMeta.DeletionTimestamp.IsZero() {
		if containsString(lightrunNodeAgent.ObjectMeta.Finalizers, finalizerName) {
			log.Info("Unpatching StatefulSet", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName)

			originalStatefulSet = &appsv1.StatefulSet{}
			err = r.Get(ctx, stsNamespacedObj, originalStatefulSet)
			if err != nil {
				if client.IgnoreNotFound(err) == nil {
					log.Info("StatefulSet not found", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName)
					log.Info("Removing finalizer")
					err = r.removeFinalizer(ctx, lightrunNodeAgent, finalizerName)
					if err != nil {
						return r.errorStatus(ctx, lightrunNodeAgent, err)
					}
					return r.successStatus(ctx, lightrunNodeAgent, reconcileTypeReady)
				}
				log.Error(err, "unable to unpatch statefulset", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName)
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}

			clientSidePatch := client.MergeFrom(originalStatefulSet.DeepCopy())
			for i, container := range originalStatefulSet.Spec.Template.Spec.Containers {
				for _, targetContainer := range lightrunNodeAgent.Spec.ContainerSelector {
					if targetContainer == container.Name {
						r.unpatchNodeOptionsEnv(originalStatefulSet.Annotations, &originalStatefulSet.Spec.Template.Spec.Containers[i])
					}
				}
			}
			delete(originalStatefulSet.Annotations, nodeAnnotationPatchedEnvName)
			delete(originalStatefulSet.Annotations, nodeAnnotationPatchedEnvValue)
			delete(originalStatefulSet.Annotations, nodeAnnotationAgentName)
			err = r.Patch(ctx, originalStatefulSet, clientSidePatch)
			if err != nil {
				log.Error(err, "failed to unpatch statefulset environment variables")
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}

			emptyApplyConfig := appsv1ac.StatefulSet(stsNamespacedObj.Name, stsNamespacedObj.Namespace)
			obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(emptyApplyConfig)
			if err != nil {
				log.Error(err, "failed to convert StatefulSet to unstructured")
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}
			patch := &unstructured.Unstructured{
				Object: obj,
			}
			err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
				FieldManager: nodeFieldManager,
				Force:        pointer.Bool(true),
			})
			if err != nil {
				log.Error(err, "failed to unpatch statefulset")
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}

			log.Info("Removing finalizer")
			err = r.removeFinalizer(ctx, lightrunNodeAgent, finalizerName)
			if err != nil {
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}

			log.Info("StatefulSet returned to original state", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName)
			return r.successStatus(ctx, lightrunNodeAgent, reconcileTypeProgressing)
		}
		return r.successStatus(ctx, lightrunNodeAgent, reconcileTypeProgressing)
	}

	if oldLrnaName, ok := originalStatefulSet.Annotations[nodeAnnotationAgentName]; ok && oldLrnaName != lightrunNodeAgent.Name {
		log.Error(err, "StatefulSet already patched by LightrunNodeAgent", "Existing LightrunNodeAgent", oldLrnaName)
		return r.errorStatus(ctx, lightrunNodeAgent, errors.New("statefulset :"+statefulSetName+" already patched"))
	}

	if !containsString(lightrunNodeAgent.ObjectMeta.Finalizers, finalizerName) {
		log.V(2).Info("Adding finalizer")
		err = r.addFinalizer(ctx, lightrunNodeAgent, finalizerName)
		if err != nil {
			log.Error(err, "unable to add finalizer")
			return r.errorStatus(ctx, lightrunNodeAgent, err)
		}
	}

	secretObj := client.ObjectKey{
		Name:      lightrunNodeAgent.Spec.SecretName,
		Namespace: namespace,
	}
	secret := &corev1.Secret{}
	err = r.Get(ctx, secretObj, secret)
	if err != nil {
		log.Error(err, "unable to fetch Secret", "Secret", lightrunNodeAgent.Spec.SecretName)
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	agentArg, err := nodeAgentEnvVarArgument(lightrunNodeAgent.Spec.InitContainer.SharedVolumeMountPath)
	if err != nil {
		log.Error(err, "nodeAgentEnvVarArgument exceeds 1024 chars")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	log.V(2).Info("Reconciling config map with agent configuration")
	configMap, err := r.createAgentConfig(lightrunNodeAgent)
	if err != nil {
		log.Error(err, "unable to create configMap")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}
	applyOpts := []client.PatchOption{client.ForceOwnership, client.FieldOwner(nodeFieldManager)}

	err = r.Patch(ctx, &configMap, client.Apply, applyOpts...)
	if err != nil {
		log.Error(err, "unable to apply configMap")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	cmDataHash := configMapDataHash(configMap.Data)

	statefulSetApplyConfig, err := appsv1ac.ExtractStatefulSet(originalStatefulSet, nodeFieldManager)
	if err != nil {
		log.Error(err, "failed to extract StatefulSet")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	log.V(2).Info("Patching StatefulSet", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName, "LightrunNodeAgent", lightrunNodeAgent.Name)
	err = r.patchNodeStatefulSet(lightrunNodeAgent, secret, originalStatefulSet, statefulSetApplyConfig, cmDataHash)
	if err != nil {
		log.Error(err, "failed to patch statefulset")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(statefulSetApplyConfig)
	if err != nil {
		log.Error(err, "failed to convert StatefulSet to unstructured")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}
	patch := &unstructured.Unstructured{
		Object: obj,
	}
	err = r.Patch(ctx, patch, client.Apply, &client.PatchOptions{
		FieldManager: nodeFieldManager,
		Force:        pointer.Bool(true),
	})
	if err != nil {
		log.Error(err, "failed to patch statefulset")
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	log.V(2).Info("Patching Node Env", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName, "LightrunNodeAgent", lightrunNodeAgent.Name)
	originalStatefulSet = &appsv1.StatefulSet{}
	err = r.Get(ctx, stsNamespacedObj, originalStatefulSet)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			log.Info("StatefulSet not found", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName)
			err = r.removeFinalizer(ctx, lightrunNodeAgent, finalizerName)
			if err != nil {
				return r.errorStatus(ctx, lightrunNodeAgent, err)
			}
			return r.errorStatus(ctx, lightrunNodeAgent, errors.New("statefulset not found"))
		}
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}
	clientSidePatch := client.MergeFrom(originalStatefulSet.DeepCopy())
	for i, container := range originalStatefulSet.Spec.Template.Spec.Containers {
		for _, targetContainer := range lightrunNodeAgent.Spec.ContainerSelector {
			if targetContainer == container.Name {
				err = r.patchNodeOptionsEnv(originalStatefulSet.Annotations, &originalStatefulSet.Spec.Template.Spec.Containers[i], lightrunNodeAgent.Spec.AgentEnvVarName, agentArg)
				if err != nil {
					log.Error(err, "failed to patch "+lightrunNodeAgent.Spec.AgentEnvVarName)
					return r.errorStatus(ctx, lightrunNodeAgent, err)
				}
			}
		}
	}
	originalStatefulSet.Annotations[nodeAnnotationPatchedEnvName] = lightrunNodeAgent.Spec.AgentEnvVarName
	originalStatefulSet.Annotations[nodeAnnotationPatchedEnvValue] = agentArg
	err = r.Patch(ctx, originalStatefulSet, clientSidePatch)
	if err != nil {
		log.Error(err, "failed to patch "+lightrunNodeAgent.Spec.AgentEnvVarName)
		return r.errorStatus(ctx, lightrunNodeAgent, err)
	}

	log.V(1).Info("Reconciling finished successfully", "StatefulSet", lightrunNodeAgent.Spec.WorkloadName, "LightrunNodeAgent", lightrunNodeAgent.Name)
	return r.successStatus(ctx, lightrunNodeAgent, reconcileTypeReady)
}

// SetupWithManager configures the controller with the Manager and sets up watches and indexers.
// Reuses the same workloadNameIndexField/secretNameIndexField index names as the Java reconciler:
// field indexes are scoped per-GVK internally, so sharing the name across LightrunJavaAgent and
// LightrunNodeAgent is safe and does not collide.
func (r *LightrunNodeAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&agentv1beta.LightrunNodeAgent{},
		workloadNameIndexField,
		func(object client.Object) []string {
			agent := object.(*agentv1beta.LightrunNodeAgent)
			if agent.Spec.WorkloadName == "" {
				return nil
			}
			r.Log.Info("Indexing WorkloadName", "WorkloadName", agent.Spec.WorkloadName)
			return []string{agent.Spec.WorkloadName}
		})
	if err != nil {
		return err
	}

	err = mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&agentv1beta.LightrunNodeAgent{},
		secretNameIndexField,
		func(object client.Object) []string {
			lightrunNodeAgent := object.(*agentv1beta.LightrunNodeAgent)
			if lightrunNodeAgent.Spec.SecretName == "" {
				return nil
			}
			return []string{lightrunNodeAgent.Spec.SecretName}
		})
	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&agentv1beta.LightrunNodeAgent{}).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.mapDeploymentToNodeAgent),
		).
		Watches(
			&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(r.mapStatefulSetToNodeAgent),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.mapSecretToNodeAgent),
		).
		Complete(r)
}
