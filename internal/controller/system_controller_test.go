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
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/jobs"
	"github.com/sagh0900/zabbix-operator/internal/metrics"
	"github.com/sagh0900/zabbix-operator/internal/podset"
	"github.com/sagh0900/zabbix-operator/internal/system"
	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

// readyDatabase creates a CNPG cluster, credentials and a ZabbixDatabase that is Ready.
func readyDatabase(t *testing.T, ns string) {
	t.Helper()
	createSecret(t, ns)
	createCluster(t, ns)
	createDatabase(t, ns)
	waitReady(t, ns, metav1.ConditionTrue, ReasonReady)
}

func createSystem(t *testing.T, ns, version string, mutate func(*zabbixv1alpha1.ZabbixSystem)) {
	t.Helper()
	sys := newSystem(ns, version)
	if mutate != nil {
		mutate(sys)
	}
	if err := k8s.Create(context.Background(), sys); err != nil {
		t.Fatal(err)
	}
}

func getSystem(t *testing.T, ns string) *zabbixv1alpha1.ZabbixSystem {
	t.Helper()
	sys := &zabbixv1alpha1.ZabbixSystem{}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "zabbix"}, sys); err != nil {
		t.Fatal(err)
	}
	return sys
}

// waitPhase waits until the system has phase and a reason containing reason.
func waitPhase(t *testing.T, ns string, phase zabbixv1alpha1.SystemPhase, reason string) *zabbixv1alpha1.ZabbixSystem {
	t.Helper()
	var sys *zabbixv1alpha1.ZabbixSystem
	eventually(t, func() error {
		sys = getSystem(t, ns)
		if sys.Status.Phase != phase || !strings.Contains(sys.Status.PhaseReason, reason) {
			return fmt.Errorf("phase %s %q, want %s %q", sys.Status.Phase, sys.Status.PhaseReason, phase, reason)
		}
		return nil
	})
	return sys
}

// finishJob plays the Job controller and kubelet: it completes the operator's pending Job
// for result.Command with result.
func finishJob(t *testing.T, ns string, result jobs.Result) {
	command := result.Command
	t.Helper()
	ctx := context.Background()
	job := &batchv1.Job{}
	eventually(t, func() error {
		list := &batchv1.JobList{}
		if err := k8s.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{jobs.LabelJob: command}); err != nil {
			return err
		}
		for i := range list.Items {
			if len(list.Items[i].Status.Conditions) == 0 {
				*job = list.Items[i]
				return nil
			}
		}
		return fmt.Errorf("no pending %s Job", command)
	})
	if job.Spec.Template.Spec.Containers[0].Image != testOperatorImage {
		t.Errorf("job image %q", job.Spec.Template.Spec.Containers[0].Image)
	}
	msg, _ := json.Marshal(result)
	// The pod is owned by the Job, as the Job controller would create it.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-" + string(job.UID)[:5], Namespace: ns, Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
				Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "job", Image: testOperatorImage}}},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "job", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Message: string(msg), FinishedAt: now}}}}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
	}
	if err := k8s.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
}

// setPod plays the kubelet for a pod: running and Ready (the process is healthy).
func setPod(t *testing.T, ns, name string) {
	t.Helper()
	ctx := context.Background()
	eventually(t, func() error {
		p := &corev1.Pod{}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
			return err
		}
		status := corev1.ConditionTrue
		p.Status.Phase = corev1.PodRunning
		p.Status.PodIP = "10.0.0.1"
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
		p.Status.ContainerStatuses = nil
		for _, c := range p.Spec.Containers {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
				Name: c.Name, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}})
		}
		return k8s.Status().Update(ctx, p)
	})
}

func getPod(t *testing.T, ns, name string) *corev1.Pod {
	t.Helper()
	p := &corev1.Pod{}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return p
}

func waitPod(t *testing.T, ns, name string, present bool) *corev1.Pod {
	t.Helper()
	var p *corev1.Pod
	eventually(t, func() error {
		p = getPod(t, ns, name)
		if (p != nil) != present {
			return fmt.Errorf("pod %s present=%v, want %v", name, p != nil, present)
		}
		return nil
	})
	return p
}

// waitReplaced waits until pod name exists with a UID other than old and returns it.
func waitReplaced(t *testing.T, ns, name string, old types.UID) *corev1.Pod {
	t.Helper()
	var p *corev1.Pod
	eventually(t, func() error {
		p = getPod(t, ns, name)
		if p == nil || p.UID == old {
			return fmt.Errorf("pod %s not replaced yet", name)
		}
		return nil
	})
	return p
}

func uid(t *testing.T, ns, name string) types.UID {
	t.Helper()
	p := getPod(t, ns, name)
	if p == nil {
		t.Fatalf("pod %s missing", name)
	}
	return p.UID
}

func envValue(p *corev1.Pod, container, name string) (string, bool) {
	for _, c := range p.Spec.Containers {
		if c.Name != container {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value, true
			}
		}
	}
	return "", false
}

func image(p *corev1.Pod) string { return p.Spec.Containers[0].Image }

// setActive makes name the active server node of ns, as Zabbix HA would.
func setActive(ns, name string) {
	for _, n := range []string{"zabbix-server-0", "zabbix-server-1", "zabbix-server-2"} {
		fakeActive.Store(ns+"/"+n, n == name)
	}
}

// runAll marks every pod running and Ready (process healthy), with server-0 active.
func runAll(t *testing.T, ns string) {
	t.Helper()
	names := []string{"zabbix-server-0", "zabbix-server-1", "zabbix-web-0", "zabbix-webservice-0"}
	pods := &corev1.PodList{}
	if err := k8s.List(context.Background(), pods, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		if strings.HasPrefix(p.Labels[podset.LabelComponent], system.ProxyComponentPrefix) {
			names = append(names, p.Name)
		}
	}
	for _, n := range names {
		setPod(t, ns, n)
	}
	setActive(ns, "zabbix-server-0")
}

func installed(t *testing.T, ns, version string, mutate func(*zabbixv1alpha1.ZabbixSystem)) {
	t.Helper()
	readyDatabase(t, ns)
	createSystem(t, ns, version, mutate)
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SameSchema)})
	waitPod(t, ns, "zabbix-server-1", true)
	waitPod(t, ns, "zabbix-web-0", true)
	waitPod(t, ns, "zabbix-webservice-0", true)
	// Every proxy pod the spec implies must exist before the kubelet is played for it.
	sys := getSystem(t, ns)
	for i := range sys.Spec.Proxies {
		p := &sys.Spec.Proxies[i]
		for j := 0; p.IsEnabled() && j < int(max(p.Replicas, 1)); j++ {
			waitPod(t, ns, system.ProxyPodName(p, j), true)
		}
	}
	runAll(t, ns)
	waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "zabbix-server-0 active, 1 standby")
}

func TestSystem_FreshInstall(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "7.0.1", nil)
	waitPhase(t, ns, zabbixv1alpha1.PhaseInstalling, "Checking the database")

	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.FreshInstall)})
	initPod := waitPod(t, ns, "zabbix-server-init-0", true)
	if _, ha := envValue(initPod, "zabbix-server", "ZBX_HANODENAME"); ha {
		t.Error("the schema-creation server must run standalone (no ZBX_HANODENAME)")
	}
	if image(initPod) != "zabbix/zabbix-server-pgsql:ubuntu-7.0.1" {
		t.Errorf("image %q", image(initPod))
	}
	waitPhase(t, ns, zabbixv1alpha1.PhaseInstalling, "Creating the database schema on zabbix-server-init-0")
	if getPod(t, ns, "zabbix-server-0") != nil || getPod(t, ns, "zabbix-web-0") != nil {
		t.Fatal("HA servers or frontend started before the schema exists")
	}

	// Keep the standalone server terminating: HA servers must not start until it is gone,
	// or Zabbix refuses to start in HA mode while the standalone node is active.
	ctx := context.Background()
	initPod = getPod(t, ns, "zabbix-server-init-0")
	initPod.Finalizers = []string{"test/shutting-down"}
	if err := k8s.Update(ctx, initPod); err != nil {
		t.Fatal(err)
	}
	setPod(t, ns, "zabbix-server-init-0") // the standalone server listens: schema created
	waitPhase(t, ns, zabbixv1alpha1.PhaseDegraded, "Waiting for the schema-creation server to stop")
	time.Sleep(time.Second)
	if getPod(t, ns, "zabbix-server-0") != nil {
		t.Fatal("HA servers started while the standalone server was still shutting down")
	}
	initPod = getPod(t, ns, "zabbix-server-init-0")
	initPod.Finalizers = nil
	if err := k8s.Update(ctx, initPod); err != nil {
		t.Fatal(err)
	}
	waitPod(t, ns, "zabbix-server-init-0", false)
	server := waitPod(t, ns, "zabbix-server-0", true)
	if v, _ := envValue(server, "zabbix-server", "DB_SERVER_HOST"); v != "pg-rw" {
		t.Errorf("DB_SERVER_HOST %q", v)
	}
	waitPod(t, ns, "zabbix-server-1", true)
	waitPod(t, ns, "zabbix-web-0", true)
	waitPod(t, ns, "zabbix-webservice-0", true)
	runAll(t, ns)

	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "zabbix-server-0 active, 1 standby")
	if !getPod(t, ns, "zabbix-server-1").Status.ContainerStatuses[0].Ready {
		t.Error("a standby server must be Ready (1/1)")
	}
	if sys.Status.RunningVersion != "7.0.1" || sys.Status.ActiveServer == nil || sys.Status.ActiveServer.Pod != "zabbix-server-0" {
		t.Errorf("status %+v", sys.Status)
	}
	for _, c := range []string{zabbixv1alpha1.SystemServerActive, zabbixv1alpha1.SystemWebReady, zabbixv1alpha1.SystemDatabaseReady} {
		if cond := meta.FindStatusCondition(sys.Status.Conditions, c); cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("%s = %+v", c, cond)
		}
	}
	eventually(t, func() error {
		if r := getPod(t, ns, "zabbix-server-0").Labels[system.LabelRole]; r != "active" {
			return fmt.Errorf("server-0 role %q", r)
		}
		if r := getPod(t, ns, "zabbix-server-1").Labels[system.LabelRole]; r != "standby" {
			return fmt.Errorf("server-1 role %q", r)
		}
		return nil
	})
}

func TestSystem_ServerAndWebPods(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) { s.Spec.Timezone = "Europe/Stockholm" })

	server := getPod(t, ns, "zabbix-server-0")
	for name, want := range map[string]string{"DB_SERVER_PORT": "5432", "POSTGRES_DB": "zabbix", "TZ": "Europe/Stockholm",
		"ZBX_WEBSERVICEURL": "http://zabbix-webservice:10053/report"} {
		if v, _ := envValue(server, "zabbix-server", name); v != want {
			t.Errorf("server %s = %q, want %q", name, v, want)
		}
	}
	for _, c := range server.Spec.Containers[0].Env {
		if c.Name == "POSTGRES_PASSWORD" && (c.ValueFrom == nil || c.ValueFrom.SecretKeyRef == nil) {
			t.Error("the password must come from the credentials Secret")
		}
		if c.Name == "ZBX_HANODENAME" && (c.ValueFrom == nil || c.ValueFrom.FieldRef.FieldPath != "metadata.name") {
			t.Error("the HA node name must be the pod name")
		}
	}
	if p := server.Spec.Containers[0].ReadinessProbe; p == nil || p.Exec == nil || strings.Join(p.Exec.Command, " ") != "zabbix_server -R ha_status" {
		t.Errorf("server readiness %+v", p)
	}
	if server.Spec.Containers[0].LivenessProbe != nil {
		t.Error("server must have no liveness probe on 10051: standby nodes do not listen there")
	}
	if ref := metav1.GetControllerOf(server); ref == nil || ref.Kind != "ZabbixSystem" {
		t.Errorf("server controller reference %+v", ref)
	}
	web := getPod(t, ns, "zabbix-web-0")
	if v, _ := envValue(web, "zabbix-web", "ZBX_SERVER_HOST"); v != "zabbix-server" {
		t.Errorf("frontend ZBX_SERVER_HOST %q, want the active-only server Service", v)
	}
	if v, _ := envValue(web, "zabbix-web", "PHP_TZ"); v != "Europe/Stockholm" {
		t.Errorf("PHP_TZ %q", v)
	}

	svc := &corev1.Service{}
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "zabbix-server"}, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Selector[podset.LabelComponent] != "server" || svc.Spec.Selector[system.LabelRole] != system.RoleActive || svc.Spec.Ports[0].Port != 10051 {
		t.Errorf("server Service %+v", svc.Spec)
	}
	for _, name := range []string{"zabbix-web", "zabbix-webservice"} {
		if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &corev1.Service{}); err != nil {
			t.Errorf("Service %s: %v", name, err)
		}
	}
	for _, name := range []string{"zabbix-server", "zabbix-web", "zabbix-webservice"} {
		if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &policyv1.PodDisruptionBudget{}); err != nil {
			t.Errorf("PodDisruptionBudget %s: %v", name, err)
		}
	}
}

func TestSystem_UnsupportedLineIsBlocked(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "9.0.0", nil)
	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "Zabbix 9.0 is not supported by this operator version (supported lines: 7.0, 7.2, 7.4, 8.0)")
	if c := meta.FindStatusCondition(sys.Status.Conditions, zabbixv1alpha1.SystemUpgradeBlocked); c == nil || c.Reason != "UnsupportedVersion" {
		t.Errorf("UpgradeBlocked %+v", c)
	}
	list := &corev1.PodList{}
	if err := k8s.List(context.Background(), list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d pods created for an unsupported version", len(list.Items))
	}
}

func TestSystem_PrecheckFailureBlocksInstall(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "8.0.0rc1", nil)
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, Reason: "PostgreSQLTooOld",
		Message: "PostgreSQL 14 is too old for Zabbix 8.0 (needs 15 or newer)"})
	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "PostgreSQL 14 is too old for Zabbix 8.0 (needs 15 or newer)")
	if c := meta.FindStatusCondition(sys.Status.Conditions, zabbixv1alpha1.SystemUpgradeBlocked); c == nil || c.Reason != "PostgreSQLTooOld" {
		t.Errorf("UpgradeBlocked %+v", c)
	}
	if getPod(t, ns, "zabbix-server-init-0") != nil {
		t.Error("nothing may start while blocked")
	}
}

// While CNPG switches over (or upgrades), the database is not ready: running pods are left
// alone, nothing is replaced, and the reason names what the database is waiting for.
func TestSystem_DatabaseSwitchoverHoldsWithoutStopping(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	tuning := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tuning", Namespace: ns}, StringData: map[string]string{"ZBX_STARTPOLLERS": "15"}}
	if err := k8s.Create(ctx, tuning); err != nil {
		t.Fatal(err)
	}
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Server.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"}}}}
	})
	s0, s1 := uid(t, ns, "zabbix-server-0"), uid(t, ns, "zabbix-server-1")

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pg"}, cluster); err != nil {
		t.Fatal(err)
	}
	setClusterStatus(t, cluster, primary1, primary2)
	waitPhase(t, ns, zabbixv1alpha1.PhaseDegraded, "Waiting for database: primary is moving from pg-1 to pg-2")

	// A configuration change during the switchover waits for the database.
	tuning.StringData = map[string]string{"ZBX_STARTPOLLERS": "40"}
	if err := k8s.Update(ctx, tuning); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if uid(t, ns, "zabbix-server-0") != s0 || uid(t, ns, "zabbix-server-1") != s1 {
		t.Fatal("a server was stopped or replaced while the database was switching over")
	}

	setClusterStatus(t, cluster, primary2, primary2)
	waitReplaced(t, ns, "zabbix-server-1", s1) // the held change now rolls, standby first
	if uid(t, ns, "zabbix-server-0") != s0 {
		t.Fatal("the active server was replaced before the standby")
	}
}

// A change to a Secret referenced through envFrom rolls the servers one at a time,
// standby first.
func TestSystem_TuningSecretRollsServersStandbyFirst(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	tuning := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tuning", Namespace: ns}, StringData: map[string]string{"ZBX_STARTPOLLERS": "15"}}
	if err := k8s.Create(ctx, tuning); err != nil {
		t.Fatal(err)
	}
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Server.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"}}}}
	})
	s0, s1 := uid(t, ns, "zabbix-server-0"), uid(t, ns, "zabbix-server-1")

	tuning.StringData = map[string]string{"ZBX_STARTPOLLERS": "40"}
	if err := k8s.Update(ctx, tuning); err != nil {
		t.Fatal(err)
	}
	waitReplaced(t, ns, "zabbix-server-1", s1) // the standby goes first
	if uid(t, ns, "zabbix-server-0") != s0 {
		t.Fatal("the active server was replaced before the standby")
	}
	time.Sleep(time.Second)
	if uid(t, ns, "zabbix-server-0") != s0 {
		t.Fatal("the active server was replaced while the new standby was not running")
	}
	setPod(t, ns, "zabbix-server-1")
	waitReplaced(t, ns, "zabbix-server-0", s0) // then the active one
	setActive(ns, "zabbix-server-1")           // failover
	setPod(t, ns, "zabbix-server-0")
	waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "zabbix-server-1 active, 1 standby")
}

// A patch release rolls servers first, standby before active, then the frontend.
func TestSystem_PatchUpgrade(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	installed(t, ns, "7.0.1", nil)
	s0old, s1old := uid(t, ns, "zabbix-server-0"), uid(t, ns, "zabbix-server-1")

	sys := getSystem(t, ns)
	before := sys.DeepCopy()
	sys.Spec.Version = "7.0.25"
	if err := k8s.Patch(ctx, sys, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "Checking the database for Zabbix 7.0.25")
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SameSchema)})

	s1 := waitReplaced(t, ns, "zabbix-server-1", s1old)
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "Upgrading from 7.0.1 to 7.0.25")
	if image(getPod(t, ns, "zabbix-web-0")) != "zabbix/zabbix-web-nginx-pgsql:ubuntu-7.0.1" {
		t.Error("the frontend moved before the servers")
	}
	if image(s1) != "zabbix/zabbix-server-pgsql:ubuntu-7.0.25" {
		t.Errorf("replacement image %q", image(s1))
	}
	setPod(t, ns, "zabbix-server-1")
	waitReplaced(t, ns, "zabbix-server-0", s0old)
	setActive(ns, "zabbix-server-1")
	setPod(t, ns, "zabbix-server-0")

	eventually(t, func() error {
		if v := getSystem(t, ns).Status.RunningVersion; v != "7.0.25" {
			return fmt.Errorf("runningVersion %s", v)
		}
		return nil
	})
	eventually(t, func() error {
		p := getPod(t, ns, "zabbix-web-0")
		if p == nil || image(p) != "zabbix/zabbix-web-nginx-pgsql:ubuntu-7.0.25" {
			return fmt.Errorf("frontend not on 7.0.25 yet")
		}
		return nil
	})
	// The upgrade is still reported while the frontend rolls.
	if sys := getSystem(t, ns); sys.Status.Phase != zabbixv1alpha1.PhaseUpgrading {
		t.Errorf("phase %s %q while the frontend rolls, want Upgrading", sys.Status.Phase, sys.Status.PhaseReason)
	}
	setPod(t, ns, "zabbix-web-0")
	eventually(t, func() error {
		p := getPod(t, ns, "zabbix-webservice-0")
		if p == nil || image(p) != "zabbix/zabbix-web-service:ubuntu-7.0.25" {
			return fmt.Errorf("web service not on 7.0.25 yet")
		}
		return nil
	})
	setPod(t, ns, "zabbix-webservice-0")
	waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "zabbix-server-1 active, 1 standby")
}

// A Service with a needed name that the system does not own is reported and untouched.
func TestSystem_ServiceConflict(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "zabbix-server", Namespace: ns},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"name": "zabbix-server"}, Ports: []corev1.ServicePort{{Port: 10051}}}}
	if err := k8s.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	readyDatabase(t, ns)
	createSystem(t, ns, "7.0.25", nil)
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SameSchema)})
	waitPod(t, ns, "zabbix-server-1", true)
	runAll(t, ns)
	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseDegraded, "Conflict: Service zabbix-server exists and is not managed by this ZabbixSystem")
	if c := meta.FindStatusCondition(sys.Status.Conditions, zabbixv1alpha1.SystemConflict); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Conflict %+v", c)
	}
	svc := &corev1.Service{}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(foreign), svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Selector["name"] != "zabbix-server" || metav1.GetControllerOf(svc) != nil {
		t.Error("the foreign Service was modified")
	}
}

func TestSystem_ServiceSettingsAndIngress(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Server.Service = zabbixv1alpha1.ServiceSettings{Type: corev1.ServiceTypeLoadBalancer, LoadBalancerIP: "10.0.0.10",
			Annotations: map[string]string{"metallb.universe.tf/allow-shared-ip": "zabbix"}}
		s.Spec.Web.Ingress = zabbixv1alpha1.IngressSettings{Enabled: true, ClassName: ptr.To("traefik"),
			Hosts: []zabbixv1alpha1.IngressHost{{Host: "zabbix.example.com"}}}
	})
	svc := &corev1.Service{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-server"}, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer || svc.Spec.LoadBalancerIP != "10.0.0.10" || //nolint:staticcheck // the field under test
		svc.Annotations["metallb.universe.tf/allow-shared-ip"] != "zabbix" {
		t.Errorf("server Service %+v %v", svc.Spec, svc.Annotations)
	}
	ing := &networkingv1.Ingress{}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-web"}, ing); err != nil {
		t.Fatal(err)
	}
	if *ing.Spec.IngressClassName != "traefik" || ing.Spec.Rules[0].Host != "zabbix.example.com" ||
		ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name != "zabbix-web" {
		t.Errorf("Ingress %+v", ing.Spec)
	}

	// Removing the annotation and the Ingress from the spec removes them from the cluster.
	sys := getSystem(t, ns)
	before := sys.DeepCopy()
	sys.Spec.Server.Service.Annotations = nil
	sys.Spec.Web.Ingress.Enabled = false
	if err := k8s.Patch(ctx, sys, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-server"}, svc); err != nil {
			return err
		}
		if _, ok := svc.Annotations["metallb.universe.tf/allow-shared-ip"]; ok {
			return fmt.Errorf("annotation still present")
		}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-web"}, &networkingv1.Ingress{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("Ingress still present: %v", err)
		}
		return nil
	})
}

func TestSystem_DisablingWebRemovesIt(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	installed(t, ns, "7.0.25", nil)
	sys := getSystem(t, ns)
	before := sys.DeepCopy()
	sys.Spec.Web.Enabled = ptr.To(false)
	if err := k8s.Patch(ctx, sys, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	waitPod(t, ns, "zabbix-web-0", false)
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-web"}, &corev1.Service{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("web Service still present: %v", err)
		}
		c := meta.FindStatusCondition(getSystem(t, ns).Status.Conditions, zabbixv1alpha1.SystemWebReady)
		if c == nil || c.Reason != "Disabled" {
			return fmt.Errorf("WebReady %+v", c)
		}
		return nil
	})
}

// A Zabbix failover moves the role label, and with it the Service, without restarting pods.
func TestSystem_ServiceFollowsFailover(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	s0, s1 := uid(t, ns, "zabbix-server-0"), uid(t, ns, "zabbix-server-1")

	setActive(ns, "zabbix-server-1")
	start := time.Now()
	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "zabbix-server-1 active, 1 standby")
	if d := time.Since(start); d > 2*activeCheckInterval+time.Second {
		t.Errorf("the role moved after %s", d)
	}
	if sys.Status.ActiveServer.Pod != "zabbix-server-1" {
		t.Errorf("activeServer %+v", sys.Status.ActiveServer)
	}
	eventually(t, func() error {
		if r := getPod(t, ns, "zabbix-server-1").Labels[system.LabelRole]; r != system.RoleActive {
			return fmt.Errorf("server-1 role %q", r)
		}
		if r := getPod(t, ns, "zabbix-server-0").Labels[system.LabelRole]; r != system.RoleStandby {
			return fmt.Errorf("server-0 role %q", r)
		}
		return nil
	})
	if uid(t, ns, "zabbix-server-0") != s0 || uid(t, ns, "zabbix-server-1") != s1 {
		t.Fatal("a failover must not restart pods")
	}

	// During the Zabbix failover window no node is active: the Service has no endpoint and
	// the system says so.
	setActive(ns, "")
	waitPhase(t, ns, zabbixv1alpha1.PhaseDegraded, "No active server node")
	waitEvent(t, ns, corev1.EventTypeWarning, "ActiveServerLost", "zabbix-server-1 is no longer active")
	setActive(ns, "zabbix-server-0")
	waitEvent(t, ns, corev1.EventTypeNormal, "ActiveServer", "zabbix-server-0 is the active server node")
}

// waitEvent waits for an event about the system with the given type, reason and note.
func waitEvent(t *testing.T, ns, kind, reason, note string) {
	t.Helper()
	eventually(t, func() error {
		list := &eventsv1.EventList{}
		if err := k8s.List(context.Background(), list, client.InNamespace(ns)); err != nil {
			return err
		}
		for _, e := range list.Items {
			if e.Regarding.Kind == "ZabbixSystem" && e.Type == kind && e.Reason == reason && strings.Contains(e.Note, note) {
				return nil
			}
		}
		return fmt.Errorf("no %s event %s %q", kind, reason, note)
	})
}

// While the system runs, ha-gc keeps the live server pods, records when it last succeeded
// and does not run again before the interval.
func TestSystem_HANodeGC(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)

	job := pendingJob(t, ns, jobs.CommandHAGC)
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--keep=zabbix-server-0,zabbix-server-1") || !strings.Contains(args, "--stale-seconds=120") {
		t.Errorf("ha-gc args %q", args)
	}
	finishJob(t, ns, jobs.Result{Command: jobs.CommandHAGC, OK: true, Deleted: 3, Message: "deleted 3 stale ha_node rows"})
	eventually(t, func() error {
		if getSystem(t, ns).Status.LastHANodeGCTime == nil {
			return fmt.Errorf("lastHANodeGCTime not recorded")
		}
		return nil
	})
	waitEvent(t, ns, corev1.EventTypeNormal, "HANodesRemoved", "deleted 3 stale ha_node rows")
	if got := testutil.ToFloat64(metrics.HANodeGCRowsDeleted.WithLabelValues(ns, "zabbix")); got != 3 {
		t.Errorf("rows deleted metric %v", got)
	}
	if got := testutil.ToFloat64(metrics.JobRuns.WithLabelValues(ns, "zabbix", jobs.CommandHAGC, "succeeded")); got != 1 {
		t.Errorf("job runs metric %v", got)
	}
	// A successful run is removed once its result is read, and not run again within the
	// interval.
	eventually(t, func() error {
		list := &batchv1.JobList{}
		if err := k8s.List(context.Background(), list, client.InNamespace(ns), client.MatchingLabels{jobs.LabelJob: jobs.CommandHAGC}); err != nil {
			return err
		}
		if len(list.Items) != 0 {
			j := list.Items[0]
			return fmt.Errorf("%d ha-gc Jobs left, want 0 (uid %s deleting=%v finalizers=%v conditions=%d)", len(list.Items), j.UID, j.DeletionTimestamp, j.Finalizers, len(j.Status.Conditions))
		}
		return nil
	})
	time.Sleep(3 * activeCheckInterval)
	list := &batchv1.JobList{}
	if err := k8s.List(context.Background(), list, client.InNamespace(ns), client.MatchingLabels{jobs.LabelJob: jobs.CommandHAGC}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Errorf("%d ha-gc Jobs within the interval, want none", len(list.Items))
	}
}

// A failed ha-gc run stays for inspection, is reported once, and is not retried within
// the interval.
func TestSystem_HANodeGCFailureIsKept(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	finishJob(t, ns, jobs.Result{Command: jobs.CommandHAGC, Reason: "DatabaseError", Message: "connection refused"})
	waitEvent(t, ns, corev1.EventTypeWarning, "HANodeGCFailed", "connection refused")
	time.Sleep(3 * activeCheckInterval)
	list := &batchv1.JobList{}
	if err := k8s.List(context.Background(), list, client.InNamespace(ns), client.MatchingLabels{jobs.LabelJob: jobs.CommandHAGC}); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || len(list.Items[0].Status.Conditions) == 0 {
		t.Errorf("want the finished, failed ha-gc Job kept and no new run; got %d Jobs", len(list.Items))
	}
}

func TestSystem_Metrics(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	eventually(t, func() error {
		checks := map[string]float64{
			"phase Running": testutil.ToFloat64(metrics.SystemPhase.WithLabelValues(ns, "zabbix", "Running")),
			"active nodes":  testutil.ToFloat64(metrics.ServerActiveNodes.WithLabelValues(ns, "zabbix")),
			"server ready":  testutil.ToFloat64(metrics.ComponentPodsReady.WithLabelValues(ns, "zabbix", "server")),
			"web desired":   testutil.ToFloat64(metrics.ComponentPodsDesired.WithLabelValues(ns, "zabbix", "web")),
		}
		want := map[string]float64{"phase Running": 1, "active nodes": 1, "server ready": 2, "web desired": 1}
		for k, v := range checks {
			if v != want[k] {
				return fmt.Errorf("%s = %v, want %v", k, v, want[k])
			}
		}
		return nil
	})

	setActive(ns, "")
	waitPhase(t, ns, zabbixv1alpha1.PhaseDegraded, "No active server node")
	setActive(ns, "zabbix-server-1")
	eventually(t, func() error {
		if got := testutil.ToFloat64(metrics.ServerFailovers.WithLabelValues(ns, "zabbix")); got != 1 {
			return fmt.Errorf("failovers %v, want 1 (counted across the no-active gap)", got)
		}
		return nil
	})

	if err := k8s.Delete(context.Background(), getSystem(t, ns)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if v := testutil.ToFloat64(metrics.ServerActiveNodes.WithLabelValues(ns, "zabbix")); v != 0 {
			return fmt.Errorf("series kept after deletion")
		}
		metrics.ServerActiveNodes.DeleteLabelValues(ns, "zabbix") // the read above recreated it
		return nil
	})
}

// pendingJob waits for an unfinished operator Job running command.
func pendingJob(t *testing.T, ns, command string) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	eventually(t, func() error {
		list := &batchv1.JobList{}
		if err := k8s.List(context.Background(), list, client.InNamespace(ns), client.MatchingLabels{jobs.LabelJob: command}); err != nil {
			return err
		}
		for i := range list.Items {
			if len(list.Items[i].Status.Conditions) == 0 {
				*job = list.Items[i]
				return nil
			}
		}
		return fmt.Errorf("no pending %s Job", command)
	})
	return job
}

// patchSpec applies mutate to the system's spec with a merge patch.
func patchSpec(t *testing.T, ns string, mutate func(*zabbixv1alpha1.ZabbixSystemSpec)) {
	t.Helper()
	sys := getSystem(t, ns)
	before := sys.DeepCopy()
	mutate(&sys.Spec)
	if err := k8s.Patch(context.Background(), sys, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
}

// createBackup creates a CNPG Backup of cluster pg that completed at stopped.
func createBackup(t *testing.T, ns, name string, stopped time.Time) {
	t.Helper()
	b := &unstructured.Unstructured{}
	b.SetGroupVersionKind(schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Backup"})
	b.SetNamespace(ns)
	b.SetName(name)
	if err := unstructured.SetNestedField(b.Object, "pg", "spec", "cluster", "name"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedMap(b.Object, map[string]interface{}{
		"phase": "completed", "stoppedAt": stopped.UTC().Format(time.RFC3339)}, "status"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Status().Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

// The whole 7.0 → 8.0 schema upgrade: gates, all servers stopped, HA table reset, one
// standalone server upgrades the schema, HA servers return on the new version, and the
// frontend follows.
func TestSystem_MajorUpgrade(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	createBackup(t, ns, "before-8", time.Now().Add(-time.Hour))

	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) {
		s.Version, s.Upgrade.ApproveMajor = "8.0.0rc1", "8.0"
	})
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})

	waitPod(t, ns, "zabbix-server-0", false)
	waitPod(t, ns, "zabbix-server-1", false)
	sys := waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "Upgrading from 7.0.25 to 8.0.0rc1: clearing the HA node table")
	if sys.Status.UpgradeStep != zabbixv1alpha1.StepResettingHA || sys.Status.UpgradeTarget != "8.0.0rc1" {
		t.Errorf("step %q target %q", sys.Status.UpgradeStep, sys.Status.UpgradeTarget)
	}
	if image(getPod(t, ns, "zabbix-web-0")) != "zabbix/zabbix-web-nginx-pgsql:ubuntu-7.0.25" {
		t.Error("the frontend must stay on the old version until the servers are back")
	}

	finishJob(t, ns, jobs.Result{Command: jobs.CommandHAReset, OK: true, Deleted: 2})
	initPod := waitPod(t, ns, "zabbix-server-init-0", true)
	if image(initPod) != "zabbix/zabbix-server-pgsql:ubuntu-8.0.0rc1" {
		t.Errorf("schema upgrade image %q", image(initPod))
	}
	if _, ha := envValue(initPod, "zabbix-server", "ZBX_HANODENAME"); ha {
		t.Error("the schema upgrade must run on a standalone server")
	}
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "schema upgrade running on zabbix-server-init-0 (standalone)")

	setPod(t, ns, "zabbix-server-init-0") // the standalone server listens
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "verifying the upgraded schema")
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade),
		Message: "PostgreSQL 17, schema 7000000 will be upgraded for Zabbix 8.0.0rc1"})
	// A mismatch keeps the servers stopped and runs the verification again.
	time.Sleep(2 * time.Second)
	if getPod(t, ns, "zabbix-server-0") != nil {
		t.Fatal("HA servers started before the schema was verified")
	}
	pendingJob(t, ns, jobs.CommandPrecheck)
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SameSchema)})
	waitPod(t, ns, "zabbix-server-init-0", false)
	s0 := waitPod(t, ns, "zabbix-server-0", true)
	if image(s0) != "zabbix/zabbix-server-pgsql:ubuntu-8.0.0rc1" {
		t.Errorf("server image %q", image(s0))
	}
	waitPod(t, ns, "zabbix-server-1", true)
	setPod(t, ns, "zabbix-server-0")
	setPod(t, ns, "zabbix-server-1")
	setActive(ns, "zabbix-server-0")
	eventually(t, func() error {
		p := getPod(t, ns, "zabbix-web-0")
		if p == nil || image(p) != "zabbix/zabbix-web-nginx-pgsql:ubuntu-8.0.0rc1" {
			return fmt.Errorf("frontend not on 8.0.0rc1 yet")
		}
		return nil
	})
	setPod(t, ns, "zabbix-web-0")
	eventually(t, func() error {
		p := getPod(t, ns, "zabbix-webservice-0")
		if p == nil || image(p) != "zabbix/zabbix-web-service:ubuntu-8.0.0rc1" {
			return fmt.Errorf("web service not on 8.0.0rc1 yet")
		}
		return nil
	})
	setPod(t, ns, "zabbix-webservice-0")
	sys = waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "zabbix-server-0 active, 1 standby")
	if sys.Status.RunningVersion != "8.0.0rc1" || sys.Status.UpgradeStep != "" || sys.Status.UpgradeTarget != "" {
		t.Errorf("status %+v", sys.Status)
	}
	waitEvent(t, ns, corev1.EventTypeNormal, "SchemaUpgraded", "Database schema upgraded from 7.0.25 to 8.0.0rc1")
}

// Every gate blocks before anything stops, says why, and lets the upgrade go once the
// cause is resolved.
func TestSystem_MajorUpgradeGates(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	s0, s1 := uid(t, ns, "zabbix-server-0"), uid(t, ns, "zabbix-server-1")

	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version = "8.0.0rc1" })
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked,
		`Upgrading from 7.0 to 8.0 changes the database schema irreversibly; set spec.upgrade.approveMajor: "8.0" to approve it`)

	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Upgrade.ApproveMajor = "8.0" })
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "No completed CNPG Backup of pg within 24h0m0s (latest: none)")

	createBackup(t, ns, "old", time.Now().Add(-48*time.Hour))
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "No completed CNPG Backup of pg within 24h0m0s (latest: ")

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "pg"}, cluster); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(cluster.Object, int64(3), "spec", "instances"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	createBackup(t, ns, "fresh", time.Now())
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "2 of 3 PostgreSQL instances are healthy; the schema upgrade waits for all of them")

	if uid(t, ns, "zabbix-server-0") != s0 || uid(t, ns, "zabbix-server-1") != s1 {
		t.Fatal("servers were touched while the upgrade was blocked")
	}

	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "pg"}, cluster); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(cluster.Object, int64(2), "spec", "instances"); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Update(context.Background(), cluster); err != nil {
		t.Fatal(err)
	}
	waitPod(t, ns, "zabbix-server-0", false) // all gates pass: the servers stop
}

// requireBackupWithin: 0s skips the backup gate.
func TestSystem_MajorUpgradeBackupGateCanBeSkipped(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) {
		s.Version, s.Upgrade.ApproveMajor = "8.0.0rc1", "8.0"
		s.Upgrade.RequireBackupWithin = &metav1.Duration{}
	})
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})
	waitPod(t, ns, "zabbix-server-0", false)
}

// A version older than the running one is blocked without touching anything, and setting
// it back resumes normal operation.
func TestSystem_DowngradeIsBlockedAndWithdrawable(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	s0 := uid(t, ns, "zabbix-server-0")
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version = "7.0.1" })
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked,
		"Zabbix 7.0.25 is running; 7.0.1 is older and Zabbix cannot downgrade. Set spec.version to 7.0.25 or newer")
	if uid(t, ns, "zabbix-server-0") != s0 {
		t.Fatal("a server was replaced for a refused downgrade")
	}
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version = "7.0.25" })
	waitPhase(t, ns, zabbixv1alpha1.PhaseRunning, "active")
}

// A blocked upgrade request can be withdrawn: here 8.0 is blocked by an old PostgreSQL and
// the request is changed to a patch release, which proceeds.
func TestSystem_BlockedUpgradeCanBeWithdrawn(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.1", nil)
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version, s.Upgrade.ApproveMajor = "8.0.0rc1", "8.0" })
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, Reason: "PostgreSQLTooOld",
		Message: "PostgreSQL 14 is too old for Zabbix 8.0 (needs 15 or newer)"})
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "PostgreSQL 14 is too old for Zabbix 8.0 (needs 15 or newer)")

	s1 := uid(t, ns, "zabbix-server-1")
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version = "7.0.25" })
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SameSchema)})
	waitReplaced(t, ns, "zabbix-server-1", s1) // the patch upgrade rolls the standby first
}

// While the database is unavailable, a running schema upgrade is paused, not interrupted.
func TestSystem_MajorUpgradePausesForDatabase(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) {
		s.Version, s.Upgrade.ApproveMajor = "8.0.0rc1", "8.0"
		s.Upgrade.RequireBackupWithin = &metav1.Duration{}
	})
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})
	finishJob(t, ns, jobs.Result{Command: jobs.CommandHAReset, OK: true})
	initUID := waitPod(t, ns, "zabbix-server-init-0", true).UID

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "pg"}, cluster); err != nil {
		t.Fatal(err)
	}
	setClusterStatus(t, cluster, primary1, primary2)
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "paused; Waiting for database: primary is moving from pg-1 to pg-2")
	time.Sleep(time.Second)
	if uid(t, ns, "zabbix-server-init-0") != initUID {
		t.Fatal("the schema upgrade was interrupted")
	}
	setClusterStatus(t, cluster, primary2, primary2)
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "schema upgrade running on zabbix-server-init-0")
}

// ha-reset refuses while a node still heartbeats; the upgrade waits and says why.
func TestSystem_MajorUpgradeWaitsForLiveNodes(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", nil)
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) {
		s.Version, s.Upgrade.ApproveMajor = "8.0.0rc1", "8.0"
		s.Upgrade.RequireBackupWithin = &metav1.Duration{}
	})
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})
	finishJob(t, ns, jobs.Result{Command: jobs.CommandHAReset, Reason: "LiveNodes",
		Message: "refusing to reset: zabbix-server-old heartbeated within 30s"})
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading,
		"waiting to clear the HA node table: refusing to reset: zabbix-server-old heartbeated within 30s")
	if getPod(t, ns, "zabbix-server-init-0") != nil {
		t.Fatal("the schema upgrade started while a node still heartbeats")
	}
}

// A Job recreated under the same name must not read the result its predecessor's pod
// left behind before garbage collection removed it.
func TestSystem_JobResultIgnoresPredecessorPods(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "8.0.0rc1", nil)
	job := pendingJob(t, ns, jobs.CommandPrecheck)
	// A leftover pod of an earlier Job with the same name, not yet garbage-collected.
	stale := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-old", Namespace: ns, Labels: map[string]string{"job-name": job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: "earlier-job",
				Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "job", Image: testOperatorImage}}},
	}
	if err := k8s.Create(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	stale.Status.Phase = corev1.PodSucceeded
	stale.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "job", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Message: `{"command":"precheck","ok":true,"change":"FreshInstall","message":"stale"}`, FinishedAt: metav1.Now()}}}}
	if err := k8s.Status().Update(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, Reason: "PostgreSQLTooOld",
		Message: "PostgreSQL 14 is too old for Zabbix 8.0 (needs 15 or newer)"})
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, "PostgreSQL 14 is too old")
	if getPod(t, ns, "zabbix-server-init-0") != nil {
		t.Fatal("the stale result of an earlier Job was used")
	}
}

func TestSystem_Proxies(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Proxies = []zabbixv1alpha1.ProxySpec{
			{Name: "dc1", Mode: zabbixv1alpha1.ProxyActive, Replicas: 2},
			{Name: "edge", Mode: zabbixv1alpha1.ProxyPassive, Replicas: 1},
		}
	})
	for _, n := range []string{"dc1-0", "dc1-1", "edge-0"} {
		p := waitPod(t, ns, n, true)
		if v, _ := envValue(p, "zabbix-proxy", "ZBX_HOSTNAME"); v != n {
			t.Errorf("%s hostname %q", n, v)
		}
		setPod(t, ns, n)
	}
	for _, name := range []string{"dc1", "edge"} {
		svc := &corev1.Service{}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, svc); err != nil {
			t.Fatalf("proxy Service %s: %v", name, err)
		}
		if svc.Spec.ClusterIP != corev1.ClusterIPNone {
			t.Errorf("proxy Service %s is not headless", name)
		}
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-proxy-dc1"}, &policyv1.PodDisruptionBudget{}); err != nil {
		t.Errorf("proxy PodDisruptionBudget: %v", err)
	}
	eventually(t, func() error {
		for _, c := range getSystem(t, ns).Status.Components {
			if c.Name == "proxy/dc1" && c.Desired == 2 && c.Ready == 2 {
				return nil
			}
		}
		return fmt.Errorf("proxy/dc1 not reported ready: %+v", getSystem(t, ns).Status.Components)
	})

	// Disabling one proxy and deleting the other removes their pods and Services.
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) {
		s.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "dc1", Mode: zabbixv1alpha1.ProxyActive, Replicas: 2, Enabled: ptr.To(false)}}
	})
	for _, n := range []string{"dc1-0", "dc1-1", "edge-0"} {
		waitPod(t, ns, n, false)
	}
	eventually(t, func() error {
		for _, name := range []string{"dc1", "edge"} {
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &corev1.Service{}); !apierrors.IsNotFound(err) {
				return fmt.Errorf("Service %s still present: %v", name, err)
			}
		}
		return nil
	})
}

// Proxies move after the servers in a patch upgrade, like the frontend.
func TestSystem_ProxiesFollowServersInUpgrades(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.1", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Proxies = []zabbixv1alpha1.ProxySpec{{Name: "dc1", Replicas: 1}}
	})
	setPod(t, ns, "dc1-0")
	s1 := uid(t, ns, "zabbix-server-1")
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version = "7.0.25" })
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SameSchema)})
	waitReplaced(t, ns, "zabbix-server-1", s1)
	if image(getPod(t, ns, "dc1-0")) != "zabbix/zabbix-proxy-sqlite3:ubuntu-7.0.1" {
		t.Error("the proxy moved before the servers")
	}
}

func TestSystem_AgentDaemonSet(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	ctx := context.Background()
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Agent = zabbixv1alpha1.AgentSpec{Enabled: true, PodSettings: zabbixv1alpha1.PodSettings{Image: "zabbix/zabbix-agent2:ubuntu-7.0.25"}}
	})
	ds := &appsv1.DaemonSet{}
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-agent"}, ds)
	})
	if ref := metav1.GetControllerOf(ds); ref == nil || ref.Kind != "ZabbixSystem" || !ds.Spec.Template.Spec.HostNetwork {
		t.Fatalf("DaemonSet %+v", ds.ObjectMeta)
	}
	// Play the DaemonSet controller: 3 nodes, 2 ready.
	ds.Status.DesiredNumberScheduled, ds.Status.NumberReady, ds.Status.CurrentNumberScheduled = 3, 2, 3
	if err := k8s.Status().Update(ctx, ds); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		for _, c := range getSystem(t, ns).Status.Components {
			if c.Name == "agent" && c.Desired == 3 && c.Ready == 2 {
				if getSystem(t, ns).Status.Phase != zabbixv1alpha1.PhaseRunning {
					return fmt.Errorf("agent coverage must not change the phase")
				}
				if testutil.ToFloat64(metrics.AgentNodesReady.WithLabelValues(ns, "zabbix")) != 2 {
					return fmt.Errorf("agent metric")
				}
				return nil
			}
		}
		return fmt.Errorf("agent component %+v", getSystem(t, ns).Status.Components)
	})
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Agent.Enabled = false })
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: "zabbix-agent"}, &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("DaemonSet still present: %v", err)
		}
		return nil
	})
}

func TestSystem_AgentDaemonSetConflict(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	foreign := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zabbix-agent", Namespace: ns},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "other"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "x", Image: "busybox"}}}},
		},
	}
	if err := k8s.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	installedUntilConflict := func() {
		readyDatabase(t, ns)
		createSystem(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
			s.Spec.Agent = zabbixv1alpha1.AgentSpec{Enabled: true, PodSettings: zabbixv1alpha1.PodSettings{Image: "zabbix/zabbix-agent2:ubuntu-7.0.25"}}
		})
		finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SameSchema)})
		waitPod(t, ns, "zabbix-server-1", true)
		runAll(t, ns)
	}
	installedUntilConflict()
	waitPhase(t, ns, zabbixv1alpha1.PhaseDegraded, "Conflict: DaemonSet zabbix-agent exists and is not managed by this ZabbixSystem")
}

// A 7.4 installation is adopted and runs on 7.4; moving it to 8.0 is a schema upgrade that
// needs approval for the 8.0 line.
func TestSystem_Adopt74AndUpgradeTo80(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.4.7", nil)
	if image(getPod(t, ns, "zabbix-server-0")) != "zabbix/zabbix-server-pgsql:ubuntu-7.4.7" ||
		image(getPod(t, ns, "zabbix-web-0")) != "zabbix/zabbix-web-nginx-pgsql:ubuntu-7.4.7" {
		t.Fatal("7.4 images not used")
	}
	createBackup(t, ns, "before-8", time.Now().Add(-time.Hour))

	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version = "8.0.0rc1" })
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked,
		`Upgrading from 7.4 to 8.0 changes the database schema irreversibly; set spec.upgrade.approveMajor: "8.0" to approve it`)

	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Upgrade.ApproveMajor = "8.0" })
	waitPod(t, ns, "zabbix-server-0", false)
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "Upgrading from 7.4.7 to 8.0.0rc1: clearing the HA node table")
	finishJob(t, ns, jobs.Result{Command: jobs.CommandHAReset, OK: true, Deleted: 2})
	if image(waitPod(t, ns, "zabbix-server-init-0", true)) != "zabbix/zabbix-server-pgsql:ubuntu-8.0.0rc1" {
		t.Error("schema upgrade must run the 8.0.0rc1 server")
	}
}

// Upgrading between 7.x lines (7.0 → 7.4) is a schema upgrade approved for the 7.4 line.
func TestSystem_Upgrade70To74NeedsApproval(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	installed(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Upgrade.RequireBackupWithin = &metav1.Duration{}
	})
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Version = "7.4.7" })
	finishJob(t, ns, jobs.Result{Command: jobs.CommandPrecheck, OK: true, Change: string(zabbix.SchemaUpgrade)})
	waitPhase(t, ns, zabbixv1alpha1.PhaseBlocked, `set spec.upgrade.approveMajor: "7.4" to approve it`)
	patchSpec(t, ns, func(s *zabbixv1alpha1.ZabbixSystemSpec) { s.Upgrade.ApproveMajor = "7.4" })
	waitPhase(t, ns, zabbixv1alpha1.PhaseUpgrading, "Upgrading from 7.0.25 to 7.4.7")
}

// Jobs pull the operator image with the server's pull secrets, so a private registry works
// for them too.
func TestSystem_JobsUseServerPullSecrets(t *testing.T) {
	requireEnvtest(t)
	ns := newNamespace(t)
	readyDatabase(t, ns)
	createSystem(t, ns, "7.0.25", func(s *zabbixv1alpha1.ZabbixSystem) {
		s.Spec.Server.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry"}}
	})
	job := pendingJob(t, ns, jobs.CommandPrecheck)
	if ps := job.Spec.Template.Spec.ImagePullSecrets; len(ps) != 1 || ps[0].Name != "registry" {
		t.Errorf("precheck pull secrets %v", ps)
	}
}
