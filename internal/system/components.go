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

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

// ServerPod renders server instance name. A standalone pod has no HA node name; it is
// used once to create or upgrade the schema.
func ServerPod(in Input, name string, standalone bool) *corev1.Pod {
	sys, db := in.System, in.Database
	env := databaseEnv(db)
	if !standalone {
		env = append(env,
			corev1.EnvVar{Name: "ZBX_HANODENAME", ValueFrom: fieldRef("metadata.name")},
			corev1.EnvVar{Name: "ZBX_NODEADDRESS", ValueFrom: fieldRef("status.podIP")})
	}
	if sys.Spec.WebService.IsEnabled() {
		env = append(env, corev1.EnvVar{Name: "ZBX_WEBSERVICEURL",
			Value: fmt.Sprintf("http://%s:%d/report", ServiceName(sys, WebService), WebServicePort)})
	}
	if sys.Spec.Timezone != "" {
		env = append(env, corev1.EnvVar{Name: "TZ", Value: sys.Spec.Timezone})
	}
	vols, mounts := tlsVolumes(db)
	if tls := db.Spec.TLS; tls != nil {
		env = append(env, corev1.EnvVar{Name: "ZBX_DBTLSCONNECT", Value: serverTLSMode(tls.Mode)})
		if tls.CASecretRef != nil {
			env = append(env, corev1.EnvVar{Name: "ZBX_DBTLSCAFILE", Value: caFile})
		}
		if tls.ClientCertSecretRef != nil {
			env = append(env,
				corev1.EnvVar{Name: "ZBX_DBTLSCERTFILE", Value: clientCert},
				corev1.EnvVar{Name: "ZBX_DBTLSKEYFILE", Value: clientKey})
		}
	}

	main := corev1.Container{
		Name:         "zabbix-server",
		Image:        image(sys, "zabbix-server-pgsql", sys.Spec.Server.Image, in.Version),
		Ports:        []corev1.ContainerPort{{Name: portTrapper, ContainerPort: TrapperPort}},
		Env:          env,
		VolumeMounts: mounts,
		// Every healthy HA node is Ready, active or standby: the runtime control socket
		// answers on both. The Service routes by the role label instead.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"zabbix_server", "-R", "ha_status"}}},
			PeriodSeconds:    10,
			TimeoutSeconds:   5,
			FailureThreshold: 3,
		},
	}
	component := Server
	if standalone {
		component = ServerInit
		// The standalone server listens on 10051 only once its schema work is finished.
		main.ReadinessProbe = &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(TrapperPort)}},
			PeriodSeconds: 2,
		}
	}
	// No capabilities are dropped by default: ICMP pingers (fping) need raw sockets.
	return podFrom(in, component, name, main, &sys.Spec.Server.PodSettings, vols, nonRootPod())
}

// serverTLSMode maps a libpq sslmode to the Zabbix server's DBTLSConnect.
func serverTLSMode(mode string) string {
	switch mode {
	case "verify-ca":
		return "verify_ca"
	case "verify-full":
		return "verify_full"
	default:
		return "required"
	}
}

// WebPod renders frontend instance name. ZBX_SERVER_HOST is never set: in HA mode the
// frontend finds the active server through the ha_node table.
func WebPod(in Input, name string) *corev1.Pod {
	sys, db := in.System, in.Database
	env := databaseEnv(db)
	if sys.Spec.Timezone != "" {
		env = append(env, corev1.EnvVar{Name: "PHP_TZ", Value: sys.Spec.Timezone}, corev1.EnvVar{Name: "TZ", Value: sys.Spec.Timezone})
	}
	vols, mounts := tlsVolumes(db)
	if tls := db.Spec.TLS; tls != nil {
		env = append(env, corev1.EnvVar{Name: "ZBX_DB_ENCRYPTION", Value: "true"})
		if tls.CASecretRef != nil {
			env = append(env, corev1.EnvVar{Name: "ZBX_DB_CA_FILE", Value: caFile})
		}
		if tls.ClientCertSecretRef != nil {
			env = append(env,
				corev1.EnvVar{Name: "ZBX_DB_CERT_FILE", Value: clientCert},
				corev1.EnvVar{Name: "ZBX_DB_KEY_FILE", Value: clientKey})
		}
		if tls.Mode == "verify-full" {
			env = append(env, corev1.EnvVar{Name: "ZBX_DB_VERIFY_HOST", Value: "true"})
		}
	}
	probe := func(period, failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromString(portHTTP)}},
			PeriodSeconds:    period,
			FailureThreshold: failures,
			TimeoutSeconds:   5,
		}
	}
	main := corev1.Container{
		Name:            "zabbix-web",
		Image:           image(sys, "zabbix-web-nginx-pgsql", sys.Spec.Web.Image, in.Version),
		Ports:           []corev1.ContainerPort{{Name: portHTTP, ContainerPort: WebPort}},
		Env:             env,
		VolumeMounts:    mounts,
		ReadinessProbe:  probe(10, 3),
		LivenessProbe:   probe(20, 6),
		StartupProbe:    probe(5, 60),
		SecurityContext: restrictedContainer(),
	}
	return podFrom(in, Web, name, main, &sys.Spec.Web.PodSettings, vols, nonRootPod())
}

// WebServicePod renders web service instance name. The web service never connects to the
// database; it renders reports by calling the frontend.
func WebServicePod(in Input, name string) *corev1.Pod {
	sys := in.System
	env := []corev1.EnvVar{{Name: "ZBX_ALLOWEDIP", Value: "0.0.0.0/0,::/0"}}
	if sys.Spec.Timezone != "" {
		env = append(env, corev1.EnvVar{Name: "TZ", Value: sys.Spec.Timezone})
	}
	main := corev1.Container{
		Name:  "zabbix-web-service",
		Image: image(sys, "zabbix-web-service", sys.Spec.WebService.Image, in.Version),
		Ports: []corev1.ContainerPort{{Name: portReport, ContainerPort: WebServicePort}},
		Env:   env,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(WebServicePort)}},
			PeriodSeconds: 10,
		},
		SecurityContext: restrictedContainer(),
	}
	return podFrom(in, WebService, name, main, &sys.Spec.WebService.PodSettings, nil, nonRootPod())
}

func fieldRef(path string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}
}

// IsReady reports whether a pod is Ready.
func IsReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ServerSettings returns the PodSettings of the server, for referenced-object hashing.
func ServerSettings(sys *zabbixv1alpha1.ZabbixSystem) *zabbixv1alpha1.PodSettings {
	return &sys.Spec.Server.PodSettings
}
