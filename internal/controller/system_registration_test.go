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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/registration/fakeapi"
	"github.com/sagh0900/zabbix-operator/internal/zabbixapi"
)

// waitRegistration waits for the ProxiesRegistered condition with status and reason.
func waitRegistration(t *testing.T, ns string, status metav1.ConditionStatus, reason string) *zabbixv1alpha1.ZabbixSystem {
	t.Helper()
	var sys *zabbixv1alpha1.ZabbixSystem
	eventually(t, func() error {
		sys = getSystem(t, ns)
		c := meta.FindStatusCondition(sys.Status.Conditions, zabbixv1alpha1.SystemProxiesRegistered)
		if c == nil || c.Status != status || c.Reason != reason {
			return fmt.Errorf("ProxiesRegistered = %+v, want %s %s", c, status, reason)
		}
		return nil
	})
	return sys
}

func setProxyList(t *testing.T, ns, list string) {
	t.Helper()
	ctx := context.Background()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zabbix-proxies"}, Data: map[string]string{"proxies.yaml": list}}
	if err := k8s.Create(ctx, cm); err == nil {
		return
	}
	if err := k8s.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
}

// Registration keeps in-cluster and listed proxies registered, adopts nothing it was not
// asked to, prunes only what it registered, and forgets its record when disabled.
func TestSystem_ProxyRegistration(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	api := fakeapi.New()
	api.Add(zabbixapi.Proxy{Name: "manual", OperatingMode: zabbixapi.ModeActive})
	fakeAPIs.Store("http://zabbix-web."+ns+".svc:80/api_jsonrpc.php", api)
	t.Cleanup(func() { fakeAPIs.Delete("http://zabbix-web." + ns + ".svc:80/api_jsonrpc.php") })

	if err := k8s.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zabbix-api"},
		StringData: map[string]string{"token": testAPIToken + "\n"}}); err != nil {
		t.Fatal(err)
	}
	setProxyList(t, ns, "- name: remote\n")
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "dc1", Mode: zabbixv1alpha1.ProxyActive, Replicas: 2, ProxyGroup: "dc1"}}
		s.Spec.ProxyRegistration = zabbixv1alpha1.ProxyRegistrationSpec{Enabled: true, Prune: true,
			APITokenSecretRef: &zabbixv1alpha1.SecretKeyReference{Name: "zabbix-api", Key: "token"},
			ConfigMapRef:      &zabbixv1alpha1.ConfigMapKeyReference{Name: "zabbix-proxies", Key: "proxies.yaml"}}
	})

	sys := waitRegistration(t, ns, metav1.ConditionTrue, "Synced")
	if got := sys.Status.RegisteredProxies; !slices.Equal(got, []string{"dc1-0", "dc1-1", "remote"}) {
		t.Errorf("registeredProxies = %v", got)
	}
	if p, _ := api.Get("dc1-1"); p.ProxyGroupID != api.Group("dc1") || p.LocalAddress != "dc1-1.dc1."+ns+".svc" {
		t.Errorf("dc1-1 = %+v", p)
	}
	waitEvent(t, ns, corev1.EventTypeNormal, "ProxiesRegistered", "created 3")

	// An invalid list is reported and changes nothing.
	setProxyList(t, ns, "- mode: active\n")
	waitRegistration(t, ns, metav1.ConditionFalse, "InvalidConfiguration")

	// A failing API is reported; what was registered stays on record.
	api.SetFail("proxy.create", true)
	setProxyList(t, ns, "- name: remote\n- name: remote2\n")
	sys = waitRegistration(t, ns, metav1.ConditionFalse, "SyncFailed")
	if got := sys.Status.RegisteredProxies; !slices.Equal(got, []string{"dc1-0", "dc1-1", "remote"}) {
		t.Errorf("registeredProxies after a failed create = %v", got)
	}
	waitEvent(t, ns, corev1.EventTypeWarning, "ProxyRegistrationFailed", "remote2")

	// Recovery, and pruning of a proxy removed from the list; the hand-made one stays.
	api.SetFail("proxy.create", false)
	setProxyList(t, ns, "- name: remote2\n")
	sys = waitRegistration(t, ns, metav1.ConditionTrue, "Synced")
	if got := sys.Status.RegisteredProxies; !slices.Equal(got, []string{"dc1-0", "dc1-1", "remote2"}) {
		t.Errorf("registeredProxies = %v", got)
	}
	if got := api.Names(); !slices.Equal(got, []string{"dc1-0", "dc1-1", "manual", "remote2"}) {
		t.Errorf("Zabbix proxies = %v", got)
	}

	// Disabling forgets the record and leaves Zabbix as it is.
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.ProxyRegistration.Enabled = false })
	eventually(t, func() error {
		sys := getSystem(t, ns)
		if meta.FindStatusCondition(sys.Status.Conditions, zabbixv1alpha1.SystemProxiesRegistered) != nil || sys.Status.RegisteredProxies != nil {
			return fmt.Errorf("registration still reported: %v", sys.Status.RegisteredProxies)
		}
		return nil
	})
	if got := api.Names(); len(got) != 4 {
		t.Errorf("disabling changed Zabbix: %v", got)
	}
}

// A wrong token or an unreachable API is reported without affecting the system's phase.
func TestSystem_ProxyRegistrationUnreachable(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	if err := k8s.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "zabbix-api"},
		StringData: map[string]string{"token": "wrong"}}); err != nil {
		t.Fatal(err)
	}
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "dc1"}}
		s.Spec.ProxyRegistration = zabbixv1alpha1.ProxyRegistrationSpec{Enabled: true,
			APITokenSecretRef: &zabbixv1alpha1.SecretKeyReference{Name: "zabbix-api", Key: "token"}}
	})
	sys := waitRegistration(t, ns, metav1.ConditionFalse, "SyncFailed")
	if sys.Status.Phase != zabbixv1alpha1.PhaseRunning {
		t.Errorf("phase = %s, registration must not degrade the system", sys.Status.Phase)
	}
}
