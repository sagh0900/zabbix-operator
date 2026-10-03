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
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

const (
	primary1 = "pg-1"
	primary2 = "pg-2"
)

var t0 = time.Unix(1_000_000, 0).UTC()

func testDB() *zabbixv1alpha1.ZabbixDatabase {
	return &zabbixv1alpha1.ZabbixDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ns", Generation: 1},
		Spec: zabbixv1alpha1.ZabbixDatabaseSpec{
			ClusterRef:     zabbixv1alpha1.LocalObjectReference{Name: "pg"},
			CredentialsRef: zabbixv1alpha1.CredentialsReference{SecretName: "creds"},
		},
	}
}

func healthyCluster(primary string) cnpgCluster {
	return cnpgCluster{found: true, phase: "Cluster in healthy state", currentPrimary: primary,
		targetPrimary: primary, healthy: []string{primary1, primary2}}
}

var goodCreds = credentialsState{found: true}

func cond(t *testing.T, st zabbixv1alpha1.ZabbixDatabaseStatus, typ string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(st.Conditions, typ)
	if c == nil {
		t.Fatalf("condition %s missing", typ)
	}
	return c
}

func TestComputeDatabaseStatus_Readiness(t *testing.T) {
	cases := []struct {
		name       string
		cluster    cnpgCluster
		creds      credentialsState
		wantReady  bool
		wantReason string
	}{
		{"healthy", healthyCluster(primary1), goodCreds, true, ReasonReady},
		{"cluster missing", cnpgCluster{}, goodCreds, false, ReasonClusterNotFound},
		{"no primary", cnpgCluster{found: true}, goodCreds, false, ReasonNoPrimary},
		{"switchover", cnpgCluster{found: true, currentPrimary: primary1, targetPrimary: primary2,
			healthy: []string{primary1, primary2}}, goodCreds, false, ReasonSwitchoverInProgress},
		{"primary unhealthy", cnpgCluster{found: true, currentPrimary: primary1, targetPrimary: primary1,
			healthy: []string{primary2}}, goodCreds, false, ReasonPrimaryNotHealthy},
		{"secret missing", healthyCluster(primary1), credentialsState{}, false, ReasonSecretNotFound},
		{"key missing", healthyCluster(primary1), credentialsState{found: true, missingKeys: []string{defaultPasswordKey}},
			false, ReasonKeysMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := computeDatabaseStatus(testDB(), tc.cluster, tc.creds, t0)
			ready := cond(t, st, zabbixv1alpha1.DatabaseReady)
			if got := ready.Status == metav1.ConditionTrue; got != tc.wantReady {
				t.Errorf("Ready = %v, want %v (%s)", got, tc.wantReady, ready.Message)
			}
			if ready.Reason != tc.wantReason {
				t.Errorf("Reason = %s, want %s", ready.Reason, tc.wantReason)
			}
			if st.ObservedGeneration != 1 {
				t.Errorf("ObservedGeneration = %d", st.ObservedGeneration)
			}
		})
	}
}

func TestComputeDatabaseStatus_ClusterReadinessPrecedesCredentials(t *testing.T) {
	st, _ := computeDatabaseStatus(testDB(), cnpgCluster{}, credentialsState{}, t0)
	if r := cond(t, st, zabbixv1alpha1.DatabaseReady).Reason; r != ReasonClusterNotFound {
		t.Errorf("Reason = %s, want %s", r, ReasonClusterNotFound)
	}
	if c := cond(t, st, zabbixv1alpha1.DatabaseCredentialsReady); c.Status != metav1.ConditionFalse {
		t.Errorf("CredentialsReady = %s, want False", c.Status)
	}
}

// Primary changes beyond the threshold make the database not ready; after a quiet
// window it becomes ready again without intervention.
func TestComputeDatabaseStatus_FlappingAndRecovery(t *testing.T) {
	db := testDB()
	db.Spec.Flap = &zabbixv1alpha1.FlapSettings{Threshold: 2, Window: &metav1.Duration{Duration: time.Minute}}

	now := t0
	db.Status, _ = computeDatabaseStatus(db, healthyCluster(primary1), goodCreds, now)

	now = now.Add(10 * time.Second)
	db.Status, _ = computeDatabaseStatus(db, healthyCluster(primary2), goodCreds, now)
	if c := cond(t, db.Status, zabbixv1alpha1.DatabaseReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("one change must not flap: %s", c.Reason)
	}

	now = now.Add(10 * time.Second)
	var requeue time.Duration
	db.Status, requeue = computeDatabaseStatus(db, healthyCluster(primary1), goodCreds, now)
	if c := cond(t, db.Status, zabbixv1alpha1.DatabaseReady); c.Reason != ReasonPrimaryFlapping {
		t.Fatalf("Ready reason = %s, want %s", c.Reason, ReasonPrimaryFlapping)
	}
	if requeue <= 0 || requeue > time.Minute+time.Second {
		t.Fatalf("requeue = %s, want within the window", requeue)
	}

	now = now.Add(requeue)
	db.Status, requeue = computeDatabaseStatus(db, healthyCluster(primary1), goodCreds, now)
	if c := cond(t, db.Status, zabbixv1alpha1.DatabaseReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("expected recovery after a quiet window, got %s", c.Reason)
	}
	if db.Status.PrimaryChanges != 0 || requeue != 0 {
		t.Fatalf("PrimaryChanges = %d, requeue = %s; want 0, 0", db.Status.PrimaryChanges, requeue)
	}
}

func TestComputeDatabaseStatus_MissingClusterKeepsPrimaryHistory(t *testing.T) {
	db := testDB()
	db.Status, _ = computeDatabaseStatus(db, healthyCluster(primary1), goodCreds, t0)
	db.Status, _ = computeDatabaseStatus(db, cnpgCluster{}, goodCreds, t0.Add(time.Second))
	if db.Status.CurrentPrimary != primary1 {
		t.Errorf("CurrentPrimary = %q, want pg-1", db.Status.CurrentPrimary)
	}
}

func TestComputeDatabaseStatus_TransitionTimeOnlyChangesWithStatus(t *testing.T) {
	db := testDB()
	db.Status, _ = computeDatabaseStatus(db, healthyCluster(primary1), goodCreds, t0)
	first := cond(t, db.Status, zabbixv1alpha1.DatabaseReady).LastTransitionTime
	db.Status, _ = computeDatabaseStatus(db, healthyCluster(primary1), goodCreds, t0.Add(time.Hour))
	if got := cond(t, db.Status, zabbixv1alpha1.DatabaseReady).LastTransitionTime; !got.Equal(&first) {
		t.Errorf("LastTransitionTime moved from %v to %v without a status change", first, got)
	}
}

func TestInspectCredentials(t *testing.T) {
	db := testDB()
	if s := inspectCredentials(db, nil); s.found {
		t.Error("nil data must mean not found")
	}
	s := inspectCredentials(db, map[string][]byte{"username": []byte("zabbix"), "password": {}})
	if !s.found || len(s.missingKeys) != 1 || s.missingKeys[0] != defaultPasswordKey {
		t.Errorf("got %+v, want missing [password]", s)
	}
	db.Spec.CredentialsRef.UsernameKey, db.Spec.CredentialsRef.PasswordKey = "user", "pass"
	s = inspectCredentials(db, map[string][]byte{"user": []byte("u"), "pass": []byte("p")})
	if len(s.missingKeys) != 0 {
		t.Errorf("custom keys: missing %v", s.missingKeys)
	}
}

func TestParseCNPGCluster(t *testing.T) {
	if c := parseCNPGCluster(nil); c.found {
		t.Error("nil must mean not found")
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"phase":           "Cluster in healthy state",
			"currentPrimary":  primary1,
			"targetPrimary":   primary1,
			"instancesStatus": map[string]interface{}{"healthy": []interface{}{primary1, primary2}},
		},
	}}
	c := parseCNPGCluster(u)
	if !c.found || c.currentPrimary != primary1 || c.targetPrimary != primary1 || len(c.healthy) != 2 {
		t.Errorf("parsed %+v", c)
	}
}

func TestHostDefaults(t *testing.T) {
	db := testDB()
	if db.HostOrDefault() != "pg-rw" || db.DirectHostOrDefault() != "pg-rw" {
		t.Errorf("defaults: %s %s", db.HostOrDefault(), db.DirectHostOrDefault())
	}
	db.Spec.Host, db.Spec.DirectHost = "pooler", "direct"
	if db.HostOrDefault() != "pooler" || db.DirectHostOrDefault() != "direct" {
		t.Errorf("overrides: %s %s", db.HostOrDefault(), db.DirectHostOrDefault())
	}
}
