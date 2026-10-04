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
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/podset"
	"github.com/sagh0900/zabbix-operator/internal/system"
)

// stuckReasons are container waiting reasons that do not resolve by waiting.
var stuckReasons = map[string]bool{
	"ImagePullBackOff": true, "ErrImagePull": true, "InvalidImageName": true,
	"CrashLoopBackOff": true, "CreateContainerConfigError": true, "CreateContainerError": true,
}

// explainComponents adds to every component short of ready pods the reason Kubernetes
// gives for the first stuck pod, and records a Warning event when that reason changes.
func (r *SystemReconciler) explainComponents(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, obs *observation) error {
	previous := map[string]string{}
	for _, c := range sys.Status.Components {
		previous[c.Name] = c.Message
	}
	for i := range obs.components {
		c := &obs.components[i]
		if c.Ready >= c.Desired {
			continue
		}
		pods := &corev1.PodList{}
		if err := r.List(ctx, pods, client.InNamespace(sys.Namespace),
			client.MatchingLabels(podset.SelectorLabels(sys.Name, componentLabel(c.Name)))); err != nil {
			return err
		}
		c.Message = podProblem(pods.Items)
		if c.Message != "" && c.Message != previous[c.Name] {
			r.event(sys, corev1.EventTypeWarning, "PodsNotReady", c.Name+": "+c.Message)
		}
	}
	return nil
}

// componentLabel maps a status component name to its pods' component label.
func componentLabel(name string) string {
	if p, ok := strings.CutPrefix(name, "proxy/"); ok {
		return system.ProxyComponentPrefix + p
	}
	return name
}

// podProblem describes the first pod, by name, that Kubernetes reports as unschedulable
// or stuck starting a container, with how many pods share a problem; empty when none is.
func podProblem(pods []corev1.Pod) string {
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	first, n := "", 0
	for i := range pods {
		msg := stuckPod(&pods[i])
		if msg == "" {
			continue
		}
		if n++; first == "" {
			first = pods[i].Name + ": " + msg
		}
	}
	if n > 1 {
		return fmt.Sprintf("%s (and %d more pods)", first, n-1)
	}
	return first
}

func stuckPod(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return ""
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return "unschedulable: " + c.Message
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil && stuckReasons[w.Reason] {
			msg := s.Name + " " + w.Reason
			if w.Message != "" {
				msg += ": " + w.Message
			}
			return msg
		}
	}
	return ""
}
