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

package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/jobs"
	"github.com/sagh0900/zabbix-operator/internal/metrics"
	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

// setCompatibility writes the operator's compatibility ConfigMap for the rest of the test.
func setCompatibility(t *testing.T, data string) {
	t.Helper()
	ctx := context.Background()
	ensureNamespace(t, testOperatorNamespace)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: testOperatorNamespace, Name: testCompatibilityConfigMap},
		Data: map[string]string{CompatibilityKey: data}}
	if err := k8s.Create(ctx, cm); err != nil {
		if err := k8s.Update(ctx, cm); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), cm) })
}

const line82 = "lines:\n  - line: \"8.2\"\n    minPostgres: 15\n    maxPostgres: 18\n"

// A line added through the ConfigMap installs and runs, is reported as not validated, and
// its precheck enforces the configured PostgreSQL limits.
func TestCompatibility_ConfiguredLine(t *testing.T) {
	requireEnvtest(t)
	setCompatibility(t, line82)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "8.2.0", nil)

	job := pendingJob(t, ns, jobs.CommandPrecheck)
	args := job.Spec.Template.Spec.Containers[0].Args
	for _, want := range []string{"--target-version=8.2.0", "--min-postgres=15", "--max-postgres=18"} {
		if !slices.Contains(args, want) {
			t.Errorf("precheck args %v lack %s", args, want)
		}
	}
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.FreshInstall)})
	waitPod(t, ns, "zabbix-server-init-0", true)
	eventually(t, func() error {
		c := meta.FindStatusCondition(getSystem(t, ns).Status.Conditions, zabbixv1alpha1.SystemUnverifiedVersion)
		if c == nil || c.Status != metav1.ConditionTrue {
			return fmt.Errorf("UnverifiedVersion = %+v", c)
		}
		return nil
	})
	waitEvent(t, ns, corev1.EventTypeWarning, zabbixv1alpha1.SystemUnverifiedVersion,
		"Zabbix 8.2 comes from ConfigMap zabbix-operator/zabbix-operator-compatibility and is not validated")
}

// Upgrading to a line that is not validated needs a backup even when the backup check is
// turned off.
func TestCompatibility_UnverifiedUpgradeNeedsBackup(t *testing.T) {
	requireEnvtest(t)
	setCompatibility(t, line82)
	ns := newNamespace(t)
	installed(t, ns, "8.0.0rc1", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Upgrade.RequireBackupWithin = &metav1.Duration{}
	})
	if c := meta.FindStatusCondition(getSystem(t, ns).Status.Conditions, zabbixv1alpha1.SystemUnverifiedVersion); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("a validated line must not be reported: %+v", c)
	}
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version, s.Upgrade.ApproveMajor = "8.2.0", "8.2" })
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "Zabbix 8.2 is not validated with this operator version; upgrading to it needs a completed CNPG Backup")

	createBackup(t, ns, "before-8-2", time.Now().Add(-time.Hour))
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "Upgrading from 8.0.0rc1 to 8.2.0")
}

// An invalid ConfigMap is ignored: only the built-in lines apply, and the metric says so.
func TestCompatibility_InvalidConfigMap(t *testing.T) {
	requireEnvtest(t)
	setCompatibility(t, "lines:\n  - line: nonsense\n")
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "8.2.0", nil)
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "Zabbix 8.2 is not supported by this operator version (supported lines: 7.0, 7.2, 7.4, 8.0)")
	if v := testutil.ToFloat64(metrics.CompatibilityConfigValid); v != 0 {
		t.Errorf("compatibility_config_valid = %v, want 0", v)
	}
}
