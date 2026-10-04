//go:build e2e

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

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

// TestMigration moves a running plain-manifest installation to the operator side by side:
// the operator's servers join the running HA cluster as standbys while the old Service is
// reported as a conflict and never routes to them; removing the old server hands over to
// the operator's nodes and the operator then creates its own Service.
func TestMigration(t *testing.T) {
	ns := namespace(t, "migration")
	database(t, ns, 16)
	apply(t, ns, `apiVersion: apps/v1
kind: Deployment
metadata: { name: zabbix-server }
spec:
  replicas: 1
  selector: { matchLabels: { app.kubernetes.io/name: zabbix-server } }
  template:
    metadata: { labels: { app.kubernetes.io/name: zabbix-server } }
    spec:
      containers:
        - name: zabbix-server
          image: zabbix/zabbix-server-pgsql:ubuntu-7.4.7
          env:
            - { name: DB_SERVER_HOST, value: zabbix-pg-rw }
            - { name: POSTGRES_DB, value: zabbix }
            - name: POSTGRES_USER
              valueFrom: { secretKeyRef: { name: zabbix-db-creds, key: username } }
            - name: POSTGRES_PASSWORD
              valueFrom: { secretKeyRef: { name: zabbix-db-creds, key: password } }
            - name: ZBX_HANODENAME
              valueFrom: { fieldRef: { fieldPath: metadata.name } }
            - name: ZBX_NODEADDRESS
              valueFrom: { fieldRef: { fieldPath: status.podIP } }
          ports: [{ containerPort: 10051 }]
---
apiVersion: v1
kind: Service
metadata: { name: zabbix-server }
spec:
  selector: { app.kubernetes.io/name: zabbix-server }
  ports: [{ port: 10051, targetPort: 10051 }]
`)
	waitFor(t, 8*time.Minute, "the old server active with its schema", func() error {
		if sql(t, ns, "select count(*) from pg_tables where tablename = 'ha_node'") != "1" {
			return fmt.Errorf("no schema yet")
		}
		if n := sql(t, ns, "select count(*) from ha_node where status = 3"); n != "1" {
			return fmt.Errorf("%s active nodes", n)
		}
		return nil
	})
	oldPods := &corev1.PodList{}
	if err := e.c.List(ctx(), oldPods, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/name": "zabbix-server"}); err != nil || len(oldPods.Items) != 1 {
		t.Fatalf("old server pods: %v", err)
	}
	oldPod := oldPods.Items[0].Name

	start := time.Now()
	apply(t, ns, `apiVersion: zabbix.io/v1alpha1
kind: ZabbixSystem
metadata: { name: zabbix }
spec:
  version: "7.4.7"
  databaseRef: { name: zabbix-db }
  upgrade: { requireBackupWithin: 0s }
`)
	waitFor(t, 8*time.Minute, "operator servers running as standbys next to the old server", func() error {
		sys := system(t, ns)
		conflict := false
		for _, c := range sys.Status.Conditions {
			if c.Type == zabbixv1alpha1.SystemConflict && c.Status == "True" &&
				strings.Contains(c.Message, "Service zabbix-server") {
				conflict = true
			}
		}
		if !conflict {
			return fmt.Errorf("no Conflict on Service zabbix-server: %+v", sys.Status.Conditions)
		}
		pods := &corev1.PodList{}
		if err := e.c.List(ctx(), pods, client.InNamespace(ns),
			client.MatchingLabels{"zabbix.io/component": "server"}); err != nil {
			return err
		}
		running := 0
		for _, p := range pods.Items {
			ready := len(p.Status.ContainerStatuses) > 0 && p.Status.ContainerStatuses[0].Ready
			if p.Labels["zabbix.io/role"] != "standby" || !ready {
				return fmt.Errorf("pod %s role %q not a ready standby", p.Name, p.Labels["zabbix.io/role"])
			}
			running++
		}
		if running != 2 {
			return fmt.Errorf("%d operator servers", running)
		}
		return nil
	})
	t.Logf("operator servers joined as standbys %s after creating the system", time.Since(start).Round(time.Second))
	if eps := serverEndpoints(t, ns); !slices.Equal(eps, []string{oldPod}) {
		t.Errorf("the old Service routes to %v, want only the old server %s", eps, oldPod)
	}

	start = time.Now()
	oldServer := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zabbix-server"}}
	if err := e.c.Delete(ctx(), oldServer); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Minute, "an operator server active", func() error {
		if len(activePods(t, ns)) != 1 {
			return fmt.Errorf("no active operator server")
		}
		return nil
	})
	t.Logf("an operator server became active %s after deleting the old Deployment",
		time.Since(start).Round(100*time.Millisecond))

	oldService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zabbix-server"}}
	if err := e.c.Delete(ctx(), oldService); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Minute, "the operator's own zabbix-server Service routing to the active node", func() error {
		svc := &corev1.Service{}
		if err := e.c.Get(ctx(), client.ObjectKey{Namespace: ns, Name: "zabbix-server"}, svc); err != nil {
			return err
		}
		ref := metav1.GetControllerOf(svc)
		if ref == nil || ref.Kind != "ZabbixSystem" || ref.Name != "zabbix" {
			return fmt.Errorf("Service owner %+v", ref)
		}
		active := activePods(t, ns)
		if len(active) != 1 {
			return fmt.Errorf("%d active", len(active))
		}
		if eps := serverEndpoints(t, ns); !slices.Equal(eps, []string{active[0].Name}) {
			return fmt.Errorf("endpoints %v", eps)
		}
		return nil
	})
	waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "active, 1 standby", 3*time.Minute)
}
