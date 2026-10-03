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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Condition types reported on a ZabbixDatabase.
const (
	// DatabaseClusterReady is True when the CNPG cluster has a healthy primary that is
	// not being switched over.
	DatabaseClusterReady = "ClusterReady"
	// DatabaseCredentialsReady is True when the credentials Secret has both keys.
	DatabaseCredentialsReady = "CredentialsReady"
	// DatabasePrimaryStable is False while the primary changes more often than the
	// configured flap threshold.
	DatabasePrimaryStable = "PrimaryStable"
	// DatabaseReady is True when all other conditions are True. Zabbix may connect only
	// while it is True.
	DatabaseReady = "Ready"
)

// ZabbixDatabaseSpec references a CloudNativePG cluster and the credentials Zabbix uses.
type ZabbixDatabaseSpec struct {
	// ClusterRef names the CNPG Cluster in the same namespace.
	ClusterRef LocalObjectReference `json:"clusterRef"`

	// Host is the database host Zabbix connects to, for example a Pooler Service.
	// Defaults to <cluster>-rw.
	// +optional
	Host string `json:"host,omitempty"`

	// DirectHost is the host used for schema work. It must reach the primary directly,
	// never through a transaction pooler. Defaults to <cluster>-rw.
	// +optional
	DirectHost string `json:"directHost,omitempty"`

	// Port of Host and DirectHost.
	// +kubebuilder:default=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`

	// Database is the name of the Zabbix database.
	// +kubebuilder:default=zabbix
	// +optional
	Database string `json:"database,omitempty"`

	// CredentialsRef points at the Secret holding the database user and password,
	// typically the passwordSecret of a CNPG managed role.
	CredentialsRef CredentialsReference `json:"credentialsRef"`

	// TLS configures client TLS to PostgreSQL.
	// +optional
	TLS *DatabaseTLS `json:"tls,omitempty"`

	// Flap configures primary-change damping.
	// +optional
	Flap *FlapSettings `json:"flap,omitempty"`
}

// LocalObjectReference names an object in the same namespace.
type LocalObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretKeyReference selects a key of a Secret in the same namespace.
type SecretKeyReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// CredentialsReference selects the user and password keys of a Secret.
type CredentialsReference struct {
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`
	// +kubebuilder:default=username
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`
	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// DatabaseTLS configures client TLS to PostgreSQL.
type DatabaseTLS struct {
	// Mode is the libpq sslmode.
	// +kubebuilder:validation:Enum=require;verify-ca;verify-full
	Mode string `json:"mode"`
	// CASecretRef selects the CA certificate that signs the server certificate.
	// Required for verify-ca and verify-full.
	// +optional
	CASecretRef *SecretKeyReference `json:"caSecretRef,omitempty"`
	// ClientCertSecretRef names a kubernetes.io/tls Secret with a client certificate.
	// +optional
	ClientCertSecretRef *LocalObjectReference `json:"clientCertSecretRef,omitempty"`
}

// FlapSettings configures primary-change damping.
type FlapSettings struct {
	// Threshold is the number of primary changes within Window that marks the
	// primary unstable.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +optional
	Threshold int `json:"threshold,omitempty"`
	// Window is the sliding window for counting primary changes.
	// +kubebuilder:default="10m"
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`
}

// ZabbixDatabaseStatus reports whether Zabbix may connect to the database.
type ZabbixDatabaseStatus struct {
	// ObservedGeneration is the generation the status was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase mirrors the phase reported by CNPG.
	// +optional
	Phase string `json:"phase,omitempty"`
	// CurrentPrimary is the pod name of the current primary.
	// +optional
	CurrentPrimary string `json:"currentPrimary,omitempty"`
	// PrimaryChanges counts primary changes in the current flap window.
	// +optional
	PrimaryChanges int `json:"primaryChanges,omitempty"`
	// LastPrimaryChange is when the primary last changed.
	// +optional
	LastPrimaryChange *metav1.Time `json:"lastPrimaryChange,omitempty"`
	// Conditions: ClusterReady, CredentialsReady, PrimaryStable, Ready.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ZabbixDatabase is a read-only view of a CloudNativePG cluster used by Zabbix.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=zdb
// +kubebuilder:validation:XValidation:rule="!has(self.spec.tls) || self.spec.tls.mode == 'require' || has(self.spec.tls.caSecretRef)",message="spec.tls.caSecretRef is required for verify-ca and verify-full"
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.currentPrimary`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ZabbixDatabase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZabbixDatabaseSpec   `json:"spec,omitempty"`
	Status ZabbixDatabaseStatus `json:"status,omitempty"`
}

// ZabbixDatabaseList is a list of ZabbixDatabase.
// +kubebuilder:object:root=true
type ZabbixDatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZabbixDatabase `json:"items"`
}

func init() {
	knownTypes = append(knownTypes, &ZabbixDatabase{}, &ZabbixDatabaseList{})
}

// HostOrDefault returns the host Zabbix connects to.
func (d *ZabbixDatabase) HostOrDefault() string {
	if d.Spec.Host != "" {
		return d.Spec.Host
	}
	return d.Spec.ClusterRef.Name + "-rw"
}

// DirectHostOrDefault returns the host used for schema work.
func (d *ZabbixDatabase) DirectHostOrDefault() string {
	if d.Spec.DirectHost != "" {
		return d.Spec.DirectHost
	}
	return d.Spec.ClusterRef.Name + "-rw"
}
