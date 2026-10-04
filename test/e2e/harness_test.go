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

// Package e2e runs the operator's end-to-end suites against a dedicated kind cluster
// (hack/e2e-kind.sh). It refuses any other cluster.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

// env is the cluster under test.
type env struct {
	cfg *rest.Config
	c   client.Client
	cs  *kubernetes.Clientset
}

var e *env

func TestMain(m *testing.M) {
	path, ctxName := os.Getenv("E2E_KUBECONFIG"), os.Getenv("E2E_CONTEXT")
	if path == "" || !strings.HasPrefix(ctxName, "kind-") {
		fmt.Fprintln(os.Stderr, "E2E_KUBECONFIG and a kind context in E2E_CONTEXT are required (run hack/e2e-kind.sh)")
		os.Exit(2)
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
		&clientcmd.ConfigOverrides{CurrentContext: ctxName}).ClientConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if !strings.Contains(cfg.Host, "127.0.0.1") && !strings.Contains(cfg.Host, "localhost") {
		fmt.Fprintf(os.Stderr, "refusing %s: the e2e suite only runs against a local kind cluster\n", cfg.Host)
		os.Exit(2)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(zabbixv1alpha1.AddToScheme(scheme))
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	e = &env{cfg: cfg, c: c, cs: kubernetes.NewForConfigOrDie(cfg)}
	os.Exit(m.Run())
}

// waitFor polls check every 2 s until it returns nil or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s not reached within %s: %v", what, timeout, err)
}

func ctx() context.Context { return context.Background() }

// namespace creates a fresh namespace, deleted when the test ends unless E2E_KEEP is set.
func namespace(t *testing.T, prefix string) string {
	t.Helper()
	ns := &corev1.Namespace{}
	ns.GenerateName = prefix + "-"
	ns.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "privileged"}
	if err := e.c.Create(ctx(), ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if os.Getenv("E2E_KEEP") == "" {
			_ = e.c.Delete(ctx(), ns)
		}
	})
	return ns.Name
}

// apply creates objects from YAML documents in ns.
func apply(t *testing.T, ns, manifest string) {
	t.Helper()
	for _, doc := range strings.Split(manifest, "\n---\n") {
		u := &unstructured.Unstructured{}
		if err := yamlToUnstructured(doc, u); err != nil {
			t.Fatal(err)
		}
		if len(u.Object) == 0 {
			continue
		}
		u.SetNamespace(ns)
		if err := e.c.Create(ctx(), u); err != nil {
			t.Fatalf("creating %s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}
}

// database creates a CNPG cluster of PostgreSQL major pg with credentials and a
// ZabbixDatabase zabbix-db, and waits until the ZabbixDatabase is Ready.
func database(t *testing.T, ns string, pg int) {
	t.Helper()
	apply(t, ns, fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata: { name: zabbix-db-creds }
type: kubernetes.io/basic-auth
stringData: { username: zabbix, password: e2e-only }
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: { name: zabbix-pg }
spec:
  instances: 2
  imageName: ghcr.io/cloudnative-pg/postgresql:%d
  storage: { size: 2Gi }
  bootstrap:
    initdb: { database: zabbix, owner: zabbix, secret: { name: zabbix-db-creds } }
---
apiVersion: zabbix.io/v1alpha1
kind: ZabbixDatabase
metadata: { name: zabbix-db }
spec:
  clusterRef: { name: zabbix-pg }
  credentialsRef: { secretName: zabbix-db-creds }
`, pg))
	waitFor(t, 6*time.Minute, "ZabbixDatabase Ready", func() error {
		db := &zabbixv1alpha1.ZabbixDatabase{}
		if err := e.c.Get(ctx(), client.ObjectKey{Namespace: ns, Name: "zabbix-db"}, db); err != nil {
			return err
		}
		for _, c := range db.Status.Conditions {
			if c.Type == zabbixv1alpha1.DatabaseReady && c.Status == "True" {
				return nil
			}
		}
		return fmt.Errorf("conditions %+v", db.Status.Conditions)
	})
}

// systemName is the name of the ZabbixSystem every scenario creates.
const systemName = "zabbix"

func system(t *testing.T, ns string) *zabbixv1alpha1.ZabbixSystem {
	t.Helper()
	sys := &zabbixv1alpha1.ZabbixSystem{}
	if err := e.c.Get(ctx(), client.ObjectKey{Namespace: ns, Name: systemName}, sys); err != nil {
		t.Fatal(err)
	}
	return sys
}

// waitPhase waits until the system reports phase with a reason containing reason.
func waitPhase(t *testing.T, ns string, phase zabbixv1alpha1.SystemPhase, reason string,
	timeout time.Duration) *zabbixv1alpha1.ZabbixSystem {
	t.Helper()
	var sys *zabbixv1alpha1.ZabbixSystem
	waitFor(t, timeout, fmt.Sprintf("phase %s %q", phase, reason), func() error {
		sys = system(t, ns)
		if sys.Status.Phase != phase || !strings.Contains(sys.Status.PhaseReason, reason) {
			return fmt.Errorf("phase %s %q", sys.Status.Phase, sys.Status.PhaseReason)
		}
		return nil
	})
	return sys
}

// patchSpec merge-patches the system's spec.
func patchSpec(t *testing.T, ns string, spec map[string]any) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"spec": spec})
	sys := &zabbixv1alpha1.ZabbixSystem{}
	sys.Namespace, sys.Name = ns, systemName
	if err := e.c.Patch(ctx(), sys, client.RawPatch("application/merge-patch+json", data)); err != nil {
		t.Fatal(err)
	}
}

// execIn runs a command in a container and returns its standard output.
func execIn(t *testing.T, ns, pod, container string, cmd ...string) (string, error) {
	t.Helper()
	req := e.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: cmd, Stdout: true, Stderr: true},
			clientgoscheme.ParameterCodec)
	ex, err := remotecommand.NewSPDYExecutor(e.cfg, http.MethodPost, req.URL())
	if err != nil {
		return "", err
	}
	var out, errOut bytes.Buffer
	if err := ex.StreamWithContext(ctx(), remotecommand.StreamOptions{Stdout: &out, Stderr: &errOut}); err != nil {
		return "", fmt.Errorf("%w: %s", err, errOut.String())
	}
	return strings.TrimSpace(out.String()), nil
}

// sql runs a query on the CNPG primary of zabbix-pg as postgres and returns the
// unaligned, tuples-only output.
func sql(t *testing.T, ns, query string) string {
	t.Helper()
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"})
	if err := e.c.Get(ctx(), client.ObjectKey{Namespace: ns, Name: "zabbix-pg"}, cluster); err != nil {
		t.Fatal(err)
	}
	primary, _, _ := unstructured.NestedString(cluster.Object, "status", "currentPrimary")
	out, err := execIn(t, ns, primary, "postgres", "psql", "-U", "postgres", "-d", "zabbix", "-At", "-c", query)
	if err != nil {
		t.Fatalf("sql %q: %v", query, err)
	}
	return out
}

// forward opens a port-forward to port of pod and returns the local address.
func forward(t *testing.T, ns, pod string, port int) string {
	t.Helper()
	rt, upgrader, err := spdy.RoundTripperFor(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(e.cfg.Host)
	u.Path = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", ns, pod)
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, u)
	stop, ready := make(chan struct{}), make(chan struct{})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = fw.ForwardPorts() }()
	t.Cleanup(func() { close(stop) })
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("port-forward not ready")
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		t.Fatalf("port-forward ports: %v", err)
	}
	return fmt.Sprintf("127.0.0.1:%d", ports[0].Local)
}

// zabbixAPI calls the Zabbix JSON-RPC API at addr, with a bearer token when token is set.
func zabbixAPI(t *testing.T, addr, token, method string, params any, out any) error {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": 1})
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api_jsonrpc.php", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json-rpc")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message, Data string
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %s %s", method, r.Error.Message, r.Error.Data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

func yamlToUnstructured(doc string, u *unstructured.Unstructured) error {
	return yaml.Unmarshal([]byte(doc), &u.Object)
}

// activePods returns the server pods labelled active.
func activePods(t *testing.T, ns string) []corev1.Pod {
	t.Helper()
	pods := &corev1.PodList{}
	if err := e.c.List(ctx(), pods, client.InNamespace(ns),
		client.MatchingLabels{"zabbix.io/component": "server", "zabbix.io/role": "active"}); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

// serverEndpoints returns the names of the ready pods behind the zabbix-server Service,
// the operator's or a pre-existing one.
func serverEndpoints(t *testing.T, ns string) []string {
	t.Helper()
	slices := &discoveryv1.EndpointSliceList{}
	if err := e.c.List(ctx(), slices, client.InNamespace(ns),
		client.MatchingLabels{"kubernetes.io/service-name": "zabbix-server"}); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			if ep.TargetRef != nil && (ep.Conditions.Ready == nil || *ep.Conditions.Ready) {
				out = append(out, ep.TargetRef.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}
