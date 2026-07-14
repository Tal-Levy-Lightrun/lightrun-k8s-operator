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

// AgentPool CRD per the "DESIGN CHANGE" section of
// .dot-agent-deck/webhook-refactor-context.md (2026-07-14): SecretRef.Name is a Secret in the
// *same namespace* as the AgentPool, and ServerHostname mirrors the old per-pod
// lightrun.com/server-hostname annotation value.

// AgentPoolSecretRef references a Secret holding the Lightrun agent-pool credentials
// (lightrun_key / pinned_cert_hash keys, same as today). The Secret must live in the same
// namespace as the AgentPool.
type AgentPoolSecretRef struct {
	// Name of the Secret in the same namespace as this AgentPool.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AgentPoolSpec defines the desired state of AgentPool.
type AgentPoolSpec struct {
	// SecretRef points at the Secret holding this pool's Lightrun credentials.
	SecretRef AgentPoolSecretRef `json:"secretRef"`

	// ServerHostname is the Lightrun server hostname used for downloading the agent; the
	// key/company id in SecretRef's Secret must originate from this same server.
	// +kubebuilder:validation:MinLength=1
	ServerHostname string `json:"serverHostname"`
}

// AgentPoolStatus defines the observed state of AgentPool.
type AgentPoolStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="ServerHostname",type=string,JSONPath=".spec.serverHostname"
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentPool is the Schema for the agentpools API. It is namespace-scoped: pods reference
// one by name (via the lightrun.com/agent-pool annotation) within their own namespace.
type AgentPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentPoolSpec   `json:"spec,omitempty"`
	Status AgentPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// AgentPoolList contains a list of AgentPool.
type AgentPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentPool{}, &AgentPoolList{})
}
