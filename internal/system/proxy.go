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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

// ProxyComponentPrefix prefixes the component label of every proxy: proxy-<name>.
const ProxyComponentPrefix = "proxy-"

// zabbixGID is the group of the official images' zabbix user.
const zabbixGID = 1995

// ProxyComponent returns the component label of a proxy.
func ProxyComponent(p *zabbixv1alpha1.ProxySpec) string { return ProxyComponentPrefix + p.Name }

// ProxyPodName returns the pod name, and Zabbix host name, of instance i of a proxy.
func ProxyPodName(p *zabbixv1alpha1.ProxySpec, i int) string { return fmt.Sprintf("%s-%d", p.Name, i) }

// ProxyAddress is the DNS name the server uses to reach a passive proxy instance: the pod
// name under the proxy's headless Service.
func ProxyAddress(sys *zabbixv1alpha1.ZabbixSystem, p *zabbixv1alpha1.ProxySpec, i int) string {
	return fmt.Sprintf("%s.%s.%s.svc", ProxyPodName(p, i), p.Name, sys.Namespace)
}

// ProxyPod renders instance name of proxy p. Proxies keep their SQLite database in an
// emptyDir, so they are stateless; data buffered by an instance is lost when its pod is
// replaced.
func ProxyPod(in Input, p *zabbixv1alpha1.ProxySpec, name string) *corev1.Pod {
	sys := in.System
	mode, server := "0", serverAddress(sys)
	if p.Mode == zabbixv1alpha1.ProxyPassive {
		// The server connects from its pod IP, which changes; access is limited by the
		// proxy's network exposure instead.
		mode, server = "1", anyAddress
	}
	env := []corev1.EnvVar{
		{Name: envHostname, Value: name},
		{Name: "ZBX_PROXYMODE", Value: mode},
		{Name: envServerHost, Value: server},
	}
	if sys.Spec.Timezone != "" {
		env = append(env, corev1.EnvVar{Name: "TZ", Value: sys.Spec.Timezone})
	}
	main := corev1.Container{
		Name:         "zabbix-proxy",
		Image:        image(sys, "zabbix-proxy-sqlite3", p.Image, in.Version),
		Ports:        []corev1.ContainerPort{{Name: portTrapper, ContainerPort: TrapperPort}},
		Env:          env,
		VolumeMounts: []corev1.VolumeMount{{Name: "db-data", MountPath: "/var/lib/zabbix/db_data"}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(TrapperPort)}},
			PeriodSeconds: 10,
		},
	}
	sec := nonRootPod()
	sec.FSGroup = ptr.To[int64](zabbixGID)
	vols := []corev1.Volume{{Name: "db-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	// No capabilities are dropped by default: ICMP pingers (fping) need raw sockets.
	pod := podFrom(in, ProxyComponent(p), name, main, &p.PodSettings, vols, sec)
	// Per-instance DNS: <pod>.<proxy>.<namespace>.svc through the headless Service.
	pod.Spec.Subdomain = p.Name
	return pod
}

// serverAddress is how components reach the active server: the role-selected Service.
func serverAddress(sys *zabbixv1alpha1.ZabbixSystem) string {
	port := sys.Spec.Server.Service.Port
	if port == 0 {
		port = TrapperPort
	}
	return fmt.Sprintf("%s:%d", ServiceName(sys, Server), port)
}
