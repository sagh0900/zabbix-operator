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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPodProblem(t *testing.T) {
	unschedulable := func(name string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/7 nodes are available: 1 node(s) didn't have free ports for the requested pod ports"}}}}
	}
	waiting := func(name, reason string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "zabbix-web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "not found"}}}}}}
	}
	for _, c := range []struct {
		name string
		pods []corev1.Pod
		want string
	}{
		{"none", []corev1.Pod{waiting("a", "ContainerCreating")}, ""},
		{"unschedulable", []corev1.Pod{unschedulable("agent-b"), unschedulable("agent-a")},
			"agent-a: unschedulable: 0/7 nodes are available: 1 node(s) didn't have free ports for the requested pod ports (and 1 more pods)"},
		{"image pull", []corev1.Pod{waiting("zabbix-web-0", "ImagePullBackOff")}, "zabbix-web-0: zabbix-web ImagePullBackOff: not found"},
	} {
		if got := podProblem(c.pods); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// A pod stuck pulling its image is named in the component status and an event.
func TestSystem_ComponentExplainsStuckPods(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	ctx := context.Background()
	eventually(t, func() error {
		p := getPod(t, ns, "zabbix-web-0")
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "zabbix-web", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ImagePullBackOff", Message: "Back-off pulling image"}}}}
		return k8s.Status().Update(ctx, p)
	})
	eventually(t, func() error {
		for _, c := range getSystem(t, ns).Status.Components {
			if c.Name == "web" && strings.Contains(c.Message, "zabbix-web-0: zabbix-web ImagePullBackOff") {
				return nil
			}
		}
		return fmt.Errorf("web component without the pull problem: %+v", getSystem(t, ns).Status.Components)
	})
	waitEvent(t, ns, corev1.EventTypeWarning, "PodsNotReady", "web: zabbix-web-0: zabbix-web ImagePullBackOff")
}
