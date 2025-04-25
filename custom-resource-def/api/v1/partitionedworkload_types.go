package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PartitionedWorkloadSpec defines the desired state of PartitionedWorkload
type PartitionedWorkloadSpec struct {
	// Number of Pod replicas
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas"`

	// Simplified Pod template
	PodTemplate PodTemplateSpec `json:"podTemplate"`

	// Number of Pods to remain on the old version during upgrade
	// +kubebuilder:validation:Minimum=0
	PartitionCount int32 `json:"partitionCount"`
}

// PodTemplateSpec defines a simplified Pod template
type PodTemplateSpec struct {
	Containers    []corev1.Container   `json:"containers"`
	Volumes       []corev1.Volume      `json:"volumes,omitempty"`
	Affinity      *corev1.Affinity     `json:"affinity,omitempty"`
	NodeSelector  map[string]string    `json:"nodeSelector,omitempty"`
	RestartPolicy corev1.RestartPolicy `json:"restartPolicy,omitempty"`
}

// PartitionedWorkloadStatus defines the observed state of PartitionedWorkload
type PartitionedWorkloadStatus struct {
	// Number of available replicas
	AvailableReplicas int32 `json:"availableReplicas"`

	// Number of replicas being upgraded
	UpgradingReplicas int32 `json:"upgradingReplicas"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PartitionedWorkload is the Schema for the partitionedworkloads API
type PartitionedWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PartitionedWorkloadSpec   `json:"spec,omitempty"`
	Status PartitionedWorkloadStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PartitionedWorkloadList contains a list of PartitionedWorkload
type PartitionedWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PartitionedWorkload `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PartitionedWorkload{}, &PartitionedWorkloadList{})
}
