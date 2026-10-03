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

// Package podset manages a set of bare pods owned by one object, the way CloudNativePG
// manages its instances: pods are created directly with a controller owner reference,
// keep stable names, are replaced one at a time in a caller-defined order when their
// template changes, are recreated when lost, and are protected by a
// PodDisruptionBudget. The package knows nothing about Zabbix.
package podset

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Labels and annotations the package manages.
const (
	LabelManagedBy         = "app.kubernetes.io/managed-by"
	ManagedBy              = "zabbix-operator"
	LabelSystem            = "zabbix.io/system"
	LabelComponent         = "zabbix.io/component"
	AnnotationTemplateHash = "zabbix.io/template-hash"
)

// Member is one desired pod.
type Member struct {
	// Template is the desired pod. Its name is the member's identity; the namespace,
	// owner reference, set labels and template hash are added by the package.
	Template *corev1.Pod
	// Order sets the replacement order: lower values are replaced first.
	Order int
	// LiveLabels are kept on the pod without replacing it, for example an HA role.
	// They are not part of the template hash.
	LiveLabels map[string]string
}

// Set is the desired state of one component.
type Set struct {
	// Owner is the controller of every pod and of the PodDisruptionBudget.
	Owner client.Object
	// System and Component label the pods and select them.
	System    string
	Component string
	// Members are the desired pods. An empty list removes all pods of the component.
	Members []Member
	// Healthy reports whether a pod is serving. Nil means the pod is Ready.
	Healthy func(*corev1.Pod) bool
	// Hold, when set, stops all creation, replacement and scale-down and is reported as
	// the reason. Lost pods are not recreated while the hold lasts.
	Hold string
	// DisruptionBudget maintains a PodDisruptionBudget allowing one voluntary
	// disruption at a time.
	DisruptionBudget bool
}

// Status is the outcome of one reconcile pass.
type Status struct {
	// Desired is the number of members.
	Desired int32
	// Healthy is the number of members whose pod is healthy.
	Healthy int32
	// Conflicts names objects the set needs but that are owned by something else.
	Conflicts []string
	// Action describes what the pass did or is waiting for; empty when converged.
	Action string
	// Converged is true when every member exists, is current and is healthy.
	Converged bool
}

// Manager reconciles pod sets.
type Manager struct {
	Client client.Client
	Scheme *runtime.Scheme
}

// SelectorLabels are the labels that identify the pods of a set.
func SelectorLabels(system, component string) map[string]string {
	return map[string]string{LabelManagedBy: ManagedBy, LabelSystem: system, LabelComponent: component}
}

// observed is a member's current pod.
type observed struct {
	member  *Member
	pod     *corev1.Pod
	hash    string
	healthy bool
}

// Reconcile moves the pods of set one step towards the desired state. It is idempotent
// and is meant to be called again whenever a pod of the set changes. Each pass takes at
// most one disruptive step: create missing pods, or replace one outdated pod.
func (m *Manager) Reconcile(ctx context.Context, set Set) (Status, error) {
	st := Status{Desired: int32(len(set.Members))}

	owned, err := m.ownedPods(ctx, set)
	if err != nil {
		return st, err
	}
	actions, err := m.removeUnwanted(ctx, set, owned)
	if err != nil {
		return st, err
	}
	members, err := m.observe(ctx, set, owned, &st)
	if err != nil {
		return st, err
	}
	if set.DisruptionBudget {
		conflict, err := m.reconcileBudget(ctx, set)
		if err != nil {
			return st, err
		}
		if conflict != "" {
			st.Conflicts = append(st.Conflicts, conflict)
		}
	}
	for _, o := range members {
		if o.pod != nil && o.pod.DeletionTimestamp == nil {
			if err := m.applyLiveLabels(ctx, o.pod, o.member.LiveLabels); err != nil {
				return st, err
			}
		}
	}

	if set.Hold != "" {
		st.Action = joinActions(append(actions, "on hold: "+set.Hold))
		return st, nil
	}
	created, err := m.createMissing(ctx, set, members, st.Conflicts)
	if err != nil {
		return st, err
	}
	if actions = append(actions, created...); len(actions) > 0 {
		st.Action = joinActions(actions)
		return st, nil
	}
	if st.Action, err = m.rollOne(ctx, members); err != nil || st.Action != "" {
		return st, err
	}
	st.Converged = len(st.Conflicts) == 0
	return st, nil
}

// ownedPods returns the pods of the set controlled by its owner, by name.
func (m *Manager) ownedPods(ctx context.Context, set Set) (map[string]*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := m.Client.List(ctx, pods, client.InNamespace(set.Owner.GetNamespace()),
		client.MatchingLabels(SelectorLabels(set.System, set.Component))); err != nil {
		return nil, err
	}
	owned := map[string]*corev1.Pod{}
	for i := range pods.Items {
		if p := &pods.Items[i]; m.ownedBy(p, set.Owner) {
			owned[p.Name] = p
		}
	}
	return owned, nil
}

// removeUnwanted deletes failed pods and, unless the set is on hold, pods that are no
// longer members.
func (m *Manager) removeUnwanted(ctx context.Context, set Set, owned map[string]*corev1.Pod) ([]string, error) {
	wanted := map[string]bool{}
	for _, mem := range set.Members {
		wanted[mem.Template.Name] = true
	}
	var actions []string
	for name, p := range owned {
		var why string
		switch {
		case p.DeletionTimestamp != nil:
			continue
		case !wanted[name] && set.Hold == "":
			why = "no longer desired"
		case p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded:
			why = terminalReason(p)
		default:
			continue
		}
		if err := m.delete(ctx, p); err != nil {
			return nil, err
		}
		actions = append(actions, fmt.Sprintf("deleting %s (%s)", name, why))
	}
	return actions, nil
}

// observe pairs every member with its pod, counts healthy members and records conflicts.
func (m *Manager) observe(ctx context.Context, set Set, owned map[string]*corev1.Pod, st *Status) ([]observed, error) {
	healthy := set.Healthy
	if healthy == nil {
		healthy = isReady
	}
	members := make([]observed, 0, len(set.Members))
	for i := range set.Members {
		mem := &set.Members[i]
		o := observed{member: mem, hash: TemplateHash(mem.Template)}
		if p, ok := owned[mem.Template.Name]; ok {
			o.pod = p
			o.healthy = p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning && healthy(p)
			if o.healthy {
				st.Healthy++
			}
		} else {
			conflict, err := m.conflict(ctx, set.Owner.GetNamespace(), mem.Template.Name, set.Owner)
			if err != nil {
				return nil, err
			}
			if conflict != "" {
				st.Conflicts = append(st.Conflicts, conflict)
			}
		}
		members = append(members, o)
	}
	return members, nil
}

// createMissing creates every member without a pod, except those blocked by a conflict.
func (m *Manager) createMissing(ctx context.Context, set Set, members []observed, conflicts []string) ([]string, error) {
	var actions []string
	for _, o := range members {
		if o.pod != nil || containsPrefix(conflicts, "pod "+o.member.Template.Name+" ") {
			continue
		}
		if err := m.create(ctx, set, o.member, o.hash); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return nil, err
		}
		actions = append(actions, "creating "+o.member.Template.Name)
	}
	return actions, nil
}

// rollOne replaces at most one outdated member, and only while every other member is
// healthy. It returns what it did or is waiting for; empty means nothing is left to do.
func (m *Manager) rollOne(ctx context.Context, members []observed) (string, error) {
	var outdated []observed
	for _, o := range members {
		if o.pod == nil {
			continue
		}
		if o.pod.DeletionTimestamp != nil {
			return fmt.Sprintf("waiting for %s to terminate", o.pod.Name), nil
		}
		if o.pod.Annotations[AnnotationTemplateHash] != o.hash {
			outdated = append(outdated, o)
		}
	}
	if len(outdated) == 0 {
		for _, o := range members {
			if o.pod != nil && !o.healthy {
				return fmt.Sprintf("waiting for %s to become healthy", o.member.Template.Name), nil
			}
		}
		return "", nil
	}
	sort.SliceStable(outdated, func(i, j int) bool {
		a, b := outdated[i], outdated[j]
		if a.healthy != b.healthy {
			return !a.healthy // broken pods first: they may need the new template to recover
		}
		if a.member.Order != b.member.Order {
			return a.member.Order < b.member.Order
		}
		return a.member.Template.Name < b.member.Template.Name
	})
	next := outdated[0]
	for _, o := range members {
		if o.member != next.member && o.pod != nil && !o.healthy {
			return fmt.Sprintf("waiting for %s to become healthy before replacing %s",
				o.member.Template.Name, next.member.Template.Name), nil
		}
	}
	if err := m.delete(ctx, next.pod); err != nil {
		return "", err
	}
	return fmt.Sprintf("replacing %s (template changed); %d outdated",
		next.member.Template.Name, len(outdated)), nil
}

// ownedBy reports whether owner is the controller of obj.
func (m *Manager) ownedBy(obj client.Object, owner client.Object) bool {
	ref := metav1.GetControllerOf(obj)
	return ref != nil && ref.UID == owner.GetUID()
}

// conflict returns a description if a pod named name exists but is not controlled by owner.
func (m *Manager) conflict(ctx context.Context, ns, name string, owner client.Object) (string, error) {
	p := &corev1.Pod{}
	err := m.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, p)
	switch {
	case apierrors.IsNotFound(err):
		return "", nil
	case err != nil:
		return "", err
	case m.ownedBy(p, owner):
		return "", nil
	}
	return fmt.Sprintf("pod %s exists and is not managed by this %s", name, owner.GetObjectKind().GroupVersionKind().Kind), nil
}

func (m *Manager) create(ctx context.Context, set Set, mem *Member, hash string) error {
	p := mem.Template.DeepCopy()
	p.Namespace = set.Owner.GetNamespace()
	p.ResourceVersion = ""
	p.Labels = mergeMaps(p.Labels, mem.LiveLabels, SelectorLabels(set.System, set.Component))
	p.Annotations = mergeMaps(p.Annotations, map[string]string{AnnotationTemplateHash: hash})
	if err := controllerutil.SetControllerReference(set.Owner, p, m.Scheme); err != nil {
		return err
	}
	return m.Client.Create(ctx, p)
}

func (m *Manager) delete(ctx context.Context, p *corev1.Pod) error {
	err := m.Client.Delete(ctx, p, client.Preconditions{UID: &p.UID})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

func (m *Manager) applyLiveLabels(ctx context.Context, p *corev1.Pod, live map[string]string) error {
	changed := false
	for k, v := range live {
		if p.Labels[k] != v {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	patch := client.MergeFrom(p.DeepCopy())
	p.Labels = mergeMaps(p.Labels, live)
	return m.Client.Patch(ctx, p, patch)
}

// reconcileBudget keeps a PodDisruptionBudget that allows one voluntary disruption at a
// time, and removes it when the set has no members.
func (m *Manager) reconcileBudget(ctx context.Context, set Set) (string, error) {
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Namespace: set.Owner.GetNamespace(),
		Name:      set.System + "-" + set.Component,
	}}
	err := m.Client.Get(ctx, client.ObjectKeyFromObject(pdb), pdb)
	switch {
	case apierrors.IsNotFound(err):
		if len(set.Members) == 0 {
			return "", nil
		}
	case err != nil:
		return "", err
	case !m.ownedBy(pdb, set.Owner):
		return fmt.Sprintf("PodDisruptionBudget %s exists and is not managed by this %s",
			pdb.Name, set.Owner.GetObjectKind().GroupVersionKind().Kind), nil
	case len(set.Members) == 0:
		return "", client.IgnoreNotFound(m.Client.Delete(ctx, pdb))
	}

	_, err = controllerutil.CreateOrPatch(ctx, m.Client, pdb, func() error {
		one := intstr.FromInt32(1)
		pdb.Labels = mergeMaps(pdb.Labels, SelectorLabels(set.System, set.Component))
		pdb.Spec.MaxUnavailable = &one
		pdb.Spec.MinAvailable = nil
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: SelectorLabels(set.System, set.Component)}
		return controllerutil.SetControllerReference(set.Owner, pdb, m.Scheme)
	})
	return "", err
}

func isReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func terminalReason(p *corev1.Pod) string {
	if p.Status.Reason != "" {
		return "pod " + string(p.Status.Phase) + ": " + p.Status.Reason
	}
	return "pod " + string(p.Status.Phase)
}

func mergeMaps(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func containsPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func joinActions(a []string) string {
	sort.Strings(a)
	out := ""
	for i, s := range a {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
