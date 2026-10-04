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

	appsv1 "k8s.io/api/apps/v1"
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

// The frontend reaches the active server through the role-selected server Service.
func TestWebUsesServerService(t *testing.T) {
	in := fixture()
	in.System.Name = "monitoring"
	in.System.Spec.Web.Env = []corev1.EnvVar{{Name: "ZBX_SERVER_HOST", Value: "elsewhere"}}
	e := env(WebPod(in, "monitoring-web-0"))
	if e["ZBX_SERVER_HOST"].Value != "monitoring-server" || e["ZBX_SERVER_PORT"].Value != "10051" {
		t.Errorf("server host %q port %q", e["ZBX_SERVER_HOST"].Value, e["ZBX_SERVER_PORT"].Value)
	}
	in.System.Spec.Server.Service.Port = 10061
	if p := env(WebPod(in, "monitoring-web-0"))["ZBX_SERVER_PORT"].Value; p != "10061" {
		t.Errorf("server port %q", p)
	}
}

func TestProxyPods(t *testing.T) {
	in := fixture()
	in.System.Spec.Timezone = "Europe/Stockholm"
	active := &zabbixv1alpha1.ProxySpec{Name: "dc1", Mode: zabbixv1alpha1.ProxyActive}
	p := ProxyPod(in, active, ProxyPodName(active, 1))
	e := env(p)
	if p.Name != "dc1-1" || e["ZBX_HOSTNAME"].Value != "dc1-1" || e["ZBX_PROXYMODE"].Value != "0" ||
		e["ZBX_SERVER_HOST"].Value != "zabbix-server:10051" || e["TZ"].Value != "Europe/Stockholm" {
		t.Errorf("active proxy %s env %v", p.Name, e)
	}
	if p.Spec.Containers[0].Image != "zabbix/zabbix-proxy-sqlite3:ubuntu-7.0.25" {
		t.Errorf("image %q", p.Spec.Containers[0].Image)
	}
	if p.Spec.Subdomain != "dc1" || p.Labels["app.kubernetes.io/name"] != "zabbix-proxy-dc1" {
		t.Errorf("subdomain %q labels %v", p.Spec.Subdomain, p.Labels)
	}
	if p.Spec.Volumes[0].EmptyDir == nil || p.Spec.Containers[0].VolumeMounts[0].MountPath != "/var/lib/zabbix/db_data" ||
		*p.Spec.SecurityContext.FSGroup != 1995 {
		t.Error("the SQLite database must live in a writable emptyDir")
	}
	passive := &zabbixv1alpha1.ProxySpec{Name: "edge", Mode: zabbixv1alpha1.ProxyPassive}
	e = env(ProxyPod(in, passive, ProxyPodName(passive, 0)))
	if e["ZBX_PROXYMODE"].Value != "1" || e["ZBX_SERVER_HOST"].Value != "0.0.0.0/0,::/0" {
		t.Errorf("passive proxy env %v", e)
	}
	if got := ProxyAddress(in.System, passive, 0); got != "edge-0.edge.ns.svc" {
		t.Errorf("address %q", got)
	}
	passive.Env = []corev1.EnvVar{{Name: "ZBX_HOSTNAME", Value: "x"}, {Name: "ZBX_PROXYOFFLINEBUFFER", Value: "24"}}
	e = env(ProxyPod(in, passive, "edge-0"))
	if e["ZBX_HOSTNAME"].Value != "edge-0" || e["ZBX_PROXYOFFLINEBUFFER"].Value != "24" {
		t.Error("proxy host name must be protected; tuning must pass through")
	}
}

func TestProxyServices(t *testing.T) {
	in := fixture()
	in.System.Spec.Proxies = []zabbixv1alpha1.ProxySpec{
		{Name: "dc1"},
		{Name: "edge", Service: zabbixv1alpha1.ServiceSettings{Type: corev1.ServiceTypeLoadBalancer}},
		{Name: "off", Enabled: ptr.To(false)},
	}
	services := Services(in.System)
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, ServiceObjectName(in.System, s))
		if s.Name == "dc1" {
			svc := &corev1.Service{}
			ApplyService(in.System, s, svc)
			if svc.Spec.ClusterIP != corev1.ClusterIPNone || !svc.Spec.PublishNotReadyAddresses || svc.Spec.Selector["zabbix.io/component"] != "proxy-dc1" {
				t.Errorf("headless proxy Service %+v", svc.Spec)
			}
		}
	}
	if got := strings.Join(names, ","); got != "zabbix-server,zabbix-web,zabbix-webservice,dc1,edge,edge-external" {
		t.Errorf("services %s", got)
	}
}

func TestAgentDaemonSet(t *testing.T) {
	in := fixture()
	in.System.Spec.Agent = zabbixv1alpha1.AgentSpec{Enabled: true, PodSettings: zabbixv1alpha1.PodSettings{
		Image:       "zabbix/zabbix-agent2:ubuntu-7.0.25",
		Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
		Env:         []corev1.EnvVar{{Name: "ZBX_SERVER_HOST", Value: "x"}, {Name: "ZBX_TIMEOUT", Value: "10"}},
	}}
	ds := &appsv1.DaemonSet{}
	ApplyAgent(in.System, ds, "h1")
	spec := ds.Spec.Template.Spec
	if !spec.HostNetwork || !spec.HostPID || spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Error("the agent must run in the node's network and process namespaces and resolve cluster DNS")
	}
	if len(spec.Tolerations) != 1 || spec.Containers[0].Image != "zabbix/zabbix-agent2:ubuntu-7.0.25" {
		t.Error("agent settings not applied")
	}
	e := map[string]corev1.EnvVar{}
	for _, v := range spec.Containers[0].Env {
		e[v.Name] = v
	}
	if e["ZBX_HOSTNAME"].ValueFrom == nil || e["ZBX_HOSTNAME"].ValueFrom.FieldRef.FieldPath != "spec.nodeName" ||
		e["ZBX_SERVER_HOST"].Value != "zabbix-server" || e["ZBX_TIMEOUT"].Value != "10" {
		t.Errorf("agent env %v", e)
	}
	if ds.Spec.Selector.MatchLabels["zabbix.io/component"] != Agent || ds.Spec.Template.Annotations[AnnotationConfigHash] != "h1" {
		t.Errorf("selector %v annotations %v", ds.Spec.Selector, ds.Spec.Template.Annotations)
	}
}
