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
//
// PLACEHOLDER (2026-07-15, "DESIGN CHANGE v3" section #2): the shared-default fields below
// (ContainerSelector..UseSecretsAsMountedFiles) are new, added by tester to support
// RED-phase test coverage for hierarchical config resolution (pool defaults + per-field pod
// annotation overrides). Field names/types/kubebuilder markers are tester's best-effort
// placeholder guess at the final shape and are coder's to finalize -- in particular the
// exact zero-value semantics for UseSecretsAsMountedFiles (nil vs. false must be
// distinguishable so "pod annotation absent" can correctly fall through to "pool didn't set
// it either" vs. "pool explicitly set false"), and whether AgentConfig should be
// map[string]string here or something else. ClusterAgentPoolSpec in
// clusteragentpool_types.go mirrors these same fields and should be kept in sync.

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

	// PLACEHOLDER (DESIGN CHANGE v3 #2): shared defaults for fields that used to be
	// required per-pod annotations. A pod's own lightrun.com/* annotation (if present)
	// overrides the corresponding field here, per-field, not all-or-nothing.

	// ContainerSelector lists the default container names to patch when the consuming pod
	// does not override lightrun.com/container-selector.
	// +optional
	ContainerSelector []string `json:"containerSelector,omitempty"`

	// AgentEnvVarName is the default env var (e.g. JAVA_TOOL_OPTIONS) the agentpath arg is
	// appended to when the consuming pod does not override lightrun.com/agent-env-var-name.
	// +optional
	AgentEnvVarName string `json:"agentEnvVarName,omitempty"`

	// AgentCliFlags is the default value used when the consuming pod does not override
	// lightrun.com/agent-cli-flags.
	// +optional
	AgentCliFlags string `json:"agentCliFlags,omitempty"`

	// AgentTags is the default value used when the consuming pod does not override
	// lightrun.com/agent-tags.
	// +optional
	AgentTags []string `json:"agentTags,omitempty"`

	// AgentName is the default value used when the consuming pod does not override
	// lightrun.com/agent-name.
	// +optional
	AgentName string `json:"agentName,omitempty"`

	// AgentConfig is the default value used when the consuming pod does not override
	// lightrun.com/agent-config.
	// +optional
	AgentConfig map[string]string `json:"agentConfig,omitempty"`

	// UseSecretsAsMountedFiles is the default value used when the consuming pod does not
	// override lightrun.com/use-secrets-as-mounted-files. A *bool (rather than bool) so
	// "unset on the pool" is distinguishable from "explicitly set to false" -- resolution
	// falls through to the webhook's own built-in default (true) only when both the pod
	// annotation is absent AND this is nil.
	// +optional
	UseSecretsAsMountedFiles *bool `json:"useSecretsAsMountedFiles,omitempty"`
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
