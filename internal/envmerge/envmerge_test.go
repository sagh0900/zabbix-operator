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

package envmerge

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func env(name, value string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, Value: value}
}

func envByName(vars []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, v := range vars {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.EnvVar{}, false
}

// TestProtectedEnvMerge verifies that the operator's protected env keys
// always win over user-supplied overrides.
func TestProtectedEnvMerge(t *testing.T) {
	protected := []string{"ZBX_DBHOST", "ZBX_DBPASSWORD"}

	operatorEnv := []corev1.EnvVar{
		env("ZBX_DBHOST", "operator-host"),
		env("ZBX_DBPASSWORD", "operator-pass"),
		env("ZBX_TIMEOUT", "5"),
	}

	userEnv := []corev1.EnvVar{
		env("ZBX_DBHOST", "user-host"),        // should be blocked — protected
		env("ZBX_DBPASSWORD", "user-pass"),    // should be blocked — protected
		env("ZBX_TIMEOUT", "30"),              // should win — not protected
		env("ZBX_LOGFILE", "/tmp/zabbix.log"), // new key — should be appended
	}

	result := Merge(operatorEnv, userEnv, protected)

	// Protected keys must retain the operator value.
	if v, ok := envByName(result, "ZBX_DBHOST"); !ok || v.Value != "operator-host" {
		t.Errorf("ZBX_DBHOST: expected operator-host, got %q (ok=%v)", v.Value, ok)
	}
	if v, ok := envByName(result, "ZBX_DBPASSWORD"); !ok || v.Value != "operator-pass" {
		t.Errorf("ZBX_DBPASSWORD: expected operator-pass, got %q (ok=%v)", v.Value, ok)
	}

	// Non-protected key overridden by user.
	if v, ok := envByName(result, "ZBX_TIMEOUT"); !ok || v.Value != "30" {
		t.Errorf("ZBX_TIMEOUT: expected 30, got %q (ok=%v)", v.Value, ok)
	}

	// New key from user appended.
	if v, ok := envByName(result, "ZBX_LOGFILE"); !ok || v.Value != "/tmp/zabbix.log" {
		t.Errorf("ZBX_LOGFILE: expected /tmp/zabbix.log, got %q (ok=%v)", v.Value, ok)
	}
}

// TestMergeNoUserEnv verifies that an empty userEnv returns all operator defaults.
func TestMergeNoUserEnv(t *testing.T) {
	operatorEnv := []corev1.EnvVar{
		env("ZBX_DBHOST", "db.svc"),
		env("ZBX_TIMEOUT", "5"),
	}
	result := Merge(operatorEnv, nil, []string{"ZBX_DBHOST"})

	if len(result) != 2 {
		t.Errorf("expected 2 vars, got %d", len(result))
	}
	if v, _ := envByName(result, "ZBX_DBHOST"); v.Value != "db.svc" {
		t.Errorf("ZBX_DBHOST: expected db.svc, got %q", v.Value)
	}
}

// TestMergeNoDuplicates verifies that no key appears more than once in the result.
func TestMergeNoDuplicates(t *testing.T) {
	operatorEnv := []corev1.EnvVar{env("FOO", "op")}
	userEnv := []corev1.EnvVar{env("FOO", "user")}
	result := Merge(operatorEnv, userEnv, nil)

	seen := map[string]int{}
	for _, v := range result {
		seen[v.Name]++
	}
	for k, c := range seen {
		if c > 1 {
			t.Errorf("key %q appears %d times in result (want exactly 1)", k, c)
		}
	}
}

// TestMergeOrderPreserved verifies that the result slice is in a deterministic order.
func TestMergeOrderPreserved(t *testing.T) {
	operatorEnv := []corev1.EnvVar{
		env("A", "1"),
		env("B", "2"),
		env("C", "3"),
	}
	userEnv := []corev1.EnvVar{
		env("D", "4"),
		env("E", "5"),
	}
	result := Merge(operatorEnv, userEnv, nil)

	// First three should be A,B,C (operator order), then D,E (user order).
	want := []string{"A", "B", "C", "D", "E"}
	if len(result) != len(want) {
		t.Fatalf("expected %d vars, got %d", len(want), len(result))
	}
	for i, w := range want {
		if result[i].Name != w {
			t.Errorf("position %d: expected %q, got %q", i, w, result[i].Name)
		}
	}
}

// TestMergeAllKeysProtected verifies that when every key is protected, the
// user cannot override any of them.
func TestMergeAllKeysProtected(t *testing.T) {
	operatorEnv := []corev1.EnvVar{
		env("ZBX_DBHOST", "op-host"),
		env("ZBX_DBNAME", "zabbix"),
	}
	userEnv := []corev1.EnvVar{
		env("ZBX_DBHOST", "user-host"),
		env("ZBX_DBNAME", "mydb"),
	}
	protected := []string{"ZBX_DBHOST", "ZBX_DBNAME"}
	result := Merge(operatorEnv, userEnv, protected)

	if v, _ := envByName(result, "ZBX_DBHOST"); v.Value != "op-host" {
		t.Errorf("ZBX_DBHOST: expected op-host, got %q", v.Value)
	}
	if v, _ := envByName(result, "ZBX_DBNAME"); v.Value != "zabbix" {
		t.Errorf("ZBX_DBNAME: expected zabbix, got %q", v.Value)
	}
}
