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

func init() {
	ctrlmetrics.Registry.MustRegister(DatabaseReady, DatabaseCondition, DatabasePrimaryChanges)
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
