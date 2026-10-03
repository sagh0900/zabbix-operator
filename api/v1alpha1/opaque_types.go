/*
Copyright The Zabbix Operator Authors.

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

package v1alpha1

import (
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
)

// The types in this file wrap large Kubernetes types so the CRD stores them without an
// OpenAPI schema. Each wrapper has only an unexported field, which makes controller-gen
// emit an open object for it; JSON round-trips through the wrapped type unchanged. The
// API server validates the values when the operator creates pods from them.

// Container is a corev1.Container stored without a schema.
// +kubebuilder:validation:Type=object
// +kubebuilder:pruning:PreserveUnknownFields
// +kubebuilder:object:generate=false
type Container struct{ v corev1.Container }

// ContainerOf wraps c.
func ContainerOf(c corev1.Container) Container { return Container{v: c} }

// Get returns the wrapped container.
func (c Container) Get() corev1.Container { return c.v }

// MarshalJSON encodes the wrapped container.
func (c Container) MarshalJSON() ([]byte, error) { return json.Marshal(c.v) }

// UnmarshalJSON decodes into the wrapped container.
func (c *Container) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &c.v) }

// DeepCopyInto copies c into out.
func (c *Container) DeepCopyInto(out *Container) { c.v.DeepCopyInto(&out.v) }

// Volume is a corev1.Volume stored without a schema.
// +kubebuilder:validation:Type=object
// +kubebuilder:pruning:PreserveUnknownFields
// +kubebuilder:object:generate=false
type Volume struct{ v corev1.Volume }

// VolumeOf wraps v.
func VolumeOf(v corev1.Volume) Volume { return Volume{v: v} }

// Get returns the wrapped volume.
func (v Volume) Get() corev1.Volume { return v.v }

// MarshalJSON encodes the wrapped volume.
func (v Volume) MarshalJSON() ([]byte, error) { return json.Marshal(v.v) }

// UnmarshalJSON decodes into the wrapped volume.
func (v *Volume) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &v.v) }

// DeepCopyInto copies v into out.
func (v *Volume) DeepCopyInto(out *Volume) { v.v.DeepCopyInto(&out.v) }

// TopologySpreadConstraint is a corev1.TopologySpreadConstraint stored without a schema.
// +kubebuilder:validation:Type=object
// +kubebuilder:pruning:PreserveUnknownFields
// +kubebuilder:object:generate=false
type TopologySpreadConstraint struct {
	v corev1.TopologySpreadConstraint
}

// TopologySpreadConstraintOf wraps t.
func TopologySpreadConstraintOf(t corev1.TopologySpreadConstraint) TopologySpreadConstraint {
	return TopologySpreadConstraint{v: t}
}

// Get returns the wrapped constraint.
func (t TopologySpreadConstraint) Get() corev1.TopologySpreadConstraint { return t.v }

// MarshalJSON encodes the wrapped constraint.
func (t TopologySpreadConstraint) MarshalJSON() ([]byte, error) { return json.Marshal(t.v) }

// UnmarshalJSON decodes into the wrapped constraint.
func (t *TopologySpreadConstraint) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &t.v) }

// DeepCopyInto copies t into out.
func (t *TopologySpreadConstraint) DeepCopyInto(out *TopologySpreadConstraint) {
	t.v.DeepCopyInto(&out.v)
}
