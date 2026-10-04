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

// Package metrics defines the Prometheus metrics exported by the operator. They are
// registered with the controller-runtime registry and served by the manager.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	namespace = "zabbix_operator"

	labelNamespace = "namespace"
	labelDatabase  = "database"
	labelSystem    = "system"
	labelComponent = "component"
)

var (
	// DatabaseReady is 1 when a ZabbixDatabase is Ready.
	DatabaseReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "database_ready",
		Help:      "1 when the ZabbixDatabase is Ready, otherwise 0.",
	}, []string{labelNamespace, labelDatabase})

	// DatabaseCondition is 1 for the current status of each ZabbixDatabase condition.
	DatabaseCondition = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "database_condition",
		Help:      "1 for the current status of each ZabbixDatabase condition.",
	}, []string{labelNamespace, labelDatabase, "condition", "status"})

	// DatabasePrimaryChanges counts primary changes observed for a ZabbixDatabase.
	DatabasePrimaryChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "database_primary_changes_total",
		Help:      "Primary changes observed for the ZabbixDatabase.",
	}, []string{labelNamespace, labelDatabase})
)

var (
	// SystemPhase is 1 for the current phase of a ZabbixSystem.
	SystemPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "system_phase",
		Help: "1 for the current phase of the ZabbixSystem.",
	}, []string{labelNamespace, labelSystem, "phase"})

	// SystemInfo carries the desired and running Zabbix versions.
	SystemInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "system_info",
		Help: "Always 1; labels carry the desired and running Zabbix versions.",
	}, []string{labelNamespace, labelSystem, "version", "running_version"})

	// ComponentPodsDesired is the number of pods a component should have.
	ComponentPodsDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "component_pods_desired",
		Help: "Pods a component of the ZabbixSystem should have.",
	}, []string{labelNamespace, labelSystem, labelComponent})

	// ComponentPodsReady is the number of a component's pods that are ready.
	ComponentPodsReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "component_pods_ready",
		Help: "Pods of a component of the ZabbixSystem that are ready.",
	}, []string{labelNamespace, labelSystem, labelComponent})

	// PodReplacements counts pods the operator deleted to replace them.
	PodReplacements = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "pod_replacements_total",
		Help: "Pods the operator deleted, by reason: rollout, failed or scaledown.",
	}, []string{labelNamespace, labelSystem, labelComponent, "reason"})

	// ServerActiveNodes is the number of server pods accepting trapper connections.
	ServerActiveNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "server_active_nodes",
		Help: "Server pods that are the active HA node (expected: 1).",
	}, []string{labelNamespace, labelSystem})

	// ServerFailovers counts changes of the active server pod.
	ServerFailovers = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "server_failovers_total",
		Help: "Changes of the active server pod.",
	}, []string{labelNamespace, labelSystem})

	// UpgradeInProgress is 1 while a version change runs.
	UpgradeInProgress = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "upgrade_in_progress",
		Help: "1 while the ZabbixSystem moves to a new version.",
	}, []string{labelNamespace, labelSystem})

	// UpgradeBlocked is 1 for the reason an install or upgrade is blocked.
	UpgradeBlocked = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "upgrade_blocked",
		Help: "1 for the reason the install or upgrade of the ZabbixSystem is blocked.",
	}, []string{labelNamespace, labelSystem, "reason"})

	// JobRuns counts finished database Jobs.
	JobRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "job_runs_total",
		Help: "Finished database Jobs, by command and result (succeeded or failed).",
	}, []string{labelNamespace, labelSystem, "job", "result"})

	// AgentNodesDesired is the number of nodes that should run an agent.
	AgentNodesDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "agent_nodes_desired",
		Help: "Nodes that should run a Zabbix agent (only while the agent is enabled).",
	}, []string{labelNamespace, labelSystem})

	// AgentNodesReady is the number of nodes with a ready agent.
	AgentNodesReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "agent_nodes_ready",
		Help: "Nodes with a ready Zabbix agent (only while the agent is enabled).",
	}, []string{labelNamespace, labelSystem})

	// HANodeGCRowsDeleted counts stale ha_node rows removed.
	HANodeGCRowsDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "hanode_gc_rows_deleted_total",
		Help: "Stale ha_node rows removed by ha-gc.",
	}, []string{labelNamespace, labelSystem})

	// HANodeGCLastSuccess is the time of the last successful ha-gc run.
	HANodeGCLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "hanode_gc_last_success_timestamp_seconds",
		Help: "Unix time of the last successful ha-gc run.",
	}, []string{labelNamespace, labelSystem})
)

var (
	// ProxiesRegistered is the number of proxies the system keeps registered in Zabbix.
	ProxiesRegistered = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "proxies_registered",
		Help: "Proxies the ZabbixSystem keeps registered in Zabbix (only while registration is enabled).",
	}, []string{labelNamespace, labelSystem})

	// ProxyRegistrationFailing is 1 while the last proxy registration sync failed.
	ProxyRegistrationFailing = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "proxy_registration_failing",
		Help: "1 while the last proxy registration sync failed (only while registration is enabled).",
	}, []string{labelNamespace, labelSystem})

	// ProxyRegistrationSyncs counts proxy registration syncs.
	ProxyRegistrationSyncs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "proxy_registration_syncs_total",
		Help: "Proxy registration syncs with the Zabbix API, by result (succeeded or failed).",
	}, []string{labelNamespace, labelSystem, "result"})
)

// systemCollectors are every metric labelled by a ZabbixSystem.
var systemCollectors = []interface {
	prometheus.Collector
	DeletePartialMatch(prometheus.Labels) int
}{
	SystemPhase, SystemInfo, ComponentPodsDesired, ComponentPodsReady, PodReplacements, ServerActiveNodes,
	ServerFailovers, UpgradeInProgress, UpgradeBlocked, JobRuns, HANodeGCRowsDeleted, HANodeGCLastSuccess,
	AgentNodesDesired, AgentNodesReady, ProxiesRegistered, ProxyRegistrationFailing, ProxyRegistrationSyncs,
}

func init() {
	ctrlmetrics.Registry.MustRegister(DatabaseReady, DatabaseCondition, DatabasePrimaryChanges)
	for _, c := range systemCollectors {
		ctrlmetrics.Registry.MustRegister(c)
	}
}

// SystemPhases are the phases a ZabbixSystem reports.
var SystemPhases = []string{"Installing", "Running", "Upgrading", "Degraded", "Blocked"}

// SetSystemPhase sets the phase series so exactly the current phase is 1.
func SetSystemPhase(ns, name, phase string) {
	for _, p := range SystemPhases {
		v := 0.0
		if p == phase {
			v = 1
		}
		SystemPhase.WithLabelValues(ns, name, p).Set(v)
	}
}

// SetSystemInfo replaces the info series of a system.
func SetSystemInfo(ns, name, version, running string) {
	SystemInfo.DeletePartialMatch(prometheus.Labels{labelNamespace: ns, labelSystem: name})
	SystemInfo.WithLabelValues(ns, name, version, running).Set(1)
}

// SetUpgradeBlocked sets the blocked reason of a system; an empty reason clears it.
func SetUpgradeBlocked(ns, name, reason string) {
	UpgradeBlocked.DeletePartialMatch(prometheus.Labels{labelNamespace: ns, labelSystem: name})
	if reason != "" {
		UpgradeBlocked.WithLabelValues(ns, name, reason).Set(1)
	}
}

// DeleteAgent removes the agent series of a system whose agent is disabled.
func DeleteAgent(ns, name string) {
	labels := prometheus.Labels{labelNamespace: ns, labelSystem: name}
	AgentNodesDesired.DeletePartialMatch(labels)
	AgentNodesReady.DeletePartialMatch(labels)
}

// DeleteRegistration removes the registration gauges of a system whose registration is
// disabled.
func DeleteRegistration(ns, name string) {
	labels := prometheus.Labels{labelNamespace: ns, labelSystem: name}
	ProxiesRegistered.DeletePartialMatch(labels)
	ProxyRegistrationFailing.DeletePartialMatch(labels)
}

// DeleteSystem removes every series of a deleted ZabbixSystem.
func DeleteSystem(ns, name string) {
	labels := prometheus.Labels{labelNamespace: ns, labelSystem: name}
	for _, c := range systemCollectors {
		c.DeletePartialMatch(labels)
	}
}

// conditionStatuses are the values a condition status can take.
var conditionStatuses = []string{"True", "False", "Unknown"}

// SetDatabaseCondition records the status of one condition, clearing the other statuses
// so exactly one series per condition is 1.
func SetDatabaseCondition(ns, name, condition, status string) {
	for _, s := range conditionStatuses {
		v := 0.0
		if s == status {
			v = 1
		}
		DatabaseCondition.WithLabelValues(ns, name, condition, s).Set(v)
	}
}

// DeleteDatabase removes every series of a deleted ZabbixDatabase.
func DeleteDatabase(ns, name string) {
	labels := prometheus.Labels{labelNamespace: ns, labelDatabase: name}
	DatabaseReady.DeletePartialMatch(labels)
	DatabaseCondition.DeletePartialMatch(labels)
	DatabasePrimaryChanges.DeletePartialMatch(labels)
}
