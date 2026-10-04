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

package registration

import (
	"context"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/registration/fakeapi"
	"github.com/sagh0900/zabbix-operator/internal/zabbixapi"
)

func TestParse(t *testing.T) {
	list, err := Parse([]byte(`
- name: proxy-dc1
- name: proxy-dc2
  mode: active
  proxyGroup: dc
  localAddress: dc2.example.com
- name: proxy-edge
  mode: passive
  address: edge.example.com
  port: 10052
  proxyGroup: dc
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Name: "proxy-dc1", Mode: ModeActive, Port: 10051},
		{Name: "proxy-dc2", Mode: ModeActive, Port: 10051, ProxyGroup: "dc", LocalAddress: "dc2.example.com", LocalPort: 10051},
		{Name: "proxy-edge", Mode: ModePassive, Address: "edge.example.com", Port: 10052, ProxyGroup: "dc",
			LocalAddress: "edge.example.com", LocalPort: 10051},
	}
	if !slices.Equal(list, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", list, want)
	}
	if list, err := Parse([]byte("")); err != nil || len(list) != 0 {
		t.Errorf("empty list: %v, %v", list, err)
	}

	for in, msg := range map[string]string{
		"- mode: active":                         "name",
		"- name: a\n  mode: standby":             "mode",
		"- name: a\n  mode: passive":             "address is required",
		"- name: a\n  proxyGroup: g":             "localAddress",
		"- name: a\n  hostname: typo":            "unknown field",
		"- name: a\n  port: 70000\n  address: x": "out of range",
		"name: a":                                "parsing",
	} {
		if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("Parse(%q) error = %v, want %q", in, err, msg)
		}
	}
}

func testSystem() *zabbixv1alpha1.ZabbixSystem {
	return &zabbixv1alpha1.ZabbixSystem{
		ObjectMeta: metav1.ObjectMeta{Namespace: "zbx", Name: "zabbix"},
		Spec: zabbixv1alpha1.ZabbixSystemSpec{Proxies: []zabbixv1alpha1.ProxySpec{
			{Name: "dc1", Mode: zabbixv1alpha1.ProxyActive, Replicas: 2, ProxyGroup: "dc1"},
			{Name: "edge", Mode: zabbixv1alpha1.ProxyPassive, Replicas: 1},
			{Name: "off", Enabled: ptr.To(false)},
		}},
	}
}

func TestDesired(t *testing.T) {
	sys := testSystem()
	got, err := Desired(sys, []Entry{{Name: "remote", Mode: ModeActive, Port: 10051}})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"remote", "dc1-0", "dc1-1", "edge-0"}) {
		t.Fatalf("names = %v", names)
	}
	if e := got[2]; e.Mode != ModeActive || e.Address != "" || e.ProxyGroup != "dc1" || e.LocalAddress != "dc1-1.dc1.zbx.svc" {
		t.Errorf("grouped active instance = %+v", e)
	}
	if e := got[3]; e.Mode != ModePassive || e.Address != "edge-0.edge.zbx.svc" || e.Port != 10051 || e.ProxyGroup != "" {
		t.Errorf("passive instance = %+v", e)
	}
	if _, err := Desired(sys, []Entry{{Name: "edge-0"}}); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("duplicate name error = %v", err)
	}
}

func TestDecide(t *testing.T) {
	desired := make([]Entry, 0, 5)
	desired = append(desired, []Entry{
		{Name: "new", Mode: ModeActive, Port: 10051},
		{Name: "same", Mode: ModePassive, Address: "a", Port: 10051},
		{Name: "moved", Mode: ModePassive, Address: "b", Port: 10051},
		{Name: "adopted", Mode: ModeActive, Port: 10051, ProxyGroup: "g", LocalAddress: "l", LocalPort: 10051},
	}...)
	existing := []zabbixapi.Proxy{
		{ProxyID: "1", Name: "same", OperatingMode: "1", Address: "a", Port: "10051", ProxyGroupID: "0"},
		{ProxyID: "2", Name: "moved", OperatingMode: "1", Address: "old", Port: "10051", ProxyGroupID: "0"},
		{ProxyID: "3", Name: "adopted", OperatingMode: "0", Address: "127.0.0.1", Port: "10051", ProxyGroupID: "0"},
		{ProxyID: "4", Name: "gone", OperatingMode: "0"},
		{ProxyID: "5", Name: "manual", OperatingMode: "0"},
		// An active proxy's address is not managed.
		{ProxyID: "6", Name: "active", OperatingMode: "0", Address: "127.0.0.1", Port: "10051", ProxyGroupID: "0"},
	}
	desired = append(desired, Entry{Name: "active", Mode: ModeActive, Port: 10051})
	groups := map[string]string{"g": "9"}
	registered := []string{"same", "gone", "vanished"}

	plan := Decide(desired, existing, groups, registered, false)
	if len(plan.Create) != 1 || plan.Create[0].Name != "new" || plan.Create[0].ProxyGroupID != "0" {
		t.Errorf("Create = %+v", plan.Create)
	}
	updated := make([]string, 0, len(plan.Update))
	for _, p := range plan.Update {
		updated = append(updated, p.Name+"/"+p.ProxyID)
	}
	if !slices.Equal(updated, []string{"moved/2", "adopted/3"}) {
		t.Errorf("Update = %v", updated)
	}
	if a := plan.Update[1]; a.ProxyGroupID != "9" || a.LocalAddress != "l" || a.LocalPort != "10051" {
		t.Errorf("adopted proxy = %+v", a)
	}
	if len(plan.Delete) != 0 || !slices.Equal(plan.Keep, []string{"gone"}) {
		t.Errorf("without prune: Delete %v Keep %v", plan.Delete, plan.Keep)
	}

	plan = Decide(desired, existing, groups, registered, true)
	if !slices.Equal(plan.Delete, []string{"4"}) || len(plan.Keep) != 0 {
		t.Errorf("with prune: Delete %v Keep %v (manual and vanished proxies must not be touched)", plan.Delete, plan.Keep)
	}
}

func TestSync(t *testing.T) {
	ctx := context.Background()
	api := fakeapi.New()
	manual := api.Add(zabbixapi.Proxy{Name: "manual", OperatingMode: "0"})
	desired, err := Desired(testSystem(), nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := Sync(ctx, api, desired, nil, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 3 || !slices.Equal(res.Registered, []string{"dc1-0", "dc1-1", "edge-0"}) {
		t.Fatalf("first sync = %+v", res)
	}
	g := api.Group("dc1")
	if p, _ := api.Get("dc1-1"); g == "" || p.ProxyGroupID != g || p.LocalAddress != "dc1-1.dc1.zbx.svc" {
		t.Errorf("dc1-1 = %+v, group %q", p, g)
	}

	// Nothing changed: no writes.
	res, err = Sync(ctx, api, desired, res.Registered, true, "")
	if err != nil || res.Created+res.Updated+res.Deleted != 0 || api.Count("proxygroup.create") != 1 {
		t.Fatalf("idempotent sync = %+v, %v, %v", res, err, api.Calls)
	}

	// A failed create is not recorded; the others are, and the error is returned.
	api.SetFail("proxy.create", true)
	more := append(slices.Clone(desired), Entry{Name: "extra", Mode: ModeActive, Port: 10051})
	res, err = Sync(ctx, api, more, res.Registered, true, "")
	if err == nil || slices.Contains(res.Registered, "extra") || len(res.Registered) != 3 {
		t.Fatalf("failed create = %+v, %v", res, err)
	}
	api.SetFail("proxy.create", false)

	// Scale-down with prune deletes only registered proxies; hand-made ones stay.
	res, err = Sync(ctx, api, desired[:1], res.Registered, true, "")
	if err != nil || res.Deleted != 2 || !slices.Equal(api.Names(), []string{"dc1-0", "manual"}) {
		t.Fatalf("prune = %+v, %v, %v", res, err, api.Names())
	}
	if _, ok := api.Get("manual"); !ok || manual == "" {
		t.Error("the hand-made proxy was removed")
	}

	// A failed prune keeps the names on record so it is retried.
	api.SetFail("proxy.delete", true)
	res, err = Sync(ctx, api, nil, res.Registered, true, "")
	if err == nil || !slices.Equal(res.Registered, []string{"dc1-0"}) {
		t.Fatalf("failed prune = %+v, %v", res, err)
	}

	// An unreachable API keeps the record unchanged.
	api.SetFail("proxy.get", true)
	res, err = Sync(ctx, api, desired, []string{"dc1-0"}, true, "")
	if err == nil || !slices.Equal(res.Registered, []string{"dc1-0"}) {
		t.Fatalf("unreachable = %+v, %v", res, err)
	}
}
