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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/metrics"
	"github.com/sagh0900/zabbix-operator/internal/podset"
	"github.com/sagh0900/zabbix-operator/internal/system"
)

// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete

// reconcileProxies keeps every enabled proxy at version and removes the pods of proxies
// that were disabled or deleted from the spec. It returns the first action in progress.
func (r *SystemReconciler) reconcileProxies(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	version, hold string, obs *observation) (string, error) {
	action := ""
	wanted := map[string]bool{}
	for i := range sys.Spec.Proxies {
		p := &sys.Spec.Proxies[i]
		var members []podset.Member
		if p.IsEnabled() {
			wanted[system.ProxyComponent(p)] = true
			in := r.input(ctx, sys, db, version, &p.PodSettings, false)
			replicas := int(p.Replicas)
			if replicas < 1 {
				replicas = 1
			}
			for j := 0; j < replicas; j++ {
				name := system.ProxyPodName(p, j)
				members = append(members, podset.Member{Template: system.ProxyPod(in, p, name)})
			}
		}
		st, err := r.podsetReconcile(ctx, sys, system.ProxyComponent(p), members, nil, hold)
		if err != nil {
			return "", err
		}
		if p.IsEnabled() {
			obs.add("proxy/"+p.Name, st)
		}
		if action == "" && st.Action != "" {
			action = "Proxy " + p.Name + ": " + st.Action
		}
	}

	// Proxies deleted from the spec: their pods and PodDisruptionBudget go away.
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(sys.Namespace), client.MatchingLabels{
		podset.LabelManagedBy: podset.ManagedBy, podset.LabelSystem: sys.Name}); err != nil {
		return "", err
	}
	gone := map[string]bool{}
	for _, p := range pods.Items {
		c := p.Labels[podset.LabelComponent]
		if strings.HasPrefix(c, system.ProxyComponentPrefix) && !wanted[c] && !inSpec(sys, c) {
			gone[c] = true
		}
	}
	for c := range gone {
		if _, err := r.podsetReconcile(ctx, sys, c, nil, nil, ""); err != nil {
			return "", err
		}
	}
	return action, nil
}

// inSpec reports whether a proxy component still has an entry in the spec (enabled or not).
func inSpec(sys *zabbixv1alpha1.ZabbixSystem, component string) bool {
	for i := range sys.Spec.Proxies {
		if system.ProxyComponent(&sys.Spec.Proxies[i]) == component {
			return true
		}
	}
	return false
}

// reconcileAgent keeps the agent DaemonSet when the agent is enabled and removes it
// otherwise. Agent readiness is reported but does not change the system's phase: it is
// node coverage, not the health of the Zabbix service.
func (r *SystemReconciler) reconcileAgent(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, obs *observation) error {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: sys.Namespace, Name: system.AgentName(sys)}}
	if !sys.Spec.Agent.Enabled {
		metrics.DeleteAgent(sys.Namespace, sys.Name)
		if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
			return client.IgnoreNotFound(err)
		}
		if ref := metav1.GetControllerOf(ds); ref != nil && ref.UID == sys.UID {
			return client.IgnoreNotFound(r.Delete(ctx, ds))
		}
		return nil
	}
	in := r.input(ctx, sys, nil, "", &sys.Spec.Agent.PodSettings, false)
	ok, err := r.applyOwned(ctx, sys, ds, func() { system.ApplyAgent(sys, ds, in.ConfigHash) })
	if err != nil {
		return err
	}
	if !ok {
		obs.conflicts = append(obs.conflicts, fmt.Sprintf("DaemonSet %s exists and is not managed by this ZabbixSystem", ds.Name))
		return nil
	}
	desired, ready := ds.Status.DesiredNumberScheduled, ds.Status.NumberReady
	obs.components = append(obs.components, zabbixv1alpha1.ComponentStatus{Name: system.Agent, Desired: desired, Ready: ready})
	metrics.AgentNodesDesired.WithLabelValues(sys.Namespace, sys.Name).Set(float64(desired))
	metrics.AgentNodesReady.WithLabelValues(sys.Namespace, sys.Name).Set(float64(ready))
	return nil
}
