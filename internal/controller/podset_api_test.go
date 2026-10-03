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
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/podset"
)

// TestPodsetAgainstAPIServer checks that the objects the podset engine writes are
// accepted by a real API server with a ZabbixSystem as owner.
func TestPodsetAgainstAPIServer(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := zabbixv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sys := newSystem(newNamespace(t), "7.0.25")
	if err := k8s.Create(ctx, sys); err != nil {
		t.Fatal(err)
	}
	m := &podset.Manager{Client: k8s, Scheme: scheme}
	set := func(image string) podset.Set {
		s := podset.Set{Owner: sys, System: sys.Name, Component: "web", DisruptionBudget: true}
		for _, n := range []string{"zabbix-web-0", "zabbix-web-1"} {
			s.Members = append(s.Members, podset.Member{Template: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: n},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: image}}},
			}})
		}
		return s
	}

	if _, err := m.Reconcile(ctx, set("zabbix/zabbix-web-nginx-pgsql:ubuntu-7.0.25")); err != nil {
		t.Fatal(err)
	}
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(sys.Namespace), client.MatchingLabels(podset.SelectorLabels(sys.Name, "web"))); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("%d pods, want 2", len(pods.Items))
	}
	ref := metav1.GetControllerOf(&pods.Items[0])
	if ref == nil || ref.Kind != "ZabbixSystem" || ref.UID != sys.UID {
		t.Fatalf("controller reference = %+v", ref)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: sys.Namespace, Name: "zabbix-web"}, pdb); err != nil {
		t.Fatal(err)
	}

	// A template change deletes exactly one pod; unscheduled pods go away immediately.
	for i := range pods.Items {
		p := &pods.Items[i]
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		if err := k8s.Status().Update(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	st, err := m.Reconcile(ctx, set("zabbix/zabbix-web-nginx-pgsql:ubuntu-7.0.30"))
	if err != nil {
		t.Fatal(err)
	}
	if err := k8s.List(ctx, pods, client.InNamespace(sys.Namespace), client.MatchingLabels(podset.SelectorLabels(sys.Name, "web"))); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("%d pods after one replacement step (%s), want 1", len(pods.Items), st.Action)
	}
}
