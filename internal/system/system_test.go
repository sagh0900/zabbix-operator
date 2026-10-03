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

package system

import (
	"context"
	"net"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

func fixture() Input {
	sys := &zabbixv1alpha1.ZabbixSystem{
		ObjectMeta: metav1.ObjectMeta{Name: "zabbix", Namespace: "ns"},
		Spec: zabbixv1alpha1.ZabbixSystemSpec{Version: "7.0.25", ImageRepository: "zabbix", ImageFlavor: "ubuntu",
			Server: zabbixv1alpha1.ServerSpec{Replicas: 2}},
	}
	db := &zabbixv1alpha1.ZabbixDatabase{Spec: zabbixv1alpha1.ZabbixDatabaseSpec{
		ClusterRef:     zabbixv1alpha1.LocalObjectReference{Name: "zabbix-pg"},
		CredentialsRef: zabbixv1alpha1.CredentialsReference{SecretName: "pg-zabbix-user"},
	}}
	return Input{System: sys, Database: db, Version: "7.0.25"}
}

func env(p *corev1.Pod) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range p.Spec.Containers[0].Env {
		m[e.Name] = e
	}
	return m
}

func TestImages(t *testing.T) {
	in := fixture()
	if got := ServerPod(in, "zabbix-server-0", false).Spec.Containers[0].Image; got != "zabbix/zabbix-server-pgsql:ubuntu-7.0.25" {
		t.Errorf("server image %q", got)
	}
	in.System.Spec.ImageRepository, in.System.Spec.ImageFlavor = "harbor.example.com/zabbix", "alpine"
	if got := WebPod(in, "zabbix-web-0").Spec.Containers[0].Image; got != "harbor.example.com/zabbix/zabbix-web-nginx-pgsql:alpine-7.0.25" {
		t.Errorf("web image %q", got)
	}
	in.System.Spec.WebService.Image = "zabbix/zabbix-web-service@sha256:ac12"
	if got := WebServicePod(in, "zabbix-webservice-0").Spec.Containers[0].Image; got != "zabbix/zabbix-web-service@sha256:ac12" {
		t.Errorf("override image %q", got)
	}
}

// Users cannot override the variables the operator manages, but can add their own.
func TestUserEnvCannotOverrideManagedKeys(t *testing.T) {
	in := fixture()
	in.System.Spec.Server.Env = []corev1.EnvVar{
		{Name: "DB_SERVER_HOST", Value: "elsewhere"},
		{Name: "ZBX_HANODENAME", Value: "fixed"},
		{Name: "ZBX_STARTPOLLERS", Value: "40"},
	}
	e := env(ServerPod(in, "zabbix-server-0", false))
	if e["DB_SERVER_HOST"].Value != "zabbix-pg-rw" {
		t.Errorf("DB_SERVER_HOST overridden: %q", e["DB_SERVER_HOST"].Value)
	}
	if e["ZBX_HANODENAME"].ValueFrom == nil {
		t.Error("ZBX_HANODENAME overridden")
	}
	if e["ZBX_STARTPOLLERS"].Value != "40" {
		t.Error("user tuning variable dropped")
	}
}

func TestStandaloneServerHasNoHAIdentity(t *testing.T) {
	e := env(ServerPod(fixture(), "zabbix-server-init-0", true))
	for _, k := range []string{"ZBX_HANODENAME", "ZBX_NODEADDRESS", "ZBX_AUTOHANODENAME"} {
		if _, ok := e[k]; ok {
			t.Errorf("standalone server sets %s", k)
		}
	}
}

func TestDatabaseTLS(t *testing.T) {
	in := fixture()
	in.Database.Spec.TLS = &zabbixv1alpha1.DatabaseTLS{Mode: "verify-full",
		CASecretRef:         &zabbixv1alpha1.SecretKeyReference{Name: "zabbix-pg-ca", Key: "ca.crt"},
		ClientCertSecretRef: &zabbixv1alpha1.LocalObjectReference{Name: "client"}}
	server := ServerPod(in, "zabbix-server-0", false)
	e := env(server)
	if e["ZBX_DBTLSCONNECT"].Value != "verify_full" || e["ZBX_DBTLSCAFILE"].Value != caFile ||
		e["ZBX_DBTLSCERTFILE"].Value != clientCert || e["ZBX_DBTLSKEYFILE"].Value != clientKey {
		t.Errorf("server TLS env %v", e)
	}
	if len(server.Spec.Volumes) != 2 {
		t.Errorf("server TLS volumes %d", len(server.Spec.Volumes))
	}
	w := env(WebPod(in, "zabbix-web-0"))
	if w["ZBX_DB_ENCRYPTION"].Value != "true" || w["ZBX_DB_CA_FILE"].Value != caFile || w["ZBX_DB_VERIFY_HOST"].Value != "true" {
		t.Errorf("web TLS env %v", w)
	}
	for mode, want := range map[string]string{"require": "required", "verify-ca": "verify_ca", "verify-full": "verify_full"} {
		if got := serverTLSMode(mode); got != want {
			t.Errorf("serverTLSMode(%s) = %s", mode, got)
		}
	}
	if len(WebServicePod(in, "zabbix-webservice-0").Spec.Volumes) != 0 {
		t.Error("the web service never connects to the database and needs no TLS material")
	}
}

func TestWebServiceURLFollowsEnabled(t *testing.T) {
	in := fixture()
	if env(ServerPod(in, "s", false))["ZBX_WEBSERVICEURL"].Value != "http://zabbix-webservice:10053/report" {
		t.Error("web service URL missing")
	}
	in.System.Spec.WebService.Enabled = ptr.To(false)
	if _, ok := env(ServerPod(in, "s", false))["ZBX_WEBSERVICEURL"]; ok {
		t.Error("web service URL set although the web service is disabled")
	}
}

func TestPodSettingsApplied(t *testing.T) {
	in := fixture()
	s := &in.System.Spec.Web.PodSettings
	s.NodeSelector = map[string]string{"pool": "zabbix"}
	s.Tolerations = []corev1.Toleration{{Key: "dedicated", Value: "zabbix", Effect: corev1.TaintEffectNoSchedule}}
	s.Volumes = []zabbixv1alpha1.Volume{zabbixv1alpha1.VolumeOf(corev1.Volume{Name: "saml",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "zabbix-web-saml"}}}})}
	s.VolumeMounts = []corev1.VolumeMount{{Name: "saml", MountPath: "/etc/zabbix/web/zabbix.conf.php", SubPath: "zabbix.conf.php"}}
	s.ExtraContainers = []zabbixv1alpha1.Container{zabbixv1alpha1.ContainerOf(corev1.Container{Name: "sidecar", Image: "busybox"})}
	s.PodLabels = map[string]string{"team": "monitoring", "app.kubernetes.io/name": "hijack"}
	p := WebPod(in, "zabbix-web-0")
	if p.Spec.NodeSelector["pool"] != "zabbix" || len(p.Spec.Tolerations) != 1 {
		t.Error("placement not applied")
	}
	if len(p.Spec.Containers) != 2 || p.Spec.Containers[1].Name != "sidecar" {
		t.Error("extra container not applied")
	}
	if p.Spec.Volumes[0].ConfigMap.Name != "zabbix-web-saml" || p.Spec.Containers[0].VolumeMounts[0].SubPath != "zabbix.conf.php" {
		t.Error("SAML mount not applied")
	}
	if p.Labels["team"] != "monitoring" || p.Labels["app.kubernetes.io/name"] != "zabbix-web" {
		t.Errorf("labels %v", p.Labels)
	}
	if p.Spec.Affinity == nil || p.Spec.Affinity.PodAntiAffinity == nil {
		t.Error("default anti-affinity missing")
	}
	s.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
	if WebPod(in, "zabbix-web-0").Spec.Affinity.PodAntiAffinity != nil {
		t.Error("a user affinity must replace the default")
	}
}

func TestSecurityDefaults(t *testing.T) {
	in := fixture()
	server := ServerPod(in, "s", false)
	if !*server.Spec.SecurityContext.RunAsNonRoot {
		t.Error("server must run non-root")
	}
	if server.Spec.Containers[0].SecurityContext != nil {
		t.Error("server must keep its capabilities by default: ICMP pingers need raw sockets")
	}
	web := WebPod(in, "w").Spec.Containers[0].SecurityContext
	if web == nil || *web.AllowPrivilegeEscalation || len(web.Capabilities.Drop) != 1 {
		t.Errorf("web security %+v", web)
	}
}

func TestReferencedObjects(t *testing.T) {
	in := fixture()
	in.Database.Spec.TLS = &zabbixv1alpha1.DatabaseTLS{Mode: "verify-ca", CASecretRef: &zabbixv1alpha1.SecretKeyReference{Name: "zabbix-pg-ca", Key: "ca.crt"}}
	s := &in.System.Spec.Server.PodSettings
	s.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"}}},
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "extra"}}}}
	s.Volumes = []zabbixv1alpha1.Volume{zabbixv1alpha1.VolumeOf(corev1.Volume{Name: "v", VolumeSource: corev1.VolumeSource{
		Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "proj"}}}}}}})}
	secrets, cms := ReferencedObjects(in.Database, s, true)
	if strings.Join(secrets, ",") != "pg-zabbix-user,proj,tuning,zabbix-pg-ca" || strings.Join(cms, ",") != "extra" {
		t.Errorf("secrets %v, configmaps %v", secrets, cms)
	}
	secrets, _ = ReferencedObjects(in.Database, s, false)
	if strings.Join(secrets, ",") != "proj,tuning" {
		t.Errorf("without database: %v", secrets)
	}
}

func TestApplyService(t *testing.T) {
	in := fixture()
	spec := Services(in.System)[0]
	spec.Settings = zabbixv1alpha1.ServiceSettings{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerIP: "10.11.165.130",
		Annotations: map[string]string{"a": "1", "b": "2"}}
	svc := &corev1.Service{}
	svc.Annotations = map[string]string{"metallb.universe.tf/ip-allocated-from-pool": "first-pool"} // set by MetalLB
	ApplyService(in.System, spec, svc)
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Spec.Ports[0].Port != 10051 || svc.Spec.Ports[0].TargetPort.StrVal != "trapper" {
		t.Errorf("spec %+v", svc.Spec)
	}
	svc.Spec.Ports[0].NodePort = 31234 // allocated by the API server
	spec.Settings.Annotations = map[string]string{"b": "2"}
	ApplyService(in.System, spec, svc)
	if svc.Spec.Ports[0].NodePort != 31234 {
		t.Error("an allocated node port must be kept")
	}
	if _, ok := svc.Annotations["a"]; ok {
		t.Error("an annotation removed from the spec must be removed")
	}
	if svc.Annotations["metallb.universe.tf/ip-allocated-from-pool"] != "first-pool" {
		t.Error("annotations set by other controllers must be kept")
	}
	spec.Settings.Type = corev1.ServiceTypeClusterIP
	ApplyService(in.System, spec, svc)
	if svc.Spec.Ports[0].NodePort != 0 || svc.Spec.ExternalTrafficPolicy != "" {
		t.Error("ClusterIP Services carry no node port or traffic policy")
	}
}

func TestApplyIngress(t *testing.T) {
	in := fixture()
	in.System.Spec.Web.Ingress = zabbixv1alpha1.IngressSettings{Enabled: true,
		Hosts: []zabbixv1alpha1.IngressHost{{Host: "zabbix.example.com", Paths: []string{"/", "/api"}}}}
	specs := Ingresses(in.System)
	if len(specs) != 1 {
		t.Fatalf("%d ingresses", len(specs))
	}
	ing := &networkingv1.Ingress{}
	ApplyIngress(in.System, specs[0], ing)
	paths := ing.Spec.Rules[0].HTTP.Paths
	if len(paths) != 2 || paths[1].Path != "/api" || paths[0].Backend.Service.Port.Number != 80 {
		t.Errorf("rules %+v", ing.Spec.Rules)
	}
	in.System.Spec.Web.Enabled = ptr.To(false)
	if len(Ingresses(in.System)) != 0 {
		t.Error("a disabled frontend has no Ingress")
	}
}

func TestServerProbesAndSelector(t *testing.T) {
	in := fixture()
	ha := ServerPod(in, "zabbix-server-0", false).Spec.Containers[0].ReadinessProbe
	if ha.Exec == nil || strings.Join(ha.Exec.Command, " ") != "zabbix_server -R ha_status" {
		t.Errorf("HA readiness %+v", ha)
	}
	init := ServerPod(in, "zabbix-server-init-0", true).Spec.Containers[0].ReadinessProbe
	if init.TCPSocket == nil || init.TCPSocket.Port.IntValue() != TrapperPort {
		t.Errorf("standalone readiness %+v", init)
	}
	svc := &corev1.Service{}
	ApplyService(in.System, Services(in.System)[0], svc)
	if svc.Spec.Selector[LabelRole] != RoleActive {
		t.Errorf("server Service selector %v", svc.Spec.Selector)
	}
	web := &corev1.Service{}
	ApplyService(in.System, Services(in.System)[1], web)
	if _, ok := web.Spec.Selector[LabelRole]; ok {
		t.Error("only the server Service selects by role")
	}
}

func TestTCPActiveProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:10051")
	if err != nil {
		t.Skipf("port 10051 unavailable: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	active := &corev1.Pod{Status: corev1.PodStatus{PodIP: "127.0.0.1"}}
	if !TCPActiveProbe(context.Background(), active) {
		t.Error("a listening node must be active")
	}
	standby := &corev1.Pod{Status: corev1.PodStatus{PodIP: "127.0.0.2"}}
	if TCPActiveProbe(context.Background(), standby) {
		t.Error("a node that refuses connections must be standby")
	}
	if TCPActiveProbe(context.Background(), &corev1.Pod{}) {
		t.Error("a pod without an IP is not active")
	}
}

func TestIsReady(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if !IsReady(p) || IsReady(&corev1.Pod{}) {
		t.Error("IsReady")
	}
}
