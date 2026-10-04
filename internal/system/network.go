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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/podset"
)

// LabelRole marks the active HA server node; the server Service selects it.
const (
	LabelRole   = "zabbix.io/role"
	RoleActive  = "active"
	RoleStandby = "standby"
)

// ServiceSpec describes the Service of a component.
type ServiceSpec struct {
	// Name of the Service; empty means ServiceName(system, Component).
	Name       string
	Component  string
	PortName   string
	Port       int32 // default Service port
	TargetPort string
	Settings   zabbixv1alpha1.ServiceSettings
	// Headless gives every pod its own DNS name instead of one virtual IP.
	Headless bool
}

// ServiceObjectName returns the name of the Service described by s.
func ServiceObjectName(sys *zabbixv1alpha1.ZabbixSystem, s ServiceSpec) string {
	if s.Name != "" {
		return s.Name
	}
	return ServiceName(sys, s.Component)
}

// Services returns the Services the system needs.
func Services(sys *zabbixv1alpha1.ZabbixSystem) []ServiceSpec {
	out := []ServiceSpec{{Component: Server, PortName: portTrapper, Port: TrapperPort, TargetPort: portTrapper,
		Settings: sys.Spec.Server.Service}}
	if sys.Spec.Web.IsEnabled() {
		out = append(out, ServiceSpec{Component: Web, PortName: portHTTP, Port: 80, TargetPort: portHTTP, Settings: sys.Spec.Web.Service})
	}
	if sys.Spec.WebService.IsEnabled() {
		out = append(out, ServiceSpec{Component: WebService, PortName: portReport, Port: WebServicePort, TargetPort: portReport,
			Settings: sys.Spec.WebService.Service})
	}
	for i := range sys.Spec.Proxies {
		p := &sys.Spec.Proxies[i]
		if !p.IsEnabled() {
			continue
		}
		// A headless Service gives every proxy instance a stable DNS name; a LoadBalancer or
		// NodePort setting adds a second Service for clients outside the cluster.
		out = append(out, ServiceSpec{Name: p.Name, Component: ProxyComponent(p), PortName: portTrapper, Port: TrapperPort,
			TargetPort: portTrapper, Headless: true})
		if t := p.Service.Type; t == corev1.ServiceTypeLoadBalancer || t == corev1.ServiceTypeNodePort {
			out = append(out, ServiceSpec{Name: p.Name + "-external", Component: ProxyComponent(p), PortName: portTrapper,
				Port: TrapperPort, TargetPort: portTrapper, Settings: p.Service})
		}
	}
	return out
}

// ApplyService writes the desired state of s into svc, keeping fields the API server
// owns (cluster IPs, allocated node ports when none is requested).
func ApplyService(sys *zabbixv1alpha1.ZabbixSystem, s ServiceSpec, svc *corev1.Service) {
	set := s.Settings
	svc.Labels = mergeInto(svc.Labels, labels(sys, s.Component), podset.SelectorLabels(sys.Name, s.Component))
	applyUserMetadata(&svc.Labels, &svc.Annotations, set.Labels, set.Annotations)
	svc.Spec.Selector = podset.SelectorLabels(sys.Name, s.Component)
	if s.Component == Server {
		// Only the active HA node accepts trapper connections.
		svc.Spec.Selector[LabelRole] = RoleActive
	}
	svc.Spec.Type = set.Type
	if svc.Spec.Type == "" || s.Headless {
		svc.Spec.Type = corev1.ServiceTypeClusterIP
	}
	if s.Headless {
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		// Proxies must be resolvable while they start, before they are Ready.
		svc.Spec.PublishNotReadyAddresses = true
	}
	port := set.Port
	if port == 0 {
		port = s.Port
	}
	nodePort := set.NodePort
	if nodePort == 0 && len(svc.Spec.Ports) == 1 && svc.Spec.Type != corev1.ServiceTypeClusterIP {
		nodePort = svc.Spec.Ports[0].NodePort
	}
	if svc.Spec.Type == corev1.ServiceTypeClusterIP {
		nodePort = 0
	}
	svc.Spec.Ports = []corev1.ServicePort{{
		Name: s.PortName, Port: port, TargetPort: intstr.FromString(s.TargetPort),
		Protocol: corev1.ProtocolTCP, NodePort: nodePort,
	}}
	svc.Spec.LoadBalancerIP = set.LoadBalancerIP //nolint:staticcheck // still the portable way to request an address
	svc.Spec.LoadBalancerClass = set.LoadBalancerClass
	svc.Spec.LoadBalancerSourceRanges = set.LoadBalancerSourceRanges
	svc.Spec.ExternalTrafficPolicy = ""
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		svc.Spec.ExternalTrafficPolicy = set.ExternalTrafficPolicy
		if svc.Spec.ExternalTrafficPolicy == "" {
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
		}
	}
}

// IngressSpec describes the Ingress of an HTTP component.
type IngressSpec struct {
	Component   string
	ServicePort int32
	Settings    zabbixv1alpha1.IngressSettings
}

// Ingresses returns the Ingresses the system needs.
func Ingresses(sys *zabbixv1alpha1.ZabbixSystem) []IngressSpec {
	var out []IngressSpec
	if sys.Spec.Web.IsEnabled() && sys.Spec.Web.Ingress.Enabled {
		port := sys.Spec.Web.Service.Port
		if port == 0 {
			port = 80
		}
		out = append(out, IngressSpec{Web, port, sys.Spec.Web.Ingress})
	}
	if sys.Spec.WebService.IsEnabled() && sys.Spec.WebService.Ingress.Enabled {
		port := sys.Spec.WebService.Service.Port
		if port == 0 {
			port = WebServicePort
		}
		out = append(out, IngressSpec{WebService, port, sys.Spec.WebService.Ingress})
	}
	return out
}

// ApplyIngress writes the desired state of s into ing.
func ApplyIngress(sys *zabbixv1alpha1.ZabbixSystem, s IngressSpec, ing *networkingv1.Ingress) {
	set := s.Settings
	ing.Labels = mergeInto(ing.Labels, labels(sys, s.Component), podset.SelectorLabels(sys.Name, s.Component))
	applyUserMetadata(&ing.Labels, &ing.Annotations, set.Labels, set.Annotations)
	ing.Spec.IngressClassName = set.ClassName
	ing.Spec.TLS = set.TLS
	backend := networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
		Name: ServiceName(sys, s.Component), Port: networkingv1.ServiceBackendPort{Number: s.ServicePort}}}
	prefix := networkingv1.PathTypePrefix
	ing.Spec.Rules = nil
	for _, h := range set.Hosts {
		paths := h.Paths
		if len(paths) == 0 {
			paths = []string{"/"}
		}
		var hp []networkingv1.HTTPIngressPath
		for _, p := range paths {
			hp = append(hp, networkingv1.HTTPIngressPath{Path: p, PathType: &prefix, Backend: backend})
		}
		ing.Spec.Rules = append(ing.Spec.Rules, networkingv1.IngressRule{Host: h.Host,
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: hp}}})
	}
	if len(set.Hosts) == 0 {
		ing.Spec.DefaultBackend = &backend
	} else {
		ing.Spec.DefaultBackend = nil
	}
}

// IngressName returns the Ingress name of a component.
func IngressName(sys *zabbixv1alpha1.ZabbixSystem, component string) string {
	return ServiceName(sys, component)
}

// Annotations recording which user labels and annotations the operator set, so keys
// removed from the spec are removed from the object while keys other controllers add are
// left alone.
const (
	annotationUserLabels      = "zabbix.io/user-labels"
	annotationUserAnnotations = "zabbix.io/user-annotations"
)

// applyUserMetadata sets the user's labels and annotations on an object and removes the
// ones the user previously set but no longer wants.
func applyUserMetadata(objLabels, objAnnotations *map[string]string, userLabels, userAnnotations map[string]string) {
	if *objLabels == nil {
		*objLabels = map[string]string{}
	}
	if *objAnnotations == nil {
		*objAnnotations = map[string]string{}
	}
	prevLabels := splitKeys((*objAnnotations)[annotationUserLabels])
	prevAnnotations := splitKeys((*objAnnotations)[annotationUserAnnotations])
	for _, k := range prevLabels {
		if _, keep := userLabels[k]; !keep {
			delete(*objLabels, k)
		}
	}
	for _, k := range prevAnnotations {
		if _, keep := userAnnotations[k]; !keep {
			delete(*objAnnotations, k)
		}
	}
	for k, v := range userLabels {
		(*objLabels)[k] = v
	}
	for k, v := range userAnnotations {
		(*objAnnotations)[k] = v
	}
	setKeys(*objAnnotations, annotationUserLabels, userLabels)
	setKeys(*objAnnotations, annotationUserAnnotations, userAnnotations)
}

func splitKeys(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func setKeys(annotations map[string]string, key string, m map[string]string) {
	if len(m) == 0 {
		delete(annotations, key)
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	annotations[key] = strings.Join(keys, ",")
}

// mergeInto returns base with every map in add applied in order; later maps win.
func mergeInto(base map[string]string, add ...map[string]string) map[string]string {
	if base == nil {
		base = map[string]string{}
	}
	for _, m := range add {
		for k, v := range m {
			base[k] = v
		}
	}
	return base
}
