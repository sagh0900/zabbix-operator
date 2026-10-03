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

package jobs

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

func testSpec() Spec {
	owner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "zabbix", Namespace: "ns", UID: "uid"},
	}
	db := &zabbixv1alpha1.ZabbixDatabase{Spec: zabbixv1alpha1.ZabbixDatabaseSpec{
		ClusterRef:     zabbixv1alpha1.LocalObjectReference{Name: "pg"},
		CredentialsRef: zabbixv1alpha1.CredentialsReference{SecretName: "creds", UsernameKey: "user", PasswordKey: "pass"},
	}}
	return Spec{Owner: owner, System: "zabbix", Command: CommandPrecheck, Args: []string{"--target-version=8.0.0rc1"},
		Image: "ghcr.io/sagh0900/zabbix-operator:v0.1.0", Database: db, Host: "pg-rw"}
}

func scheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func envOf(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
	}
	return m
}

func TestNameIsDeterministicAndBounded(t *testing.T) {
	s := testSpec()
	if again := testSpec(); Name(s) != Name(again) {
		t.Fatal("same inputs gave different names")
	}
	other := s
	other.Args = []string{"--target-version=7.0.25"}
	if Name(s) == Name(other) {
		t.Fatal("different arguments gave the same name")
	}
	if !strings.HasPrefix(Name(s), "zabbix-precheck-") {
		t.Errorf("name %q", Name(s))
	}
	s.System = strings.Repeat("a", 80)
	if n := Name(s); len(n) > 63 {
		t.Errorf("name %q is %d characters", n, len(n))
	}
}

func TestBuildWithoutTLS(t *testing.T) {
	job, err := Build(testSpec(), scheme(t))
	if err != nil {
		t.Fatal(err)
	}
	if ref := metav1.GetControllerOf(job); ref == nil || ref.UID != "uid" {
		t.Errorf("controller reference %+v", ref)
	}
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]
	if got := strings.Join(append(c.Command, c.Args...), " "); got != "/manager job precheck --target-version=8.0.0rc1" {
		t.Errorf("command %q", got)
	}
	env := envOf(c)
	if env[EnvHost] != "pg-rw" || env[EnvPort] != "5432" || env[EnvName] != "zabbix" || env[EnvSSLMode] != "prefer" {
		t.Errorf("env %v", env)
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil || strings.Contains(strings.ToLower(e.Name), "pass") {
			t.Errorf("credentials must come from files, found env %s", e.Name)
		}
	}
	creds := pod.Volumes[0].Secret
	if creds.SecretName != "creds" || creds.Items[0].Key != "user" || creds.Items[1].Key != "pass" {
		t.Errorf("credentials volume %+v", creds)
	}
	if !*pod.SecurityContext.RunAsNonRoot || *c.SecurityContext.AllowPrivilegeEscalation || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("job must run non-root, read-only, without privilege escalation")
	}
	if *pod.AutomountServiceAccountToken {
		t.Error("job needs no API token")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever || *job.Spec.ActiveDeadlineSeconds == 0 {
		t.Error("job must not restart in place and must have a deadline")
	}
}

func TestBuildWithTLS(t *testing.T) {
	s := testSpec()
	s.Database.Spec.TLS = &zabbixv1alpha1.DatabaseTLS{
		Mode:                "verify-full",
		CASecretRef:         &zabbixv1alpha1.SecretKeyReference{Name: "pg-ca", Key: "ca.crt"},
		ClientCertSecretRef: &zabbixv1alpha1.LocalObjectReference{Name: "client"},
	}
	job, err := Build(s, scheme(t))
	if err != nil {
		t.Fatal(err)
	}
	env := envOf(job.Spec.Template.Spec.Containers[0])
	if env[EnvSSLMode] != "verify-full" || env[EnvSSLRootCert] != TLSDir+"/ca/ca.crt" ||
		env[EnvSSLCert] != TLSDir+"/client/tls.crt" || env[EnvSSLKey] != TLSDir+"/client/tls.key" {
		t.Errorf("TLS env %v", env)
	}
	if n := len(job.Spec.Template.Spec.Volumes); n != 3 {
		t.Errorf("%d volumes, want credentials, CA and client certificate", n)
	}
}
