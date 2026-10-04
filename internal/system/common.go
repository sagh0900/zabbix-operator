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

// Package system renders the Kubernetes objects of a ZabbixSystem: one pure builder per
// component turns the spec into pods, Services and Ingresses. Nothing here talks to the
// API server.
package system

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/envmerge"
	"github.com/sagh0900/zabbix-operator/internal/podset"
)

// Component names, used in labels, pod names and Service names.
const (
	Server      = "server"
	ServerInit  = "server-init"
	Web         = "web"
	WebService  = "webservice"
	ProxyPrefix = "proxy"
)

// Port names of the Zabbix containers, referenced by the Services.
const (
	portTrapper = "trapper"
	portHTTP    = "http"
	portReport  = "report"
	defaultRepo = "zabbix"
)

// Ports of the Zabbix components.
const (
	TrapperPort    = 10051
	WebPort        = 8080
	WebServicePort = 10053
)

// Environment variables and values shared by several Zabbix images.
const (
	envHostname   = "ZBX_HOSTNAME"
	envServerHost = "ZBX_SERVER_HOST"
	envServerPort = "ZBX_SERVER_PORT"
	envTrue       = "true"
	// anyAddress allows connections from any address; used where the peer is a server Pod
	// whose IP changes.
	anyAddress = "0.0.0.0/0,::/0"
)

// Paths where database TLS material is mounted in Zabbix containers.
const (
	tlsDir     = "/etc/zabbix-db-tls"
	caFile     = tlsDir + "/ca/ca.crt"
	clientCert = tlsDir + "/client/tls.crt"
	clientKey  = tlsDir + "/client/tls.key"
)

// AnnotationConfigHash records the versions of the Secrets and ConfigMaps a pod uses, so
// changing them rolls the pod.
const AnnotationConfigHash = "zabbix.io/config-hash"

// Input is everything a builder needs.
type Input struct {
	System   *zabbixv1alpha1.ZabbixSystem
	Database *zabbixv1alpha1.ZabbixDatabase
	// Version is the Zabbix version the component runs; during an upgrade it differs
	// from the spec for components that have not moved yet.
	Version string
	// ConfigHash summarises the referenced Secrets and ConfigMaps of the component.
	ConfigHash string
}

// image returns the official image of a component for version, unless overridden.
func image(sys *zabbixv1alpha1.ZabbixSystem, repo, override, version string) string {
	if override != "" {
		return override
	}
	prefix := sys.Spec.ImageRepository
	if prefix == "" {
		prefix = defaultRepo
	}
	flavor := sys.Spec.ImageFlavor
	if flavor == "" {
		flavor = "ubuntu"
	}
	return fmt.Sprintf("%s/%s:%s-%s", prefix, repo, flavor, version)
}

// PodName returns the stable name of instance i of a component.
func PodName(sys *zabbixv1alpha1.ZabbixSystem, component string, i int) string {
	return fmt.Sprintf("%s-%s-%d", sys.Name, component, i)
}

// ServiceName returns the Service name of a component.
func ServiceName(sys *zabbixv1alpha1.ZabbixSystem, component string) string {
	return sys.Name + "-" + component
}

// labels are the labels every object of a component carries in addition to the podset
// selector labels.
func labels(sys *zabbixv1alpha1.ZabbixSystem, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     "zabbix-" + component,
		"app.kubernetes.io/instance": sys.Name,
		"app.kubernetes.io/part-of":  "zabbix",
	}
}

// operatorManagedKeys lists environment variables the operator owns; user values for them
// are ignored.
var operatorManagedKeys = []string{
	"DB_SERVER_HOST", "DB_SERVER_PORT", "POSTGRES_DB", "POSTGRES_USER", "POSTGRES_PASSWORD",
	"ZBX_HANODENAME", "ZBX_AUTOHANODENAME", "ZBX_NODEADDRESS",
	"ZBX_DBTLSCONNECT", "ZBX_DBTLSCAFILE", "ZBX_DBTLSCERTFILE", "ZBX_DBTLSKEYFILE",
	"ZBX_DB_ENCRYPTION", "ZBX_DB_CA_FILE", "ZBX_DB_CERT_FILE", "ZBX_DB_KEY_FILE", "ZBX_DB_VERIFY_HOST",
	envServerHost, envServerPort, envHostname, "ZBX_PROXYMODE",
}

// databaseEnv returns the connection variables common to server and frontend.
func databaseEnv(db *zabbixv1alpha1.ZabbixDatabase) []corev1.EnvVar {
	userKey, passKey := db.Spec.CredentialsRef.UsernameKey, db.Spec.CredentialsRef.PasswordKey
	if userKey == "" {
		userKey = "username"
	}
	if passKey == "" {
		passKey = "password"
	}
	port := db.Spec.Port
	if port == 0 {
		port = 5432
	}
	name := db.Spec.Database
	if name == "" {
		name = "zabbix"
	}
	secretRef := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: db.Spec.CredentialsRef.SecretName}, Key: key}}
	}
	return []corev1.EnvVar{
		{Name: "DB_SERVER_HOST", Value: db.HostOrDefault()},
		{Name: "DB_SERVER_PORT", Value: fmt.Sprint(port)},
		{Name: "POSTGRES_DB", Value: name},
		{Name: "POSTGRES_USER", ValueFrom: secretRef(userKey)},
		{Name: "POSTGRES_PASSWORD", ValueFrom: secretRef(passKey)},
	}
}

// tlsVolumes mounts the database CA and client certificate, if configured.
func tlsVolumes(db *zabbixv1alpha1.ZabbixDatabase) ([]corev1.Volume, []corev1.VolumeMount) {
	tls := db.Spec.TLS
	if tls == nil {
		return nil, nil
	}
	var vols []corev1.Volume
	var mounts []corev1.VolumeMount
	if tls.CASecretRef != nil {
		vols = append(vols, corev1.Volume{Name: "db-tls-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: tls.CASecretRef.Name,
			Items:      []corev1.KeyToPath{{Key: tls.CASecretRef.Key, Path: "ca.crt"}},
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "db-tls-ca", MountPath: tlsDir + "/ca", ReadOnly: true})
	}
	if tls.ClientCertSecretRef != nil {
		vols = append(vols, corev1.Volume{Name: "db-tls-client", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: tls.ClientCertSecretRef.Name, DefaultMode: ptr.To[int32](0o440),
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "db-tls-client", MountPath: tlsDir + "/client", ReadOnly: true})
	}
	return vols, mounts
}

// podFrom assembles a pod from a main container and the component's user settings.
func podFrom(in Input, component, name string, main corev1.Container, s *zabbixv1alpha1.PodSettings,
	extraVolumes []corev1.Volume, podSec *corev1.PodSecurityContext) *corev1.Pod {
	sys := in.System
	main.Env = envmerge.Merge(main.Env, s.Env, operatorManagedKeys)
	main.EnvFrom = append(main.EnvFrom, s.EnvFrom...)
	main.VolumeMounts = append(main.VolumeMounts, s.VolumeMounts...)
	main.Resources = s.Resources
	if s.SecurityContext != nil {
		main.SecurityContext = s.SecurityContext
	}

	containers := make([]corev1.Container, 0, 1+len(s.ExtraContainers))
	containers = append(containers, main)
	for _, c := range s.ExtraContainers {
		containers = append(containers, c.Get())
	}
	volumes := append([]corev1.Volume{}, extraVolumes...)
	for _, v := range s.Volumes {
		volumes = append(volumes, v.Get())
	}
	spread := make([]corev1.TopologySpreadConstraint, 0, len(s.TopologySpreadConstraints))
	for _, t := range s.TopologySpreadConstraints {
		spread = append(spread, t.Get())
	}
	if s.PodSecurityContext != nil {
		podSec = s.PodSecurityContext
	}
	affinity := s.Affinity
	if affinity == nil {
		affinity = spreadAcrossNodes(sys.Name, component)
	}

	podLabels := map[string]string{}
	for k, v := range s.PodLabels {
		podLabels[k] = v
	}
	for k, v := range labels(sys, component) {
		podLabels[k] = v
	}
	annotations := map[string]string{}
	for k, v := range s.PodAnnotations {
		annotations[k] = v
	}
	if in.ConfigHash != "" {
		annotations[AnnotationConfigHash] = in.ConfigHash
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: podLabels, Annotations: annotations},
		Spec: corev1.PodSpec{
			Hostname:                      name,
			Containers:                    containers,
			Volumes:                       volumes,
			NodeSelector:                  s.NodeSelector,
			Tolerations:                   s.Tolerations,
			Affinity:                      affinity,
			TopologySpreadConstraints:     spread,
			PriorityClassName:             s.PriorityClassName,
			ServiceAccountName:            s.ServiceAccountName,
			ImagePullSecrets:              s.ImagePullSecrets,
			SecurityContext:               podSec,
			TerminationGracePeriodSeconds: ptr.To[int64](60),
			EnableServiceLinks:            ptr.To(false),
		},
	}
}

// spreadAcrossNodes prefers placing pods of one component on different nodes.
func spreadAcrossNodes(system, component string) *corev1.Affinity {
	return &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
			Weight: 100,
			PodAffinityTerm: corev1.PodAffinityTerm{
				TopologyKey:   "kubernetes.io/hostname",
				LabelSelector: &metav1.LabelSelector{MatchLabels: podset.SelectorLabels(system, component)},
			},
		}},
	}}
}

// restrictedContainer hardens a container that needs no privileges.
func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// nonRootPod runs the official images' zabbix user with the default seccomp profile.
func nonRootPod() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// ReferencedObjects lists the Secrets and ConfigMaps a component's pods read, sorted, so
// their versions can be hashed into AnnotationConfigHash.
func ReferencedObjects(db *zabbixv1alpha1.ZabbixDatabase, s *zabbixv1alpha1.PodSettings, usesDatabase bool) (secrets, configMaps []string) {
	sec, cm := map[string]bool{}, map[string]bool{}
	if usesDatabase {
		sec[db.Spec.CredentialsRef.SecretName] = true
		if tls := db.Spec.TLS; tls != nil {
			if tls.CASecretRef != nil {
				sec[tls.CASecretRef.Name] = true
			}
			if tls.ClientCertSecretRef != nil {
				sec[tls.ClientCertSecretRef.Name] = true
			}
		}
	}
	for _, e := range s.EnvFrom {
		if e.SecretRef != nil {
			sec[e.SecretRef.Name] = true
		}
		if e.ConfigMapRef != nil {
			cm[e.ConfigMapRef.Name] = true
		}
	}
	for _, e := range s.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			sec[e.ValueFrom.SecretKeyRef.Name] = true
		}
		if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil {
			cm[e.ValueFrom.ConfigMapKeyRef.Name] = true
		}
	}
	for _, w := range s.Volumes {
		v := w.Get()
		if v.Secret != nil {
			sec[v.Secret.SecretName] = true
		}
		if v.ConfigMap != nil {
			cm[v.ConfigMap.Name] = true
		}
		if v.Projected != nil {
			for _, p := range v.Projected.Sources {
				if p.Secret != nil {
					sec[p.Secret.Name] = true
				}
				if p.ConfigMap != nil {
					cm[p.ConfigMap.Name] = true
				}
			}
		}
	}
	return sortedKeys(sec), sortedKeys(cm)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
