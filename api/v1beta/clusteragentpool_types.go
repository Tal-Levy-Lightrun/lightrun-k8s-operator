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

package v1beta

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PLACEHOLDER (2026-07-15, "DESIGN CHANGE v3" section #3 of
// .dot-agent-deck/webhook-refactor-context.md): ClusterAgentPool is cluster-scoped and
// reintroduces a cross-namespace pool, this time with the cross-namespace secret problem
// solved by a dedicated mirroring controller (not yet implemented -- see
// internal/controller/clusteragentpool/, added by tester as a RED-phase placeholder package)
// rather than by the webhook mounting the source Secret directly (impossible cross-namespace).
// Authored by tester as a best-effort placeholder for coder to finalize: field names, JSON
// tags, and kubebuilder markers below are tester's guess at the final shape, not a locked
// contract.

// ClusterAgentPoolSecretRef identifies the source Secret a ClusterAgentPool's credentials are
// mirrored from -- unlike AgentPoolSecretRef, Namespace is required since the source Secret
// does NOT live alongside the (cluster-scoped) ClusterAgentPool.
type ClusterAgentPoolSecretRef struct {
	// Name of the source Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace the source Secret lives in (e.g. the operator's own namespace, or a
	// platform-team namespace) -- never a consuming pod's namespace directly.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// ClusterAgentPoolSpec defines the desired state of ClusterAgentPool.
type ClusterAgentPoolSpec struct {
	// SecretRef points at the source Secret this pool's credentials are mirrored from.
	SecretRef ClusterAgentPoolSecretRef `json:"secretRef"`

	// ServerHostname is the Lightrun server hostname used for downloading the agent.
	// +kubebuilder:validation:MinLength=1
	ServerHostname string `json:"serverHostname"`

	// AllowedNamespaces is the explicit allow-list of namespaces permitted to reference this
	// ClusterAgentPool. Secure by default: empty/absent means usable by nothing. The
	// mirroring controller also mirrors SecretRef's Secret into every namespace listed here.
	// +optional
	AllowedNamespaces []string `json:"allowedNamespaces,omitempty"`

	// The following mirror AgentPoolSpec's shared-default fields (see the placeholder note
	// in agentpool_types.go) -- kept in sync with AgentPoolSpec so the webhook's per-field
	// pod-annotation-override resolution logic can be shared across both pool kinds.

	// +optional
	ContainerSelector []string `json:"containerSelector,omitempty"`
	// +optional
	AgentEnvVarName string `json:"agentEnvVarName,omitempty"`
	// +optional
	AgentCliFlags string `json:"agentCliFlags,omitempty"`
	// +optional
	AgentTags []string `json:"agentTags,omitempty"`
	// +optional
	AgentName string `json:"agentName,omitempty"`
	// +optional
	AgentConfig map[string]string `json:"agentConfig,omitempty"`
	// +optional
	UseSecretsAsMountedFiles *bool `json:"useSecretsAsMountedFiles,omitempty"`
}

// ClusterAgentPoolStatus defines the observed state of ClusterAgentPool.
type ClusterAgentPoolStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster
//+kubebuilder:printcolumn:name="ServerHostname",type=string,JSONPath=".spec.serverHostname"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ClusterAgentPool is the Schema for the clusteragentpools API. It is cluster-scoped: pods in
// any namespace listed in spec.allowedNamespaces may reference it (via the
// lightrun.com/inject-java or lightrun.com/agent-pool annotation naming it, plus
// lightrun.com/agent-pool-kind: ClusterAgentPool).
type ClusterAgentPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterAgentPoolSpec   `json:"spec,omitempty"`
	Status ClusterAgentPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// ClusterAgentPoolList contains a list of ClusterAgentPool.
type ClusterAgentPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterAgentPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterAgentPool{}, &ClusterAgentPoolList{})
}

// ClusterAgentPoolMirrorLabelKey is set on every Secret the mirroring controller creates,
// identifying it as a managed mirror of the named ClusterAgentPool (the label's value). The
// controller only ever creates/updates/deletes Secrets carrying this label -- it must never
// touch any other Secret in the cluster.
const ClusterAgentPoolMirrorLabelKey = "lightrun.com/cluster-agent-pool"

// ClusterAgentPoolMirrorSecretName derives the deterministic name of the Secret mirrored into
// each of a ClusterAgentPool's allowedNamespaces, per the design doc's "<clusterAgentPool-name>
// -mirror" example. Shared by the mirroring controller (internal/controller/clusteragentpool)
// and the webhook's pool resolution (internal/webhook) so both sides stay in sync.
func ClusterAgentPoolMirrorSecretName(clusterAgentPoolName string) string {
	return clusterAgentPoolName + "-mirror"
}
