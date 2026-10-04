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
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/metrics"
	"github.com/sagh0900/zabbix-operator/internal/podset"
	"github.com/sagh0900/zabbix-operator/internal/system"
)

// suspendWaits returns why a requested suspend must wait, or "" when it can start: a
// schema creation or schema upgrade in progress is never interrupted.
func (r *SystemReconciler) suspendWaits(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem) (string, error) {
	if sys.Status.UpgradeStep != "" {
		return "the schema upgrade to " + sys.Status.UpgradeTarget, nil
	}
	if sys.Status.RunningVersion != "" {
		return "", nil
	}
	pods, err := r.componentPods(ctx, sys, system.ServerInit)
	if err != nil || len(pods) == 0 {
		return "", err
	}
	return "the schema creation", nil
}

// suspend stops the system's pods one stage at a time, each stage only after the previous
// one has fully terminated: frontend and web service; proxies and agents; standby servers;
// the active server. Pods receive SIGTERM and their grace period, so a server marks its
// ha_node row stopped. Services, Ingresses and the status stay.
func (r *SystemReconciler) suspend(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, obs *observation) (time.Duration, error) {
	obs.phase = zabbixv1alpha1.PhaseSuspended
	obs.components = []zabbixv1alpha1.ComponentStatus{}
	metrics.DeleteComponents(sys.Namespace, sys.Name)
	if sys.Status.Phase != zabbixv1alpha1.PhaseSuspended {
		r.event(sys, corev1.EventTypeNormal, "Suspending", "Stopping all Zabbix pods")
	}
	for _, stage := range []func(context.Context, *zabbixv1alpha1.ZabbixSystem) (string, string, error){
		r.stopStateless, r.stopEdge, r.stopServers,
	} {
		what, action, err := stage(ctx, sys)
		if err != nil {
			return 0, err
		}
		if what != "" {
			obs.reason = "Suspending: stopping " + what
			if action != "" {
				obs.reason += ": " + action
			}
			return 2 * time.Second, nil
		}
	}
	obs.reason = "Suspended: all Zabbix pods are stopped; set spec.suspend to false to resume"
	if !strings.HasPrefix(sys.Status.PhaseReason, "Suspended") {
		r.event(sys, corev1.EventTypeNormal, "Suspended", "All Zabbix pods are stopped")
	}
	return 0, nil
}

// A suspend stage stops one group of pods. It returns what it is still stopping and the
// current action, or empty strings once everything in the group is gone.

func (r *SystemReconciler) stopStateless(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem) (string, string, error) {
	for _, c := range []string{system.Web, system.WebService} {
		st, err := r.podsetReconcile(ctx, sys, c, nil, nil, "")
		if err != nil || st.Action != "" {
			return "the frontend and web service", st.Action, err
		}
	}
	return "", "", nil
}

// stopEdge stops every proxy, including proxies no longer in the spec, and the agents.
func (r *SystemReconciler) stopEdge(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem) (string, string, error) {
	const what = "the proxies and agents"
	proxies := map[string]bool{}
	for i := range sys.Spec.Proxies {
		proxies[system.ProxyComponent(&sys.Spec.Proxies[i])] = true
	}
	all := &corev1.PodList{}
	if err := r.List(ctx, all, client.InNamespace(sys.Namespace), client.MatchingLabels{
		podset.LabelManagedBy: podset.ManagedBy, podset.LabelSystem: sys.Name}); err != nil {
		return what, "", err
	}
	for _, p := range all.Items {
		if c := p.Labels[podset.LabelComponent]; strings.HasPrefix(c, system.ProxyComponentPrefix) {
			proxies[c] = true
		}
	}
	names := make([]string, 0, len(proxies))
	for c := range proxies {
		names = append(names, c)
	}
	sort.Strings(names)
	var actions []string
	for _, c := range names {
		st, err := r.podsetReconcile(ctx, sys, c, nil, nil, "")
		if err != nil {
			return what, "", err
		}
		if st.Action != "" {
			actions = append(actions, st.Action)
		}
	}
	gone, err := r.stopAgent(ctx, sys)
	if err != nil {
		return what, "", err
	}
	if !gone {
		actions = append(actions, "deleting DaemonSet "+system.AgentName(sys))
	}
	if len(actions) > 0 {
		return what, strings.Join(actions, "; "), nil
	}
	return "", "", nil
}

// stopServers stops the standby servers first, so the active node works until last.
func (r *SystemReconciler) stopServers(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem) (string, string, error) {
	const what = "the servers"
	if st, err := r.podsetReconcile(ctx, sys, system.ServerInit, nil, nil, ""); err != nil || st.Action != "" {
		return "the schema-creation server", st.Action, err
	}
	pods, err := r.componentPods(ctx, sys, system.Server)
	if err != nil {
		return what, "", err
	}
	var standby []*corev1.Pod
	for _, p := range pods {
		if p.DeletionTimestamp != nil {
			return what, "waiting for " + p.Name + " to terminate", nil
		}
		if !r.isActive(ctx, p) {
			standby = append(standby, p)
		}
	}
	if len(standby) > 0 && len(standby) < len(pods) {
		for _, p := range standby {
			if err := r.Delete(ctx, p, client.Preconditions{UID: &p.UID}); client.IgnoreNotFound(err) != nil {
				return "the standby servers", "", err
			}
			metrics.PodReplacements.WithLabelValues(sys.Namespace, sys.Name, system.Server, string(podset.DeletedScaleDown)).Inc()
		}
		return "the standby servers", "", nil
	}
	st, err := r.podsetReconcile(ctx, sys, system.Server, nil, nil, "")
	if err != nil || st.Action != "" {
		return what, st.Action, err
	}
	return "", "", nil
}

// stopAgent deletes the agent DaemonSet and reports whether it is gone.
func (r *SystemReconciler) stopAgent(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem) (bool, error) {
	metrics.DeleteAgent(sys.Namespace, sys.Name)
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sys.Namespace, Name: system.AgentName(sys)}, ds); err != nil {
		return true, client.IgnoreNotFound(err)
	}
	if ref := metav1.GetControllerOf(ds); ref == nil || ref.UID != sys.UID {
		return true, nil
	}
	if ds.DeletionTimestamp == nil {
		if err := r.Delete(ctx, ds); client.IgnoreNotFound(err) != nil {
			return false, err
		}
	}
	return false, nil
}
