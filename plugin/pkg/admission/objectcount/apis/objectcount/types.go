package objectcount

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Configuration provides configuration for the ObjectCount admission controller.
type Configuration struct {
	metav1.TypeMeta `json:",inline"`

	// RefreshIntervalSeconds is the time interval that the admission controller
	// refreshes its object count cache. If unspecified, 60s will be the default value.
	RefreshIntervalSeconds *int64 `json:"refreshIntervalSeconds"`

	// DefaultLimit, if specified, will be used for any resources that can't be
	// found in the limits map.
	DefaultLimit *int64 `json:"defaultLimit"`

	// Limits is a map from resource name (e.g. ingresses.networking.k8s.io) to the limit.
	Limits map[string]int64 `json:"limits"`
}
