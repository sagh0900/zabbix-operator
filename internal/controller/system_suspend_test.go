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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/jobs"
	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

const holdFinalizer = "test/shutting-down"

// holdPod keeps a pod terminating after deletion, as a slow clean shutdown would.
func holdPod(t *testing.T, ns, name string, hold bool) {
	t.Helper()
	eventually(t, func() error {
		p := getPod(t, ns, name)
		if p == nil {
			return fmt.Errorf("pod %s not found", name)
		}
		p.Finalizers = nil
		if hold {
			p.Finalizers = []string{holdFinalizer}
		}
		return k8s.Update(context.Background(), p)
	})
}

// terminating waits until a pod is being deleted.
func terminating(t *testing.T, ns, name string) {
	t.Helper()
	eventually(t, func() error {
		if p := getPod(t, ns, name); p == nil || p.DeletionTimestamp == nil {
			return fmt.Errorf("pod %s is not terminating", name)
		}
		return nil
	})
}

func present(t *testing.T, ns string, names ...string) {
	t.Helper()
	for _, n := range names {
		if p := getPod(t, ns, n); p == nil || p.DeletionTimestamp != nil {
			t.Fatalf("pod %s was stopped too early", n)
		}
	}
}

// Suspend stops the frontend and web service, then proxies and agents, then the standby
// server, then the active one, each stage only after the previous one has terminated;
// nothing is recreated while suspended, and resume brings everything back.
func TestSystem_Suspend(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "dc1"}}
		s.Spec.Agent = zabbixv1alpha1.AgentSpec{Enabled: true, PodSettings: zabbixv1alpha1.PodSettings{Image: "zabbix/zabbix-agent2:ubuntu-7.0.25"}}
	})
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-agent"}, &appsv1.DaemonSet{})
	})
	for _, n := range []string{"zabbix-web-0", "dc1-0", "zabbix-server-1"} {
		holdPod(t, ns, n, true)
	}

	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Suspend = true })
	terminating(t, ns, "zabbix-web-0")
	waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspending: stopping the frontend and web service")
	time.Sleep(500 * time.Millisecond)
	present(t, ns, "dc1-0", "zabbix-server-0", "zabbix-server-1")

	holdPod(t, ns, "zabbix-web-0", false)
	terminating(t, ns, "dc1-0")
	waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspending: stopping the proxies and agents")
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-agent"}, &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("agent DaemonSet still present: %v", err)
		}
		return nil
	})
	present(t, ns, "zabbix-server-0", "zabbix-server-1")

	holdPod(t, ns, "dc1-0", false)
	terminating(t, ns, "zabbix-server-1")
	waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspending: stopping the")
	time.Sleep(500 * time.Millisecond)
	present(t, ns, "zabbix-server-0") // the active node stops last

	holdPod(t, ns, "zabbix-server-1", false)
	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspended: all Zabbix pods are stopped")
	waitEvent(t, ns, corev1.EventTypeNormal, "Suspended", "All Zabbix pods are stopped")
	if c := meta.FindStatusCondition(sys.Status.Conditions, zabbixv1alpha1.SystemServerActive); c == nil || c.Reason != "Suspended" {
		t.Errorf("ServerActive = %+v", c)
	}
	if sys.Status.ActiveServer != nil || len(sys.Status.Components) != 0 || sys.Status.RunningVersion != "7.0.25" {
		t.Errorf("status %+v", sys.Status)
	}
	pods := &corev1.PodList{}
	if err := k8s.List(ctx, pods, client.InNamespace(ns), client.MatchingLabels{"zabbix.io/system": "zabbix"}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Errorf("%d pods left", len(pods.Items))
	}
	// Services stay; disruption budgets go with their pods.
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-server"}, &corev1.Service{}); err != nil {
		t.Errorf("server Service: %v", err)
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-server"}, &policyv1.PodDisruptionBudget{}); !apierrors.IsNotFound(err) {
		t.Errorf("server PodDisruptionBudget: %v", err)
	}

	// Nothing comes back while suspended, not even after a configuration change.
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Server.Replicas = 3 })
	time.Sleep(time.Second)
	if p := getPod(t, ns, "zabbix-server-0"); p != nil {
		t.Fatal("a server pod was created while suspended")
	}
	waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspended")

	// Resume.
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Suspend = false; s.Server.Replicas = 2 })
	for _, n := range []string{"zabbix-server-0", "zabbix-server-1", "zabbix-web-0", "zabbix-webservice-0", "dc1-0"} {
		waitPod(t, ns, n, true)
	}
	runAll(t, ns)
	waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "zabbix-server-0 active, 1 standby")
	waitEvent(t, ns, corev1.EventTypeNormal, "Resuming", "Starting all Zabbix pods")
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-agent"}, &appsv1.DaemonSet{})
	})
}

// A suspend requested while the schema is being created waits for it; afterwards the
// system suspends without starting anything else.
func TestSystem_SuspendWaitsForSchemaCreation(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "7.0.25", nil)
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.FreshInstall)})
	waitPod(t, ns, "zabbix-server-init-0", true)

	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Suspend = true })
	waitPhase(t, ns, zabbixv1alpha1.PhaseInstalling, "suspend waits for the schema creation")
	present(t, ns, "zabbix-server-init-0")

	setPod(t, ns, "zabbix-server-init-0")
	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspended: all Zabbix pods are stopped")
	if sys.Status.RunningVersion != "7.0.25" {
		t.Errorf("runningVersion %q", sys.Status.RunningVersion)
	}
	for _, n := range []string{"zabbix-server-init-0", "zabbix-server-0", "zabbix-web-0"} {
		if getPod(t, ns, n) != nil {
			t.Errorf("pod %s exists while suspended", n)
		}
	}
}

// A system suspended before it ever ran creates nothing.
func TestSystem_SuspendedFromCreation(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) { s.Spec.Suspend = true })
	waitPhase(t, ns, zabbixv1alpha1.PhaseSuspended, "Suspended")
	time.Sleep(time.Second)
	if pods := (&corev1.PodList{}); k8s.List(context.Background(), pods, client.InNamespace(ns)) == nil && len(pods.Items) != 0 {
		names := make([]string, 0, len(pods.Items))
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		t.Errorf("pods created while suspended: %s", strings.Join(names, ", "))
	}
	if c := meta.FindStatusCondition(getSystem(t, ns).Status.Conditions, zabbixv1alpha1.SystemServerActive); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ServerActive = %+v", c)
	}
}
