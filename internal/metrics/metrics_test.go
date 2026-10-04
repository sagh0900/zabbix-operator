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

package metrics

import (
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

var (
	fqNameRe = regexp.MustCompile(`fqName: "([^"]+)"`)
	metricRe = regexp.MustCompile(`zabbix_operator_[a-z_]+`)
)

// exported returns the names of every metric this package defines.
func exported(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	collectors := make([]prometheus.Collector, 0, len(otherCollectors)+len(systemCollectors))
	collectors = append(collectors, otherCollectors...)
	for _, c := range systemCollectors {
		collectors = append(collectors, c)
	}
	for _, c := range collectors {
		ch := make(chan *prometheus.Desc, 1)
		c.Describe(ch)
		close(ch)
		for d := range ch {
			m := fqNameRe.FindStringSubmatch(d.String())
			if m == nil {
				t.Fatalf("cannot read name from %s", d)
			}
			names[m[1]] = true
		}
	}
	return names
}

// TestMonitoringReferencesExportedMetrics fails when an alert rule or dashboard query
// uses a zabbix_operator_ metric that the operator does not export.
func TestMonitoringReferencesExportedMetrics(t *testing.T) {
	names := exported(t)
	for _, file := range []string{"../../config/monitoring/prometheusrule.yaml", "../../config/monitoring/dashboard.json"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		used := map[string]bool{}
		for _, m := range metricRe.FindAllString(string(data), -1) {
			used[m] = true
		}
		var unknown []string
		for m := range used {
			if !names[m] {
				unknown = append(unknown, m)
			}
		}
		sort.Strings(unknown)
		if len(unknown) > 0 {
			t.Errorf("%s references metrics that are not exported: %v", file, unknown)
		}
	}
}

func TestSetDatabaseConditionKeepsOneSeriesActive(t *testing.T) {
	SetDatabaseCondition("ns", "db", "ClusterReady", "True")
	SetDatabaseCondition("ns", "db", "ClusterReady", "False")
	for status, want := range map[string]float64{"True": 0, "False": 1, "Unknown": 0} {
		if got := testutil.ToFloat64(DatabaseCondition.WithLabelValues("ns", "db", "ClusterReady", status)); got != want {
			t.Errorf("status %s = %v, want %v", status, got, want)
		}
	}
	DeleteDatabase("ns", "db")
	if n := testutil.CollectAndCount(DatabaseCondition); n != 0 {
		t.Errorf("%d series left after DeleteDatabase", n)
	}
}

func TestSystemHelpers(t *testing.T) {
	SetSystemPhase("ns", "zabbix", "Running")
	SetSystemPhase("ns", "zabbix", "Upgrading")
	for phase, want := range map[string]float64{"Running": 0, "Upgrading": 1, "Blocked": 0} {
		if got := testutil.ToFloat64(SystemPhase.WithLabelValues("ns", "zabbix", phase)); got != want {
			t.Errorf("phase %s = %v, want %v", phase, got, want)
		}
	}
	SetSystemInfo("ns", "zabbix", "8.0.0rc1", "7.0.25")
	SetSystemInfo("ns", "zabbix", "8.0.0rc1", "8.0.0rc1")
	if n := testutil.CollectAndCount(SystemInfo); n != 1 {
		t.Errorf("%d info series, want 1", n)
	}
	SetUpgradeBlocked("ns", "zabbix", "PostgreSQLTooOld")
	SetUpgradeBlocked("ns", "zabbix", "")
	if n := testutil.CollectAndCount(UpgradeBlocked); n != 0 {
		t.Errorf("%d blocked series after clearing", n)
	}
	AgentNodesReady.WithLabelValues("ns", "zabbix").Set(2)
	DeleteAgent("ns", "zabbix")
	if n := testutil.CollectAndCount(AgentNodesReady); n != 0 {
		t.Errorf("%d agent series after DeleteAgent", n)
	}
	ServerActiveNodes.WithLabelValues("ns", "zabbix").Set(1)
	DeleteSystem("ns", "zabbix")
	for _, c := range systemCollectors {
		if n := testutil.CollectAndCount(c); n != 0 {
			t.Errorf("series left after DeleteSystem")
		}
	}
}
