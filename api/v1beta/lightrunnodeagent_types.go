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

// Important: Run "make" to regenerate code after modifying this file
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LightrunNodeAgentSpec defines the desired state of LightrunNodeAgent
type LightrunNodeAgentSpec struct {
	// List of containers that should be patched in the Pod
	ContainerSelector []string      `json:"containerSelector"`
	InitContainer     InitContainer `json:"initContainer"`

	// Name of the Workload that will be patched. workload can be either Deployment or StatefulSet e.g. my-deployment, my-statefulset
	// +kubebuilder:validation:MinLength=1
	WorkloadName string `json:"workloadName"`

	// Type of the workload that will be patched supported values are Deployment, StatefulSet
	WorkloadType WorkloadType `json:"workloadType"`

	//Name of the Secret in the same namespace contains lightrun key and conmpany id
	SecretName string `json:"secretName"`

	//Env variable that will be patched with the --require bootstrap path
	//Defaults to NODE_OPTIONS
	AgentEnvVarName string `json:"agentEnvVarName"`

	// Lightrun server hostname that will be used for downloading an agent
	// Key and company id in the secret has to be taken from this server as well
	ServerHostname string `json:"serverHostname"`

	// Agent configuration to be changed from default values
	// +optional
	AgentConfig map[string]string `json:"agentConfig,omitempty"`

	// Add cli flags to the agent. Unlike Java, these are NOT concatenated into AgentEnvVarName;
	// they are passed to the app container via a separate LIGHTRUN_AGENT_CLI_FLAGS env var.
	// +optional
	AgentCliFlags string `json:"agentCliFlags,omitempty"`

	// Agent tags that will be shown in the portal / IDE plugin
	AgentTags []string `json:"agentTags"`

	// +optional
	// Agent name for registration to the server
	AgentName string `json:"agentName,omitempty"`

	// UseSecretsAsMountedFiles determines whether to use secret values as mounted files (true) or as environment variables (false)
	// +kubebuilder:default=true
	UseSecretsAsMountedFiles bool `json:"useSecretsAsMountedFiles,omitempty"`
}

// LightrunNodeAgentStatus defines the observed state of LightrunNodeAgent
type LightrunNodeAgentStatus struct {
	LastScheduleTime *metav1.Time       `json:"lastScheduleTime,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	WorkloadStatus   string             `json:"workloadStatus,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=lrna
//+kubebuilder:printcolumn:priority=0,name=Workload,type=string,JSONPath=".spec.workloadName",description="Workload name",format=""
//+kubebuilder:printcolumn:priority=0,name=Type,type=string,JSONPath=".spec.workloadType",description="Workload type",format=""
//+kubebuilder:printcolumn:priority=0,name="Status",type=string,JSONPath=".status.workloadStatus",description="Status of Workload Reconciliation",format=""
//+kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// LightrunNodeAgent is the Schema for the lightrunnodeagents API
type LightrunNodeAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LightrunNodeAgentSpec   `json:"spec,omitempty"`
	Status LightrunNodeAgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// LightrunNodeAgentList contains a list of LightrunNodeAgent
type LightrunNodeAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []LightrunNodeAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&LightrunNodeAgent{}, &LightrunNodeAgentList{})
}
