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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

const waitFor = 10 * time.Second

func createSecret(t *testing.T, ns string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns},
		Data:       map[string][]byte{"username": []byte("zabbix"), "password": []byte("secret")},
	}
	if err := k8s.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

// createCluster creates a CNPG Cluster with a healthy primary pg-1, reported the way
// CNPG reports it.
func createCluster(t *testing.T, ns string) *unstructured.Unstructured {
	t.Helper()
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(CNPGClusterGVK)
	c.SetNamespace(ns)
	c.SetName("pg")
	if err := unstructured.SetNestedField(c.Object, int64(2), "spec", "instances"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	setClusterStatus(t, c, primary1, primary1)
	return c
}

func setClusterStatus(t *testing.T, c *unstructured.Unstructured, current, target string) {
	t.Helper()
	status := map[string]interface{}{
		"phase":           "Cluster in healthy state",
		"currentPrimary":  current,
		"targetPrimary":   target,
		"instancesStatus": map[string]interface{}{"healthy": []interface{}{primary1, primary2}},
	}
	if err := unstructured.SetNestedMap(c.Object, status, "status"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Status().Update(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}

func createDatabase(t *testing.T, ns string) *zabbixv1alpha1.ZabbixDatabase {
	t.Helper()
	db := &zabbixv1alpha1.ZabbixDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "zabbix-db", Namespace: ns},
		Spec: zabbixv1alpha1.ZabbixDatabaseSpec{
			ClusterRef:     zabbixv1alpha1.LocalObjectReference{Name: "pg"},
			CredentialsRef: zabbixv1alpha1.CredentialsReference{SecretName: "creds"},
		},
	}
	if err := k8s.Create(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

// waitReady waits until the database's Ready condition has the wanted status and reason.
func waitReady(t *testing.T, ns string, want metav1.ConditionStatus, reason string) *zabbixv1alpha1.ZabbixDatabase {
	t.Helper()
	db := &zabbixv1alpha1.ZabbixDatabase{}
	eventually(t, waitFor, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "zabbix-db"}, db); err != nil {
			return err
		}
		c := meta.FindStatusCondition(db.Status.Conditions, zabbixv1alpha1.DatabaseReady)
		if c == nil || c.Status != want || c.Reason != reason {
			return fmt.Errorf("Ready = %+v, want %s/%s", c, want, reason)
		}
		return nil
	})
	return db
}

func TestDatabase_ReadyWithHealthyCluster(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	createSecret(t, ns)
	createCluster(t, ns)
	createDatabase(t, ns)

	db := waitReady(t, ns, metav1.ConditionTrue, ReasonReady)
	if db.Status.CurrentPrimary != primary1 {
		t.Errorf("CurrentPrimary = %q, want pg-1", db.Status.CurrentPrimary)
	}
	if db.Spec.Port != 5432 || db.Spec.Database != "zabbix" || db.Spec.CredentialsRef.PasswordKey != "password" {
		t.Errorf("API defaults not applied: %+v", db.Spec)
	}
}

func TestDatabase_RecoversWhenClusterAppears(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	createSecret(t, ns)
	createDatabase(t, ns)
	waitReady(t, ns, metav1.ConditionFalse, ReasonClusterNotFound)

	createCluster(t, ns)
	waitReady(t, ns, metav1.ConditionTrue, ReasonReady)
}

func TestDatabase_FollowsSwitchover(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	createSecret(t, ns)
	c := createCluster(t, ns)
	createDatabase(t, ns)
	waitReady(t, ns, metav1.ConditionTrue, ReasonReady)

	setClusterStatus(t, c, primary1, primary2)
	waitReady(t, ns, metav1.ConditionFalse, ReasonSwitchoverInProgress)

	setClusterStatus(t, c, primary2, primary2)
	db := waitReady(t, ns, metav1.ConditionTrue, ReasonReady)
	if db.Status.CurrentPrimary != primary2 || db.Status.PrimaryChanges != 1 {
		t.Errorf("primary %q changes %d, want pg-2 and 1", db.Status.CurrentPrimary, db.Status.PrimaryChanges)
	}
}

func TestDatabase_FollowsSecretLifecycle(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	s := createSecret(t, ns)
	createCluster(t, ns)
	db := createDatabase(t, ns)
	waitReady(t, ns, metav1.ConditionTrue, ReasonReady)

	if err := k8s.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ns, metav1.ConditionFalse, ReasonSecretNotFound)

	eventually(t, waitFor, func() error {
		events := &eventsv1.EventList{}
		if err := k8s.List(context.Background(), events, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.Regarding.UID == db.UID && e.Type == corev1.EventTypeWarning && e.Reason == ReasonSecretNotFound {
				return nil
			}
		}
		return fmt.Errorf("no Warning event %s", ReasonSecretNotFound)
	})

	createSecret(t, ns)
	waitReady(t, ns, metav1.ConditionTrue, ReasonReady)
}

func TestDatabase_RejectsVerifyWithoutCA(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	db := &zabbixv1alpha1.ZabbixDatabase{
		ObjectMeta: metav1.ObjectMeta{Name: "zabbix-db", Namespace: ns},
		Spec: zabbixv1alpha1.ZabbixDatabaseSpec{
			ClusterRef:     zabbixv1alpha1.LocalObjectReference{Name: "pg"},
			CredentialsRef: zabbixv1alpha1.CredentialsReference{SecretName: "creds"},
			TLS:            &zabbixv1alpha1.DatabaseTLS{Mode: "verify-full"},
		},
	}
	if err := k8s.Create(context.Background(), db); err == nil {
		t.Fatal("verify-full without caSecretRef was accepted")
	}
	db.Spec.TLS.CASecretRef = &zabbixv1alpha1.SecretKeyReference{Name: "ca", Key: "ca.crt"}
	if err := k8s.Create(context.Background(), db); err != nil {
		t.Fatalf("valid TLS spec rejected: %v", err)
	}
}
