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

// Condition types reported on a ZabbixSystem.
const (
	SystemDatabaseReady  = "DatabaseReady"
	SystemServerActive   = "ServerActive"
	SystemWebReady       = "WebReady"
	SystemUpgrading      = "Upgrading"
	SystemUpgradeBlocked = "UpgradeBlocked"
	SystemConflict       = "Conflict"
)

// SystemPhase summarises the state of a ZabbixSystem.
// +kubebuilder:validation:Enum=Installing;Running;Upgrading;Degraded;Blocked
type SystemPhase string

// Phases of a ZabbixSystem.
const (
	PhaseInstalling SystemPhase = "Installing"
	PhaseRunning    SystemPhase = "Running"
	PhaseUpgrading  SystemPhase = "Upgrading"
	PhaseDegraded   SystemPhase = "Degraded"
	PhaseBlocked    SystemPhase = "Blocked"
)

// ZabbixSystemSpec describes one Zabbix installation: server, frontend, web service,
// proxies and agents, on the database referenced by DatabaseRef.
// +kubebuilder:validation:XValidation:rule="self.version == oldSelf.version || (int(self.version.split('.')[0]) * 1000000 + int(self.version.split('.')[1]) * 1000 + int(self.version.split('.')[2].find('^[0-9]+'))) >= (int(oldSelf.version.split('.')[0]) * 1000000 + int(oldSelf.version.split('.')[1]) * 1000 + int(oldSelf.version.split('.')[2].find('^[0-9]+')))",message="spec.version cannot be lowered; Zabbix does not support downgrades"
// +kubebuilder:validation:XValidation:rule="self.databaseRef == oldSelf.databaseRef",message="spec.databaseRef is immutable"
type ZabbixSystemSpec struct {
	// Version of Zabbix for server, frontend, web service and proxies, for example 7.0.25
	// or 8.0.0rc1. A release line this operator version does not support is reported as
	// Blocked and never deployed.
	// +kubebuilder:validation:MaxLength=24
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]?\.(0|[1-9][0-9]?)\.(0|[1-9][0-9]*)((alpha|beta|rc)[1-9][0-9]*)?$`
	Version string `json:"version"`

	// DatabaseRef names the ZabbixDatabase in the same namespace.
	DatabaseRef LocalObjectReference `json:"databaseRef"`

	// ImageRepository is the registry and path prefix of the official Zabbix images.
	// +kubebuilder:default=zabbix
	// +optional
	ImageRepository string `json:"imageRepository,omitempty"`

	// ImageFlavor selects the base OS of the official images.
	// +kubebuilder:validation:Enum=ubuntu;alpine
	// +kubebuilder:default=ubuntu
	// +optional
	ImageFlavor string `json:"imageFlavor,omitempty"`

	// Timezone (TZ) of all Zabbix containers, for example Europe/Stockholm.
	// +optional
	Timezone string `json:"timezone,omitempty"`

	// Upgrade controls upgrades between versions.
	// +kubebuilder:default={}
	// +optional
	Upgrade UpgradeSettings `json:"upgrade,omitempty"`

	// Server is the Zabbix server, run in native HA mode.
	// +kubebuilder:default={}
	// +optional
	Server ServerSpec `json:"server,omitempty"`

	// Web is the Zabbix frontend.
	// +kubebuilder:default={}
	// +optional
	Web WebSpec `json:"web,omitempty"`

	// WebService is the Zabbix web service used for scheduled reports.
	// +kubebuilder:default={}
	// +optional
	WebService WebServiceSpec `json:"webService,omitempty"`

	// Proxies run in the cluster, each with its own instance count.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Proxies []ProxySpec `json:"proxies,omitempty"`

	// Agent runs Zabbix agent 2 on every eligible node.
	// +kubebuilder:default={}
	// +optional
	Agent AgentSpec `json:"agent,omitempty"`

	// ProxyRegistration keeps proxies registered in Zabbix through its API.
	// +kubebuilder:default={}
	// +optional
	ProxyRegistration ProxyRegistrationSpec `json:"proxyRegistration,omitempty"`
}

// UpgradeSettings control upgrades between versions.
type UpgradeSettings struct {
	// ApproveMajor approves one major upgrade when it equals the target line (for
	// example "8.0"). A major upgrade is irreversible without a database restore.
	// +kubebuilder:validation:Pattern=`^([1-9][0-9]?\.(0|[1-9][0-9]?))?$`
	// +optional
	ApproveMajor string `json:"approveMajor,omitempty"`

	// RequireBackupWithin makes a major upgrade wait for a completed CNPG Backup of the
	// cluster that is at most this old.
	// +kubebuilder:default="24h"
	// +optional
	RequireBackupWithin *metav1.Duration `json:"requireBackupWithin,omitempty"`
}

// ServerSpec configures the Zabbix server.
type ServerSpec struct {
	PodSettings `json:",inline"`

	// Replicas is the number of HA nodes. One is active; the others are standby.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=9
	// +kubebuilder:default=2
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Service exposes the active node on port 10051.
	// +kubebuilder:default={}
	// +optional
	Service ServiceSettings `json:"service,omitempty"`
}

// WebSpec configures the Zabbix frontend.
type WebSpec struct {
	PodSettings `json:",inline"`

	// Enabled runs the frontend.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Replicas of the frontend.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Service of the frontend, port 80 by default.
	// +kubebuilder:default={}
	// +optional
	Service ServiceSettings `json:"service,omitempty"`

	// Ingress of the frontend.
	// +optional
	Ingress IngressSettings `json:"ingress,omitempty"`
}

// WebServiceSpec configures the Zabbix web service.
type WebServiceSpec struct {
	PodSettings `json:",inline"`

	// Enabled runs the web service.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Replicas of the web service.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Service of the web service, port 10053 by default.
	// +kubebuilder:default={}
	// +optional
	Service ServiceSettings `json:"service,omitempty"`

	// Ingress of the web service.
	// +optional
	Ingress IngressSettings `json:"ingress,omitempty"`
}

// ProxyMode is how a proxy exchanges data with the server.
// +kubebuilder:validation:Enum=active;passive
type ProxyMode string

// Proxy modes.
const (
	ProxyActive  ProxyMode = "active"
	ProxyPassive ProxyMode = "passive"
)

// ProxySpec configures one in-cluster proxy. Instance i uses hostname <name>-<i>.
type ProxySpec struct {
	PodSettings `json:",inline"`

	// Name of the proxy; instances are named <name>-<i>.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=50
	Name string `json:"name"`

	// Enabled runs the proxy. Disabling keeps its settings in the spec.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Mode of the proxy.
	// +kubebuilder:default=active
	// +optional
	Mode ProxyMode `json:"mode,omitempty"`

	// Replicas of the proxy.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	// +kubebuilder:default=1
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Service of a passive proxy, port 10051 by default.
	// +kubebuilder:default={}
	// +optional
	Service ServiceSettings `json:"service,omitempty"`
}

// AgentSpec configures Zabbix agent 2, run on every eligible node.
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || (has(self.image) && size(self.image) > 0)",message="spec.agent.image is required when the agent is enabled"
type AgentSpec struct {
	PodSettings `json:",inline"`

	// Enabled runs the agent.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// ProxyRegistrationSpec keeps proxies registered in Zabbix through its API.
// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || (has(self.apiTokenSecretRef) && has(self.configMapRef))",message="apiTokenSecretRef and configMapRef are required when proxy registration is enabled"
type ProxyRegistrationSpec struct {
	// Enabled turns proxy registration on.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// APITokenSecretRef selects the Zabbix API token.
	// +optional
	APITokenSecretRef *SecretKeyReference `json:"apiTokenSecretRef,omitempty"`

	// ConfigMapRef selects the list of proxies to register.
	// +optional
	ConfigMapRef *ConfigMapKeyReference `json:"configMapRef,omitempty"`

	// Prune deletes proxies this system registered that the list no longer contains.
	// +optional
	Prune bool `json:"prune,omitempty"`
}

// ZabbixSystemStatus reports the observed state of a ZabbixSystem.
type ZabbixSystemStatus struct {
	// ObservedGeneration is the generation the status was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase summarises the state of the system.
	// +optional
	Phase SystemPhase `json:"phase,omitempty"`

	// PhaseReason explains the phase in one line, for example what an upgrade or a
	// rollout is waiting for.
	// +optional
	PhaseReason string `json:"phaseReason,omitempty"`

	// RunningVersion is the Zabbix version the database schema and server run.
	// +optional
	RunningVersion string `json:"runningVersion,omitempty"`

	// ActiveServer is the server pod Zabbix reports as active.
	// +optional
	ActiveServer *ActiveServer `json:"activeServer,omitempty"`

	// LastHANodeGCTime is when stale ha_node rows were last removed successfully.
	// +optional
	LastHANodeGCTime *metav1.Time `json:"lastHANodeGCTime,omitempty"`

	// Components reports desired and ready pods per component.
	// +listType=map
	// +listMapKey=name
	// +optional
	Components []ComponentStatus `json:"components,omitempty"`

	// Conditions: DatabaseReady, ServerActive, WebReady, Upgrading, UpgradeBlocked, Conflict.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ActiveServer identifies the active server pod.
type ActiveServer struct {
	Pod string `json:"pod"`
	// +optional
	IP string `json:"ip,omitempty"`
}

// ComponentStatus reports pods of one component.
type ComponentStatus struct {
	// Name is server, web, webservice, agent or proxy/<name>.
	Name    string `json:"name"`
	Desired int32  `json:"desired"`
	Ready   int32  `json:"ready"`
}

// ZabbixSystem is a complete Zabbix installation.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=zsys
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Running",type=string,JSONPath=`.status.runningVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.activeServer.pod`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.phaseReason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ZabbixSystem struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ZabbixSystemSpec   `json:"spec,omitempty"`
	Status ZabbixSystemStatus `json:"status,omitempty"`
}

// ZabbixSystemList is a list of ZabbixSystem.
// +kubebuilder:object:root=true
type ZabbixSystemList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZabbixSystem `json:"items"`
}

func init() {
	knownTypes = append(knownTypes, &ZabbixSystem{}, &ZabbixSystemList{})
}

// IsEnabled reports whether the frontend runs.
func (w *WebSpec) IsEnabled() bool { return w.Enabled == nil || *w.Enabled }

// IsEnabled reports whether the web service runs.
func (w *WebServiceSpec) IsEnabled() bool { return w.Enabled == nil || *w.Enabled }

// IsEnabled reports whether the proxy runs.
func (p *ProxySpec) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Replica counts applied when a spec section is omitted by a client that bypasses API
// defaulting; they match the CRD defaults.
const (
	DefaultServerReplicas int32 = 2
	DefaultReplicas       int32 = 1
)

// ServerReplicas returns the number of server HA nodes.
func (s *ZabbixSystemSpec) ServerReplicas() int32 {
	if s.Server.Replicas > 0 {
		return s.Server.Replicas
	}
	return DefaultServerReplicas
}

// WebReplicas returns the number of frontend pods.
func (s *ZabbixSystemSpec) WebReplicas() int32 {
	if s.Web.Replicas > 0 {
		return s.Web.Replicas
	}
	return DefaultReplicas
}

// WebServiceReplicas returns the number of web service pods.
func (s *ZabbixSystemSpec) WebServiceReplicas() int32 {
	if s.WebService.Replicas > 0 {
		return s.WebService.Replicas
	}
	return DefaultReplicas
}
