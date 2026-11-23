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
	"os"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

// maxCRDBytes keeps every CRD below the 256 KiB limit of the last-applied annotation
// used by client-side apply, with headroom.
const maxCRDBytes = 220 * 1024

func TestCRDsFitClientSideApply(t *testing.T) {
	entries, err := os.ReadDir("../../config/crd/bases")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > maxCRDBytes {
			t.Errorf("%s is %d bytes, above %d", e.Name(), info.Size(), maxCRDBytes)
		}
	}
}

func newSystem(ns, version string) *zabbixv1alpha1.ZabbixSystem {
	return &zabbixv1alpha1.ZabbixSystem{
		ObjectMeta: metav1.ObjectMeta{Name: "zabbix", Namespace: ns},
		Spec: zabbixv1alpha1.ZabbixSystemSpec{
			Version:     version,
			DatabaseRef: &zabbixv1alpha1.LocalObjectReference{Name: "zabbix-db"},
		},
	}
}

// expectRejected fails unless err is a validation error mentioning want.
func expectRejected(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("accepted, want rejection mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("rejected with %q, want it to mention %q", err, want)
	}
}

func TestSystemAPI_Defaults(t *testing.T) {
	requireEnvtest(t)
	sys := newSystem(newNamespace(t), "7.0.25")
	if err := k8s.Create(context.Background(), sys); err != nil {
		t.Fatal(err)
	}
	s := sys.Spec
	if s.ImageRepository != "zabbix" || s.ImageFlavor != "ubuntu" {
		t.Errorf("image defaults: %q %q", s.ImageRepository, s.ImageFlavor)
	}
	if s.Server.Replicas != 2 || s.Web.Replicas != 1 || s.WebService.Replicas != 1 {
		t.Errorf("replica defaults: server %d web %d webservice %d", s.Server.Replicas, s.Web.Replicas, s.WebService.Replicas)
	}
	if !ptr.Deref(s.Web.Enabled, false) || !ptr.Deref(s.WebService.Enabled, false) {
		t.Errorf("enabled defaults: web %v webservice %v", s.Web.Enabled, s.WebService.Enabled)
	}
	if s.Upgrade.RequireBackupWithin == nil || s.Upgrade.RequireBackupWithin.Hours() != 24 {
		t.Errorf("requireBackupWithin default: %v", s.Upgrade.RequireBackupWithin)
	}
	if s.Server.Service.Type != corev1.ServiceTypeClusterIP || s.Web.Service.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service type defaults: %q %q", s.Server.Service.Type, s.Web.Service.Type)
	}
	if s.Agent.Enabled || s.ProxyRegistration.Enabled {
		t.Error("agent and proxy registration are off by default")
	}
}

func TestSystemAPI_Version(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	// Any well-formed version is accepted; unsupported release lines are reported as
	// Blocked at runtime rather than rejected here.
	for i, v := range []string{"7.0.0", "7.0.1", "7.0.25", "8.0.0", "8.0.0rc1", "8.0.12", "8.0.1beta2", "9.0.0", "7.4.0", "6.0.30"} {
		sys := newSystem(ns, v)
		sys.Name = "ok-" + string(rune('a'+i))
		if err := k8s.Create(context.Background(), sys); err != nil {
			t.Errorf("version %s rejected: %v", v, err)
		}
	}
	for i, v := range []string{"7.0", "7.0.01", "8.0.0-rc1", "v7.0.1", "7.0.1rc", "07.0.1", "7.00.1", ""} {
		sys := newSystem(ns, v)
		sys.Name = "bad-" + string(rune('a'+i))
		expectRejected(t, k8s.Create(context.Background(), sys), "spec.version")
	}
}

// Any well-formed version change is accepted by the API, including a lower one: whether it
// may run is decided against the running version and the database (see the controller
// tests), so a blocked request can always be withdrawn.
func TestSystemAPI_VersionChangesAreAccepted(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	sys := newSystem(newNamespace(t), "7.0.25")
	if err := k8s.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"8.0.0rc1", "7.0.30", "7.0.1", "8.0.3"} {
		before := sys.DeepCopy()
		sys.Spec.Version = v
		if err := k8s.Patch(ctx, sys, client.MergeFrom(before)); err != nil {
			t.Fatalf("change to %s rejected: %v", v, err)
		}
	}
}

func TestSystemAPI_DatabaseRefImmutable(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	sys := newSystem(newNamespace(t), "7.0.25")
	if err := k8s.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	before := sys.DeepCopy()
	sys.Spec.DatabaseRef.Name = "other"
	expectRejected(t, k8s.Patch(ctx, sys, client.MergeFrom(before)), "immutable")
}

func TestSystemAPI_FieldRules(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	cases := []struct {
		name   string
		mutate func(*zabbixv1alpha1.ZabbixSystemSpec)
		reject string // empty: must be accepted
	}{
		{"server needs one replica", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Server.Replicas = -1 }, "spec.server.replicas"},
		{"web may be disabled", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Web.Enabled = ptr.To(false) }, ""},
		{"web needs one replica", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Web.Replicas = -1 }, "spec.web.replicas"},
		{"proxy may be disabled", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			s.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "dc1", Enabled: ptr.To(false)}}
		}, ""},
		{"agent needs an image", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Agent.Enabled = true }, "spec.agent.image is required"},
		{"agent with image", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			s.Agent.Enabled, s.Agent.Image = true, "zabbix/zabbix-agent2:ubuntu-7.0.25"
		}, ""},
		{"registration needs a token", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.ProxyRegistration.Enabled = true }, "apiTokenSecretRef is required"},
		{"approveMajor must be a line", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Upgrade.ApproveMajor = "8.0.0" }, "spec.upgrade.approveMajor"},
		{"approveMajor 8.0", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Upgrade.ApproveMajor = "8.0" }, ""},
		{"duplicate proxy names", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			s.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "dc1"}, {Name: "dc1"}}
		}, "Duplicate value"},
		{"proxy name must be a DNS label", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			s.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "DC_1"}}
		}, "spec.proxies[0].name"},
		{"unknown service type", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Server.Service.Type = "ExternalName" }, "spec.server.service.type"},
		{"edge system", edge, ""},
		{"edge without a database", func(s *zabbixv1alpha1.ZabbixSystemSpec) { edge(s); s.DatabaseRef = nil }, ""},
		{"local server needs a database", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.DatabaseRef = nil }, "spec.databaseRef is required"},
		{"edge needs remote addresses", func(s *zabbixv1alpha1.ZabbixSystemSpec) { edge(s); s.Server.RemoteAddresses = nil }, "spec.server.remoteAddresses is required"},
		{"remote addresses only on edge", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Server.RemoteAddresses = []string{"10.0.0.1"} }, "spec.server.remoteAddresses is required"},
		{"remote address syntax", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			edge(s)
			s.Server.RemoteAddresses = []string{"10.0.0.1;10.0.0.2"}
		}, "spec.server.remoteAddresses[0]"},
		{"edge runs no frontend", func(s *zabbixv1alpha1.ZabbixSystemSpec) { edge(s); s.Web.Enabled = nil }, "spec.web.enabled and spec.webService.enabled must be false"},
		{"edge runs no web service", func(s *zabbixv1alpha1.ZabbixSystemSpec) { edge(s); s.WebService.Enabled = ptr.To(true) }, "spec.web.enabled and spec.webService.enabled must be false"},
		{"edge proxies are active", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			edge(s)
			s.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "edge", Mode: zabbixv1alpha1.ProxyPassive}}
		}, "proxies must be active"},
		{"edge registration needs the remote URL", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			edge(s)
			s.ProxyRegistration = zabbixv1alpha1.ProxyRegistrationSpec{Enabled: true,
				APITokenSecretRef: &zabbixv1alpha1.SecretKeyReference{Name: "t", Key: "token"}}
		}, "spec.proxyRegistration.url"},
		{"agent reports to a proxy", func(s *zabbixv1alpha1.ZabbixSystemSpec) {
			edge(s)
			s.Agent = zabbixv1alpha1.AgentSpec{Enabled: true, ReportTo: "edge", PodSettings: zabbixv1alpha1.PodSettings{Image: "zabbix/zabbix-agent2:ubuntu-7.0.25"}}
		}, ""},
		{"agent reportTo must name a proxy", func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Agent.ReportTo = "nope" }, "spec.agent.reportTo must name a proxy"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sys := newSystem(ns, "7.0.25")
			sys.Name = "rule-" + string(rune('a'+i))
			tc.mutate(&sys.Spec)
			err := k8s.Create(context.Background(), sys)
			if tc.reject == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			expectRejected(t, err, tc.reject)
		})
	}
}

// Fields stored without a schema must round-trip unchanged.
func TestSystemAPI_SchemalessFieldsRoundTrip(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	sys := newSystem(newNamespace(t), "7.0.25")
	sys.Spec.Server.PodSecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1997), FSGroup: ptr.To[int64](1995)}
	sys.Spec.Server.ExtraContainers = []zabbixv1alpha1.Container{zabbixv1alpha1.ContainerOf(
		corev1.Container{Name: "agent", Image: "zabbix/zabbix-agent2:ubuntu-7.0.11"})}
	sys.Spec.Web.Volumes = []zabbixv1alpha1.Volume{zabbixv1alpha1.VolumeOf(corev1.Volume{Name: "conf", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "zabbix-web-conf"}}}})}
	if err := k8s.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	got := &zabbixv1alpha1.ZabbixSystem{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(sys), got); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(got.Spec.Server.PodSecurityContext.RunAsUser, 0) != 1997 ||
		len(got.Spec.Server.ExtraContainers) != 1 || got.Spec.Server.ExtraContainers[0].Get().Name != "agent" ||
		len(got.Spec.Web.Volumes) != 1 || got.Spec.Web.Volumes[0].Get().ConfigMap.Name != "zabbix-web-conf" {
		t.Errorf("schemaless fields changed: %+v", got.Spec)
	}
}

// TestDocExamplesAreValid applies every ZabbixDatabase and ZabbixSystem example in the
// architecture document, so the documentation cannot drift from the API.
func TestDocExamplesAreValid(t *testing.T) {
	requireEnvtest(t)
	doc, err := os.ReadFile("../../docs/architecture.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(doc), -1)
	ns := newNamespace(t)
	found := 0
	for _, b := range blocks {
		if !strings.Contains(b[1], "apiVersion: zabbix.io/") {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(b[1]), &obj.Object); err != nil {
			t.Fatalf("example does not parse: %v\n%s", err, b[1])
		}
		obj.SetNamespace(ns)
		if err := k8s.Create(context.Background(), obj, client.DryRunAll); err != nil {
			t.Errorf("%s example rejected: %v", obj.GetKind(), err)
		}
		found++
	}
	if found < 2 {
		t.Fatalf("found %d API examples in the architecture document, want at least 2", found)
	}
}

// edge makes spec an edge system reporting to a remote server.
func edge(s *zabbixv1alpha1.ZabbixSystemSpec) {
	s.Server.Enabled = ptr.To(false)
	s.Server.RemoteAddresses = []string{"10.11.135.132", "zbx-2.example.com:10051"}
	s.Web.Enabled, s.WebService.Enabled = ptr.To(false), ptr.To(false)
	s.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "edge"}}
}

func TestSystemAPI_ServerModeImmutable(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t)
	sys := newSystem(ns, "7.0.25")
	edge(&sys.Spec)
	if err := k8s.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	before := sys.DeepCopy()
	sys.Spec.Server.Enabled = ptr.To(true)
	sys.Spec.Server.RemoteAddresses = nil
	expectRejected(t, k8s.Patch(ctx, sys, client.MergeFrom(before)), "spec.server.enabled is immutable")

	// An edge system created without a database cannot gain one later.
	bare := newSystem(ns, "7.0.25")
	bare.Name, bare.Spec.DatabaseRef = "bare", nil
	edge(&bare.Spec)
	if err := k8s.Create(ctx, bare); err != nil {
		t.Fatal(err)
	}
	before = bare.DeepCopy()
	bare.Spec.DatabaseRef = &zabbixv1alpha1.LocalObjectReference{Name: "zabbix-db"}
	expectRejected(t, k8s.Patch(ctx, bare, client.MergeFrom(before)), "spec.databaseRef is immutable")
}

// A manifest that leaves sections out entirely (as kubectl sends it) still gets every
// default: replicas, enabled flags and Service types.
func TestSystemAPI_DefaultsForOmittedSections(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "zabbix.io/v1alpha1", "kind": "ZabbixSystem",
		"metadata": map[string]interface{}{"name": "minimal", "namespace": ns},
		"spec":     map[string]interface{}{"version": "7.0.1", "databaseRef": map[string]interface{}{"name": "zabbix-db"}},
	}}
	if err := k8s.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	sys := &zabbixv1alpha1.ZabbixSystem{}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "minimal"}, sys); err != nil {
		t.Fatal(err)
	}
	s := sys.Spec
	if s.Server.Replicas != 2 || s.Web.Replicas != 1 || s.WebService.Replicas != 1 ||
		!ptr.Deref(s.Web.Enabled, false) || !ptr.Deref(s.WebService.Enabled, false) ||
		s.Upgrade.RequireBackupWithin == nil || s.Server.Service.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("omitted sections not defaulted: %+v", s)
	}
}
