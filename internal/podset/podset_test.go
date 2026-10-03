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

package podset

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const ns = "zabbix"

type harness struct {
	t     *testing.T
	c     client.Client
	m     *Manager
	owner *corev1.ConfigMap
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: "owner-uid"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, owner)...).Build()
	return &harness{t: t, c: c, m: &Manager{Client: c, Scheme: scheme}, owner: owner}
}

// template returns a pod template for name with an env value that stands for "config".
func template(name, config string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": "zabbix"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "main", Image: "zabbix/zabbix-server-pgsql:ubuntu-7.0.25",
			Env: []corev1.EnvVar{{Name: "CONFIG", Value: config}},
		}}},
	}
}

// set returns a server-like set: members ordered by order[i] (lower replaced first).
func (h *harness) set(config string, names ...string) Set {
	s := Set{Owner: h.owner, System: "zabbix", Component: "server", DisruptionBudget: true}
	for i, n := range names {
		s.Members = append(s.Members, Member{Template: template(n, config), Order: i})
	}
	return s
}

func (h *harness) reconcile(s Set) Status {
	h.t.Helper()
	st, err := h.m.Reconcile(context.Background(), s)
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return st
}

func (h *harness) pod(name string) *corev1.Pod {
	h.t.Helper()
	p := &corev1.Pod{}
	err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, p)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// run marks pods as running and healthy (ready), like the kubelet would.
func (h *harness) run(names ...string) {
	h.t.Helper()
	for _, n := range names {
		p := h.pod(n)
		if p == nil {
			h.t.Fatalf("pod %s missing", n)
		}
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := h.c.Status().Update(context.Background(), p); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *harness) podNames() []string {
	h.t.Helper()
	list := &corev1.PodList{}
	if err := h.c.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, 0, len(list.Items))
	for _, p := range list.Items {
		out = append(out, p.Name)
	}
	return out
}

func expectAction(t *testing.T, st Status, want string) {
	t.Helper()
	if !strings.Contains(st.Action, want) {
		t.Fatalf("action %q, want it to contain %q", st.Action, want)
	}
}

func TestCreatesMembersWithOwnershipLabelsAndHash(t *testing.T) {
	h := newHarness(t)
	st := h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1"))
	expectAction(t, st, "creating zabbix-server-0")
	expectAction(t, st, "creating zabbix-server-1")

	p := h.pod("zabbix-server-0")
	ref := metav1.GetControllerOf(p)
	if ref == nil || ref.UID != h.owner.UID || ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Fatalf("controller reference = %+v", ref)
	}
	for k, v := range SelectorLabels("zabbix", "server") {
		if p.Labels[k] != v {
			t.Errorf("label %s = %q, want %q", k, p.Labels[k], v)
		}
	}
	if p.Labels["app"] != "zabbix" {
		t.Errorf("template label lost: %v", p.Labels)
	}
	if p.Annotations[AnnotationTemplateHash] != TemplateHash(template("zabbix-server-0", "a")) {
		t.Errorf("hash annotation %q", p.Annotations[AnnotationTemplateHash])
	}
}

func TestConvergedPassChangesNothing(t *testing.T) {
	h := newHarness(t)
	s := h.set("a", "zabbix-server-0", "zabbix-server-1")
	h.reconcile(s)
	h.run("zabbix-server-0", "zabbix-server-1")
	before := h.pod("zabbix-server-0").ResourceVersion

	st := h.reconcile(s)
	if !st.Converged || st.Action != "" || st.Healthy != 2 || st.Desired != 2 {
		t.Fatalf("status = %+v, want converged", st)
	}
	if h.pod("zabbix-server-0").ResourceVersion != before {
		t.Fatal("converged pass modified a pod")
	}
}

// A template change replaces one pod at a time, lowest order first, and never while
// another member is unhealthy.
func TestRollsOnePodAtATimeInOrder(t *testing.T) {
	h := newHarness(t)
	h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1"))
	h.run("zabbix-server-0", "zabbix-server-1")

	// Server-like order: zabbix-server-1 is active and goes last.
	s := h.set("b", "zabbix-server-0", "zabbix-server-1")
	s.Members[0].Order, s.Members[1].Order = 0, 1

	expectAction(t, h.reconcile(s), "replacing zabbix-server-0")
	if h.pod("zabbix-server-0") != nil || h.pod("zabbix-server-1") == nil {
		t.Fatalf("pods after first step: %v", h.podNames())
	}

	expectAction(t, h.reconcile(s), "creating zabbix-server-0")
	expectAction(t, h.reconcile(s), "waiting for zabbix-server-0 to become healthy before replacing zabbix-server-1")
	if h.pod("zabbix-server-1") == nil {
		t.Fatal("active pod replaced while the new standby was not healthy")
	}

	h.run("zabbix-server-0")
	expectAction(t, h.reconcile(s), "replacing zabbix-server-1")
	expectAction(t, h.reconcile(s), "creating zabbix-server-1")
	h.run("zabbix-server-1")
	if st := h.reconcile(s); !st.Converged {
		t.Fatalf("status = %+v, want converged", st)
	}
	for _, n := range []string{"zabbix-server-0", "zabbix-server-1"} {
		if h.pod(n).Annotations[AnnotationTemplateHash] != TemplateHash(template(n, "b")) {
			t.Errorf("%s not on the new template", n)
		}
	}
}

// A pod broken by its current template is replaced even though it is unhealthy;
// otherwise a bad configuration could never be fixed.
func TestBrokenOutdatedPodIsReplacedFirst(t *testing.T) {
	h := newHarness(t)
	h.reconcile(h.set("bad", "zabbix-server-0", "zabbix-server-1"))
	h.run("zabbix-server-0") // zabbix-server-1 crash-loops

	s := h.set("fixed", "zabbix-server-0", "zabbix-server-1")
	expectAction(t, h.reconcile(s), "replacing zabbix-server-1")
	if h.pod("zabbix-server-0") == nil {
		t.Fatal("healthy pod replaced before the broken one")
	}
}

func TestWaitsForTerminatingPod(t *testing.T) {
	h := newHarness(t)
	s := h.set("a", "zabbix-server-0")
	h.reconcile(s)
	h.run("zabbix-server-0")
	p := h.pod("zabbix-server-0")
	p.Finalizers = []string{"test/hold"}
	if err := h.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Delete(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	expectAction(t, h.reconcile(h.set("b", "zabbix-server-0")), "waiting for zabbix-server-0 to terminate")
}

func TestRecreatesLostPod(t *testing.T) {
	h := newHarness(t)
	s := h.set("a", "zabbix-server-0", "zabbix-server-1")
	h.reconcile(s)
	h.run("zabbix-server-0", "zabbix-server-1")
	if err := h.c.Delete(context.Background(), h.pod("zabbix-server-1")); err != nil {
		t.Fatal(err)
	}
	expectAction(t, h.reconcile(s), "creating zabbix-server-1")
	if h.pod("zabbix-server-1") == nil {
		t.Fatal("lost pod not recreated")
	}
}

func TestDeletesFailedPodThenRecreates(t *testing.T) {
	h := newHarness(t)
	s := h.set("a", "zabbix-server-0")
	h.reconcile(s)
	p := h.pod("zabbix-server-0")
	p.Status.Phase, p.Status.Reason = corev1.PodFailed, "Evicted"
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	st := h.reconcile(s)
	expectAction(t, st, "deleting zabbix-server-0 (pod Failed: Evicted)")
	expectAction(t, h.reconcile(s), "creating zabbix-server-0")
}

func TestScaleDownDeletesExtraMembers(t *testing.T) {
	h := newHarness(t)
	h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1", "zabbix-server-2"))
	h.run("zabbix-server-0", "zabbix-server-1", "zabbix-server-2")
	expectAction(t, h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1")), "deleting zabbix-server-2 (no longer desired)")
	if h.pod("zabbix-server-2") != nil || h.pod("zabbix-server-0") == nil {
		t.Fatalf("pods: %v", h.podNames())
	}
}

func TestEmptySetRemovesPodsAndBudget(t *testing.T) {
	h := newHarness(t)
	h.reconcile(h.set("a", "zabbix-web-0"))
	s := h.set("a")
	h.reconcile(s)
	h.reconcile(s)
	if len(h.podNames()) != 0 {
		t.Fatalf("pods left: %v", h.podNames())
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "zabbix-server"}, pdb); !apierrors.IsNotFound(err) {
		t.Fatalf("budget not removed: %v", err)
	}
}

// A pod with a member's name that the owner does not control is reported and never
// touched; the other members are still created.
func TestConflictIsReportedAndUntouched(t *testing.T) {
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "zabbix-server-0", Namespace: ns,
		Labels: map[string]string{"name": "zabbix-server"}}}
	h := newHarness(t, foreign)
	st := h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1"))
	if len(st.Conflicts) != 1 || !strings.Contains(st.Conflicts[0], "pod zabbix-server-0 exists and is not managed") {
		t.Fatalf("conflicts = %v", st.Conflicts)
	}
	if p := h.pod("zabbix-server-0"); p == nil || metav1.GetControllerOf(p) != nil {
		t.Fatal("foreign pod was modified or deleted")
	}
	if h.pod("zabbix-server-1") == nil {
		t.Fatal("non-conflicting member not created")
	}
	h.run("zabbix-server-1")
	if st := h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1")); st.Converged {
		t.Fatal("a set with a conflict must not report converged")
	}
}

func TestHoldStopsCreationReplacementAndScaleDown(t *testing.T) {
	h := newHarness(t)
	h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1"))
	h.run("zabbix-server-0", "zabbix-server-1")
	keep0 := h.pod("zabbix-server-0").ResourceVersion
	keep1 := h.pod("zabbix-server-1").ResourceVersion

	// Without the hold this would delete server-0 (no longer desired), replace server-1
	// (new template) and create server-2.
	s := h.set("b", "zabbix-server-1", "zabbix-server-2")
	s.Hold = "waiting for database"

	st := h.reconcile(s)
	expectAction(t, st, "on hold: waiting for database")
	if p := h.pod("zabbix-server-0"); p == nil || p.ResourceVersion != keep0 {
		t.Error("scale-down happened during hold")
	}
	if p := h.pod("zabbix-server-1"); p == nil || p.ResourceVersion != keep1 {
		t.Error("replacement happened during hold")
	}
	if h.pod("zabbix-server-2") != nil {
		t.Error("creation happened during hold")
	}

	s.Hold = ""
	expectAction(t, h.reconcile(s), "deleting zabbix-server-0 (no longer desired)")
}

func TestLiveLabelsDoNotReplacePods(t *testing.T) {
	h := newHarness(t)
	s := h.set("a", "zabbix-server-0")
	h.reconcile(s)
	h.run("zabbix-server-0")
	uid := h.pod("zabbix-server-0").UID

	s.Members[0].LiveLabels = map[string]string{"zabbix.io/role": "active"}
	if st := h.reconcile(s); !st.Converged {
		t.Fatalf("status = %+v", st)
	}
	p := h.pod("zabbix-server-0")
	if p.UID != uid || p.Labels["zabbix.io/role"] != "active" {
		t.Fatalf("uid changed %v, role %q", p.UID != uid, p.Labels["zabbix.io/role"])
	}
}

func TestDisruptionBudget(t *testing.T) {
	h := newHarness(t)
	h.reconcile(h.set("a", "zabbix-server-0", "zabbix-server-1"))
	pdb := &policyv1.PodDisruptionBudget{}
	if err := h.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "zabbix-server"}, pdb); err != nil {
		t.Fatal(err)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 || pdb.Spec.MinAvailable != nil {
		t.Errorf("budget spec = %+v", pdb.Spec)
	}
	if ref := metav1.GetControllerOf(pdb); ref == nil || ref.UID != h.owner.UID {
		t.Errorf("budget owner = %+v", ref)
	}
	if pdb.Spec.Selector.MatchLabels[LabelComponent] != "server" {
		t.Errorf("budget selector = %v", pdb.Spec.Selector)
	}
}

func TestForeignBudgetIsAConflict(t *testing.T) {
	foreign := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "zabbix-server", Namespace: ns}}
	h := newHarness(t, foreign)
	st := h.reconcile(h.set("a", "zabbix-server-0"))
	if len(st.Conflicts) != 1 || !strings.Contains(st.Conflicts[0], "PodDisruptionBudget zabbix-server") {
		t.Fatalf("conflicts = %v", st.Conflicts)
	}
}

func TestCustomHealth(t *testing.T) {
	h := newHarness(t)
	s := h.set("a", "zabbix-server-0")
	// Standby servers are never Ready; health means running.
	s.Healthy = func(*corev1.Pod) bool { return true }
	h.reconcile(s)
	p := h.pod("zabbix-server-0")
	p.Status.Phase = corev1.PodRunning
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if st := h.reconcile(s); !st.Converged || st.Healthy != 1 {
		t.Fatalf("status = %+v", st)
	}
}

func TestTemplateHash(t *testing.T) {
	a, b := template("p", "x"), template("p", "x")
	a.Labels = map[string]string{"one": "1", "two": "2"}
	b.Labels = map[string]string{"two": "2", "one": "1"}
	if TemplateHash(a) != TemplateHash(b) {
		t.Error("hash depends on map order")
	}
	b.Annotations = map[string]string{AnnotationTemplateHash: "anything"}
	if TemplateHash(a) != TemplateHash(b) {
		t.Error("hash depends on its own annotation")
	}
	if TemplateHash(a) == TemplateHash(template("p", "y")) {
		t.Error("hash ignores an env change")
	}
	c := template("p", "x")
	c.Labels = a.Labels
	c.Annotations = map[string]string{"zabbix.io/config-hash": "1"}
	if TemplateHash(a) == TemplateHash(c) {
		t.Error("hash ignores annotations")
	}
}

// A removed pod that is still shutting down keeps the set from reporting that it is
// empty, so a caller does not start what must wait for it.
func TestTerminatingUnwantedPodIsReported(t *testing.T) {
	h := newHarness(t)
	h.reconcile(h.set("a", "zabbix-server-init-0"))
	p := h.pod("zabbix-server-init-0")
	p.Finalizers = []string{"test/hold"}
	if err := h.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	expectAction(t, h.reconcile(h.set("a")), "deleting zabbix-server-init-0")
	st := h.reconcile(h.set("a"))
	expectAction(t, st, "waiting for zabbix-server-init-0 to terminate")
	if st.Converged {
		t.Fatal("a set with a terminating pod must not report converged")
	}
	p = h.pod("zabbix-server-init-0")
	p.Finalizers = nil
	if err := h.c.Update(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if st := h.reconcile(h.set("a")); !st.Converged || st.Action != "" {
		t.Fatalf("status %+v after the pod is gone", st)
	}
}
