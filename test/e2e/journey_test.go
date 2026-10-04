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

// Images of Zabbix 8.0.0rc1, pinned by digest: release candidates are not published under
// release tags.
const (
	rc1Server     = "zabbix/zabbix-server-pgsql@sha256:81fbf1898e72c688cdc93c0a2b5c9fb7c9be25deba40d77de9582a59defa264b"
	rc1Web        = "zabbix/zabbix-web-nginx-pgsql@sha256:08d00e9cdb7266ce134cf19447e05d7c19ab388c7ec1de38979fe2ee8b224fb9"
	rc1WebService = "zabbix/zabbix-web-service@sha256:ac12c16702d24ab5b6eab008d7d020ed030379bb876764884b0eaa2b635c3069"
	rc1Proxy      = "zabbix/zabbix-proxy-sqlite3@sha256:20fd82bf718c174ce17b205ebe1f0a5e0bd24e9c5393a560cc85c8cdb3a292a0"
	rc1Agent      = "zabbix/zabbix-agent2@sha256:d6b3f0ccf1a0e032a9ddaa9d3f81e8f828f4d9aea11382b5f5d5e099ce01fa9f"
)

// TestJourney runs one installation through its life in order: install 7.4.7, HA failover,
// agents (first blocked by another agent on the host port), a proxy with registration,
// suspend and resume, and the schema upgrade to 8.0.0rc1.
func TestJourney(t *testing.T) {
	j := &journey{ns: namespace(t, "journey")}
	ns := j.ns
	database(t, ns, 16)
	apply(t, ns, `apiVersion: zabbix.io/v1alpha1
kind: ZabbixSystem
metadata: { name: zabbix }
spec:
  version: "7.4.7"
  databaseRef: { name: zabbix-db }
  upgrade: { requireBackupWithin: 0s }
`)
	t.Run("Install", j.install)
	t.Run("Failover", j.failover)
	t.Run("AgentsAndProxy", j.agentsAndProxy)
	t.Run("SuspendAndResume", j.suspendAndResume)
	t.Run("UpgradeTo80", j.upgradeTo80)
}

// journey is the installation the stages of TestJourney share.
type journey struct {
	ns    string
	token string // Zabbix API token, created in agentsAndProxy
}

func (j *journey) install(t *testing.T) {
	ns := j.ns
	{
		start := time.Now()
		waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "active, 1 standby", 10*time.Minute)
		t.Logf("Running %s after creation", time.Since(start).Round(time.Second))
		active := activePods(t, ns)
		if len(active) != 1 {
			t.Fatalf("%d active server pods", len(active))
		}
		if eps := serverEndpoints(t, ns); !slices.Equal(eps, []string{active[0].Name}) {
			t.Errorf("zabbix-server endpoints %v, want only %s", eps, active[0].Name)
		}
		pods := &corev1.PodList{}
		if err := e.c.List(ctx(), pods, client.InNamespace(ns),
			client.MatchingLabels{"zabbix.io/component": "server"}); err != nil {
			t.Fatal(err)
		}
		for _, p := range pods.Items {
			if !p.Status.ContainerStatuses[0].Ready {
				t.Errorf("server pod %s is not Ready (standby nodes must be 1/1)", p.Name)
			}
		}
	}
}

func (j *journey) failover(t *testing.T) {
	ns := j.ns
	{
		old := activePods(t, ns)[0]
		start := time.Now()
		if err := e.c.Delete(ctx(), &old); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 2*time.Minute, "a new active node behind the Service", func() error {
			active := activePods(t, ns)
			if len(active) != 1 || active[0].Name == old.Name {
				return fmt.Errorf("active %d", len(active))
			}
			if eps := serverEndpoints(t, ns); !slices.Equal(eps, []string{active[0].Name}) {
				return fmt.Errorf("endpoints %v", eps)
			}
			return nil
		})
		t.Logf("Service routed to the new active node %s after deleting %s",
			time.Since(start).Round(100*time.Millisecond), old.Name)
		waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "active, 1 standby", 3*time.Minute)
	}
}

func (j *journey) agentsAndProxy(t *testing.T) {
	ns := j.ns
	{
		// Another agent already holds host port 10050 on every node.
		apply(t, ns, `apiVersion: apps/v1
kind: DaemonSet
metadata: { name: other-agent }
spec:
  selector: { matchLabels: { app: other-agent } }
  template:
    metadata: { labels: { app: other-agent } }
    spec:
      tolerations: [{ operator: Exists }]
      containers:
        - name: sleep
          image: busybox:1.37
          command: [sleep, infinity]
          ports: [{ containerPort: 10050, hostPort: 10050 }]
`)
		waitFor(t, 3*time.Minute, "the other agent on every node", func() error {
			ds := &appsv1.DaemonSet{}
			if err := e.c.Get(ctx(), client.ObjectKey{Namespace: ns, Name: "other-agent"}, ds); err != nil {
				return err
			}
			if ds.Status.DesiredNumberScheduled == 0 || ds.Status.NumberReady != ds.Status.DesiredNumberScheduled {
				return fmt.Errorf("%d/%d ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
			}
			return nil
		})

		web := forward(t, ns, "zabbix-web-0", 8080)
		var session string
		login := map[string]string{"username": "Admin", "password": "zabbix"}
		if err := zabbixAPI(t, web, "", "user.login", login, &session); err != nil {
			t.Fatal(err)
		}
		var created struct {
			IDs []string `json:"tokenids"`
		}
		newToken := map[string]string{"name": "e2e", "userid": "1"}
		if err := zabbixAPI(t, web, session, "token.create", newToken, &created); err != nil {
			t.Fatal(err)
		}
		var generated []struct {
			Token string `json:"token"`
		}
		if err := zabbixAPI(t, web, session, "token.generate", created.IDs, &generated); err != nil || len(generated) != 1 {
			t.Fatalf("token.generate: %v", err)
		}
		j.token = generated[0].Token
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zabbix-api-token"},
			StringData: map[string]string{"token": j.token}}
		if err := e.c.Create(ctx(), secret); err != nil {
			t.Fatal(err)
		}

		patchSpec(t, ns, map[string]any{
			"agent": map[string]any{"enabled": true, "image": "zabbix/zabbix-agent2:ubuntu-7.4.7",
				"tolerations": []any{map[string]any{"operator": "Exists"}}},
			"proxies": []any{map[string]any{"name": "e2e-proxy", "mode": "active", "replicas": 1}},
			"proxyRegistration": map[string]any{"enabled": true,
				"apiTokenSecretRef": map[string]any{"name": "zabbix-api-token", "key": "token"}},
		})
		waitFor(t, 3*time.Minute, "the agent component explaining the port conflict", func() error {
			for _, c := range system(t, ns).Status.Components {
				if c.Name == "agent" && strings.Contains(c.Message, "free ports") {
					return nil
				}
			}
			return fmt.Errorf("components %+v", system(t, ns).Status.Components)
		})

		other := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "other-agent"}}
		if err := e.c.Delete(ctx(), other); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 5*time.Minute, "agents on every node, the proxy registered and online", func() error {
			sys := system(t, ns)
			for _, c := range sys.Status.Components {
				if (c.Name == "agent" || c.Name == "proxy/e2e-proxy") && (c.Desired == 0 || c.Ready != c.Desired) {
					return fmt.Errorf("%s %d/%d %s", c.Name, c.Ready, c.Desired, c.Message)
				}
			}
			if !slices.Contains(sys.Status.RegisteredProxies, "e2e-proxy-0") {
				return fmt.Errorf("registered %v", sys.Status.RegisteredProxies)
			}
			var proxies []struct {
				Name, State, Compatibility string
			}
			query := map[string]any{"output": []string{"name", "state", "compatibility"}}
			if err := zabbixAPI(t, web, j.token, "proxy.get", query, &proxies); err != nil {
				return err
			}
			for _, p := range proxies {
				if p.Name == "e2e-proxy-0" && p.State == "2" && p.Compatibility == "1" {
					return nil
				}
			}
			return fmt.Errorf("proxies %+v", proxies)
		})
	}
}

func (j *journey) suspendAndResume(t *testing.T) {
	ns := j.ns
	{
		patchSpec(t, ns, map[string]any{"suspend": true})
		waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspended: all Zabbix pods are stopped", 5*time.Minute)
		pods := &corev1.PodList{}
		if err := e.c.List(ctx(), pods, client.InNamespace(ns),
			client.MatchingLabels{"app.kubernetes.io/managed-by": "zabbix-operator"}); err != nil {
			t.Fatal(err)
		}
		for _, p := range pods.Items {
			if p.Labels["zabbix.io/job"] == "" {
				t.Errorf("pod %s still exists while suspended", p.Name)
			}
		}
		if n := sql(t, ns, "select count(*) from ha_node where status <> 1"); n != "0" {
			t.Errorf("%s ha_node rows not stopped cleanly", n)
		}
		patchSpec(t, ns, map[string]any{"suspend": false})
		waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "active, 1 standby", 5*time.Minute)
	}
}

func (j *journey) upgradeTo80(t *testing.T) {
	ns := j.ns
	{
		start := time.Now()
		patchSpec(t, ns, map[string]any{
			"version":    "8.0.0rc1",
			"upgrade":    map[string]any{"approveMajor": "8.0"},
			"server":     map[string]any{"image": rc1Server},
			"web":        map[string]any{"image": rc1Web},
			"webService": map[string]any{"image": rc1WebService},
			"agent":      map[string]any{"image": rc1Agent},
			"proxies":    []any{map[string]any{"name": "e2e-proxy", "mode": "active", "replicas": 1, "image": rc1Proxy}},
		})
		waitFor(t, 10*time.Minute, "Running on 8.0.0rc1", func() error {
			sys := system(t, ns)
			if sys.Status.ObservedGeneration != sys.Generation || sys.Status.RunningVersion != "8.0.0rc1" ||
				sys.Status.Phase != zabbixv1alpha1.PhaseRunning || !strings.Contains(sys.Status.PhaseReason, "active, 1 standby") {
				return fmt.Errorf("generation %d/%d, running %q, %s %q", sys.Status.ObservedGeneration, sys.Generation,
					sys.Status.RunningVersion, sys.Status.Phase, sys.Status.PhaseReason)
			}
			return nil
		})
		t.Logf("upgraded 7.4.7 -> 8.0.0rc1 in %s", time.Since(start).Round(time.Second))
		if v := sql(t, ns, "select mandatory from dbversion"); v != "7050195" {
			t.Errorf("dbversion %s, want 7050195", v)
		}
		web := forward(t, ns, "zabbix-web-0", 8080)
		waitFor(t, 5*time.Minute, "the proxy current with the 8.0 server", func() error {
			var proxies []struct {
				Name, State, Compatibility, Version string
			}
			query := map[string]any{"output": []string{"name", "state", "compatibility", "version"}}
			if err := zabbixAPI(t, web, j.token, "proxy.get", query, &proxies); err != nil {
				return err
			}
			for _, p := range proxies {
				if p.Name == "e2e-proxy-0" && p.State == "2" && p.Compatibility == "1" && strings.HasPrefix(p.Version, "80") {
					return nil
				}
			}
			return fmt.Errorf("proxies %+v", proxies)
		})
	}
}
