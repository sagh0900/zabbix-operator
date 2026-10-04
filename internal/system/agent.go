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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/envmerge"
	"github.com/sagh0900/zabbix-operator/internal/podset"
)

// Agent is the component name of the agent DaemonSet.
const Agent = "agent"

// AgentPort is the port of Zabbix agent 2.
const AgentPort = 10050

// AgentName returns the DaemonSet name.
func AgentName(sys *zabbixv1alpha1.ZabbixSystem) string { return sys.Name + "-" + Agent }

// agentManagedKeys are the agent variables the operator owns.
var agentManagedKeys = []string{envHostname, envServerHost, envServerPort, "ZBX_PASSIVESERVERS", "ZBX_ACTIVESERVERS"}

// ApplyAgent writes the desired agent DaemonSet into ds. The agent runs in the node's
// network and process namespaces so it monitors the node itself, and uses the node name as
// its host name. It sends active checks to the role-selected server Service; passive checks
// are accepted from the server pods, whose IPs change.
func ApplyAgent(sys *zabbixv1alpha1.ZabbixSystem, ds *appsv1.DaemonSet, configHash string) {
	s := &sys.Spec.Agent.PodSettings
	port := sys.Spec.Server.Service.Port
	if port == 0 {
		port = TrapperPort
	}
	env := []corev1.EnvVar{
		{Name: envHostname, ValueFrom: fieldRef("spec.nodeName")},
		{Name: envServerHost, Value: ServiceName(sys, Server)},
		{Name: envServerPort, Value: fmt.Sprint(port)},
		{Name: "ZBX_PASSIVESERVERS", Value: anyAddress},
		{Name: "ZBX_PASSIVE_ALLOW", Value: envTrue},
		{Name: "ZBX_ACTIVE_ALLOW", Value: envTrue},
	}
	if sys.Spec.Timezone != "" {
		env = append(env, corev1.EnvVar{Name: "TZ", Value: sys.Spec.Timezone})
	}
	main := corev1.Container{
		Name:         "zabbix-agent2",
		Image:        s.Image,
		Ports:        []corev1.ContainerPort{{Name: "agent", ContainerPort: AgentPort}},
		Env:          envmerge.Merge(env, s.Env, agentManagedKeys),
		EnvFrom:      s.EnvFrom,
		VolumeMounts: s.VolumeMounts,
		Resources:    s.Resources,
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(AgentPort)}},
			PeriodSeconds: 10,
		},
		SecurityContext: s.SecurityContext,
	}
	containers := make([]corev1.Container, 0, 1+len(s.ExtraContainers))
	containers = append(containers, main)
	for _, c := range s.ExtraContainers {
		containers = append(containers, c.Get())
	}
	volumes := make([]corev1.Volume, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		volumes = append(volumes, v.Get())
	}
	selector := podset.SelectorLabels(sys.Name, Agent)
	podLabels := mergeInto(map[string]string{}, s.PodLabels, labels(sys, Agent), selector)
	annotations := mergeInto(map[string]string{}, s.PodAnnotations)
	if configHash != "" {
		annotations[AnnotationConfigHash] = configHash
	}
	one := intstr.FromInt32(1)

	ds.Labels = mergeInto(ds.Labels, labels(sys, Agent), selector)
	ds.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
	ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{
		Type:          appsv1.RollingUpdateDaemonSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: &one},
	}
	ds.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: annotations},
		Spec: corev1.PodSpec{
			HostNetwork:        true,
			HostPID:            true,
			DNSPolicy:          corev1.DNSClusterFirstWithHostNet,
			Containers:         containers,
			Volumes:            volumes,
			NodeSelector:       s.NodeSelector,
			Tolerations:        s.Tolerations,
			Affinity:           s.Affinity,
			PriorityClassName:  s.PriorityClassName,
			ServiceAccountName: s.ServiceAccountName,
			ImagePullSecrets:   s.ImagePullSecrets,
			SecurityContext:    s.PodSecurityContext,
		},
	}
}
