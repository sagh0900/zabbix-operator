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
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

// PodSettings are the pod-level settings every component accepts.
//
// Large Kubernetes types (volumes, extra containers, affinity, topology spread and
// security contexts) are stored without a schema to keep the CRD small enough for
// client-side apply; the API server validates them when the operator creates pods.
type PodSettings struct {
	// Image overrides the image derived from the system's version, image repository and
	// flavor.
	// +optional
	Image string `json:"image,omitempty"`

	// Resources of the main container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env adds environment variables to the main container. Variables the operator
	// manages cannot be overridden.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom adds environment variables from ConfigMaps or Secrets.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Volumes adds volumes to the pod, for example ConfigMaps with configuration files.
	// +optional
	Volumes []Volume `json:"volumes,omitempty"`

	// VolumeMounts adds mounts to the main container.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// ExtraContainers adds sidecar containers to the pod.
	// +optional
	ExtraContainers []Container `json:"extraContainers,omitempty"`

	// NodeSelector constrains the nodes pods are scheduled on.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations of the pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity of the pods.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// TopologySpreadConstraints of the pods.
	// +optional
	TopologySpreadConstraints []TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PodAnnotations are added to the pods.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// PodLabels are added to the pods. Labels the operator manages cannot be overridden.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// PriorityClassName of the pods.
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`

	// ServiceAccountName of the pods.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// ImagePullSecrets of the pods. The server's pull secrets also pull the operator image
	// for the operator's database Jobs.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// PodSecurityContext of the pods.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// SecurityContext of the main container.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`
}

// ServiceSettings shape the Service of a component.
type ServiceSettings struct {
	// Type of the Service.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`

	// Port the Service listens on. Defaults to the component's port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// NodePort for NodePort and LoadBalancer Services.
	// +optional
	NodePort int32 `json:"nodePort,omitempty"`

	// LoadBalancerIP requests a specific address from the load-balancer implementation.
	// +optional
	LoadBalancerIP string `json:"loadBalancerIP,omitempty"`

	// LoadBalancerClass selects the load-balancer implementation.
	// +optional
	LoadBalancerClass *string `json:"loadBalancerClass,omitempty"`

	// LoadBalancerSourceRanges restricts client addresses.
	// +optional
	LoadBalancerSourceRanges []string `json:"loadBalancerSourceRanges,omitempty"`

	// ExternalTrafficPolicy of NodePort and LoadBalancer Services.
	// +kubebuilder:validation:Enum=Cluster;Local
	// +optional
	ExternalTrafficPolicy corev1.ServiceExternalTrafficPolicy `json:"externalTrafficPolicy,omitempty"`

	// Annotations of the Service.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Labels of the Service. Labels the operator manages cannot be overridden.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
}

// IngressSettings shape the optional Ingress of an HTTP component.
type IngressSettings struct {
	// Enabled creates the Ingress.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ClassName is the IngressClass.
	// +optional
	ClassName *string `json:"className,omitempty"`

	// Annotations of the Ingress.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Labels of the Ingress.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Hosts routed to the component.
	// +optional
	Hosts []IngressHost `json:"hosts,omitempty"`

	// TLS settings of the Ingress.
	// +optional
	TLS []networkingv1.IngressTLS `json:"tls,omitempty"`
}

// IngressHost is one host routed to a component.
type IngressHost struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Paths routed to the component, with path type Prefix.
	// +kubebuilder:default={"/"}
	// +optional
	Paths []string `json:"paths,omitempty"`
}

// ConfigMapKeyReference selects a key of a ConfigMap in the same namespace.
type ConfigMapKeyReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}
