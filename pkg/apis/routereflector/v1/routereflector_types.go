package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RouteReflectorSpec defines the desired state of RouteReflector
type RouteReflectorSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "operator-sdk generate k8s" to regenerate code after modifying this file
	// Add custom validation using kubebuilder tags: https://book-v1.book.kubebuilder.io/beyond_basics/generating_crd.html
}

// RouteReflectorStatus defines the observed state of RouteReflector
type RouteReflectorStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "operator-sdk generate k8s" to regenerate code after modifying this file
	// Add custom validation using kubebuilder tags: https://book-v1.book.kubebuilder.io/beyond_basics/generating_crd.html
	RouteReflectors []string `json:"routeReflectors"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RouteReflector is the Schema for the routereflectors API
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=routereflectors,scope=Namespaced
type RouteReflector struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RouteReflectorSpec   `json:"spec,omitempty"`
	Status RouteReflectorStatus `json:"status,omitempty"`

	PreferredPool string   `json:"preferredPool"`
	IgnoredPools  []string `json:"ignoredPools"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RouteReflectorList contains a list of RouteReflector
type RouteReflectorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RouteReflector `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RouteReflector{}, &RouteReflectorList{})
}
