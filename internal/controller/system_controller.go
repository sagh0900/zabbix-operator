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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/jobs"
	"github.com/sagh0900/zabbix-operator/internal/metrics"
	"github.com/sagh0900/zabbix-operator/internal/podset"
	"github.com/sagh0900/zabbix-operator/internal/system"
	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

const (
	indexSystemDatabase  = ".spec.databaseRef.name"
	indexSystemRefs      = "zabbix.io/referenced-objects"
	indexDatabaseSecrets = "zabbix.io/database-secrets"

	// activeCheckInterval is how often the active server node is re-checked, which bounds
	// how long the server Service keeps pointing at a node that stepped down.
	activeCheckInterval = 2 * time.Second

	// haNodeGCInterval is how often stale ha_node rows are removed while the system runs,
	// and haNodeGCStaleSeconds how long a row must have gone without a heartbeat.
	haNodeGCInterval     = 5 * time.Minute
	haNodeGCStaleSeconds = 120
)

// SystemReconciler runs a ZabbixSystem: it installs Zabbix, keeps server, frontend and
// web service pods, their Services and Ingresses in place, and rolls pods when their
// configuration or a patch release changes.
type SystemReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder recorder.EventRecorder
	// OperatorImage runs the database Jobs.
	OperatorImage string
	// ActiveProbe finds the active server node; nil means system.TCPActiveProbe.
	ActiveProbe system.ActiveProbe

	// lastActive remembers the last active server pod per system ("namespace/name"), so a
	// failover is counted even when no node is active in between.
	lastActive sync.Map
	// countedJobs remembers finished Jobs (by UID) already counted in metrics.
	countedJobs sync.Map
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=zabbix.io,resources=zabbixsystems,verbs=get;list;watch
// +kubebuilder:rbac:groups=zabbix.io,resources=zabbixsystems/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// isActive reports whether a running server pod is the active HA node.
func (r *SystemReconciler) isActive(ctx context.Context, p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
		return false
	}
	probe := r.ActiveProbe
	if probe == nil {
		probe = system.TCPActiveProbe
	}
	return probe(ctx, p)
}

func (r *SystemReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// observation collects what one reconcile pass learned, for the status.
type observation struct {
	phase      zabbixv1alpha1.SystemPhase
	reason     string
	dbReady    *metav1.Condition
	blocked    string // UpgradeBlocked reason, empty when not blocked
	components []zabbixv1alpha1.ComponentStatus
	conflicts  []string
	active     *corev1.Pod
	activeN    int
	running    string // RunningVersion to record
	webReady   bool
	webEnabled bool
	lastGC     *metav1.Time // LastHANodeGCTime to record
	serverPods []string     // names of live server pods
}

// Reconcile moves one ZabbixSystem one step towards its spec.
func (r *SystemReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sys := &zabbixv1alpha1.ZabbixSystem{}
	if err := r.Get(ctx, req.NamespacedName, sys); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.DeleteSystem(req.Namespace, req.Name)
			r.lastActive.Delete(req.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	obs := &observation{running: sys.Status.RunningVersion, webEnabled: sys.Spec.Web.IsEnabled(), lastGC: sys.Status.LastHANodeGCTime}
	requeue, err := r.reconcileSystem(ctx, sys, obs)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.recordMetrics(sys, obs)
	if err := r.updateStatus(ctx, sys, obs); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *SystemReconciler) reconcileSystem(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, obs *observation) (time.Duration, error) {
	db := &zabbixv1alpha1.ZabbixDatabase{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: sys.Spec.DatabaseRef.Name}, db); err != nil {
		if !apierrors.IsNotFound(err) {
			return 0, err
		}
		obs.phase, obs.reason = zabbixv1alpha1.PhaseDegraded, fmt.Sprintf("Waiting for ZabbixDatabase %q", sys.Spec.DatabaseRef.Name)
		return 10 * time.Second, nil
	}
	obs.dbReady = meta.FindStatusCondition(db.Status.Conditions, zabbixv1alpha1.DatabaseReady)
	hold := ""
	if obs.dbReady == nil || obs.dbReady.Status != metav1.ConditionTrue {
		msg := "not ready"
		if obs.dbReady != nil && obs.dbReady.Message != "" {
			msg = obs.dbReady.Message
		}
		hold = "Waiting for database: " + msg
	}

	target, err := zabbix.ParseVersion(sys.Spec.Version)
	if err != nil {
		obs.phase, obs.reason, obs.blocked = zabbixv1alpha1.PhaseBlocked, err.Error(), "InvalidVersion"
		return 0, nil
	}
	if !target.Supported() {
		obs.phase, obs.reason, obs.blocked = zabbixv1alpha1.PhaseBlocked, zabbix.UnsupportedMessage(target), "UnsupportedVersion"
		// Existing pods keep running unchanged.
		return 0, r.observeOnly(ctx, sys, db, obs)
	}

	switch running := sys.Status.RunningVersion; running {
	case "":
		return r.install(ctx, sys, db, target, hold, obs)
	case sys.Spec.Version:
		requeue, _, err := r.converge(ctx, sys, db, running, running, hold, obs)
		if err == nil && obs.phase == zabbixv1alpha1.PhaseRunning {
			err = r.collectHANodes(ctx, sys, db, obs)
		}
		// After the servers of an upgrade, the frontend and web service still roll; the
		// upgrade is reported until every component has settled.
		if sys.Status.Phase == zabbixv1alpha1.PhaseUpgrading && hold == "" &&
			obs.phase == zabbixv1alpha1.PhaseDegraded && len(obs.conflicts) == 0 && obs.activeN > 0 {
			obs.phase = zabbixv1alpha1.PhaseUpgrading
			obs.reason = "Finishing the upgrade to " + running + ": " + obs.reason
		}
		return requeue, err
	default:
		return r.upgrade(ctx, sys, db, target, hold, obs)
	}
}

// install creates the schema on an empty database with one standalone server, or adopts a
// database whose schema already matches.
func (r *SystemReconciler) install(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	target zabbix.Version, hold string, obs *observation) (time.Duration, error) {
	obs.phase = zabbixv1alpha1.PhaseInstalling
	if hold != "" {
		obs.reason = hold
		return 10 * time.Second, nil
	}
	res, err := r.precheck(ctx, sys, db, target)
	if err != nil || res == nil {
		obs.reason = "Checking the database"
		return 5 * time.Second, err
	}
	if !res.OK {
		obs.phase, obs.reason, obs.blocked = zabbixv1alpha1.PhaseBlocked, res.Message, res.Reason
		return 30 * time.Second, nil
	}
	switch zabbix.Change(res.Change) {
	case zabbix.SameSchema:
		obs.running, obs.reason = sys.Spec.Version, "Database schema matches; starting servers"
		r.event(sys, corev1.EventTypeNormal, "Adopted", "Database schema already matches Zabbix "+sys.Spec.Version)
		return time.Second, nil
	case zabbix.SchemaUpgrade:
		obs.phase, obs.blocked = zabbixv1alpha1.PhaseBlocked, "SchemaUpgradeRequired"
		obs.reason = fmt.Sprintf("The database schema is older than Zabbix %s; install the version it was created with first", target)
		return 30 * time.Second, nil
	}

	// Fresh install: one standalone server creates the schema; it is Ready once the server
	// has started on the new schema.
	st, err := r.podsetReconcile(ctx, sys, system.ServerInit, []podset.Member{{
		Template: system.ServerPod(r.input(ctx, sys, db, sys.Spec.Version, system.ServerSettings(sys), true), system.PodName(sys, system.ServerInit, 0), true),
	}}, system.IsReady, "")
	if err != nil {
		return 0, err
	}
	obs.conflicts = append(obs.conflicts, st.Conflicts...)
	if st.Healthy == 1 {
		obs.running, obs.reason = sys.Spec.Version, "Schema created"
		r.event(sys, corev1.EventTypeNormal, "SchemaCreated", "Zabbix "+sys.Spec.Version+" schema created")
		return time.Second, nil
	}
	obs.reason = "Creating the database schema on " + system.PodName(sys, system.ServerInit, 0)
	if st.Action != "" && !strings.HasPrefix(st.Action, "waiting") {
		obs.reason += ": " + st.Action
	}
	return 5 * time.Second, nil
}

// upgrade moves a running system to a new version. Patch releases share the schema and
// roll servers one at a time; schema upgrades are not performed yet.
func (r *SystemReconciler) upgrade(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	target zabbix.Version, hold string, obs *observation) (time.Duration, error) {
	if hold != "" {
		requeue, _, err := r.converge(ctx, sys, db, sys.Status.RunningVersion, sys.Status.RunningVersion, hold, obs)
		return requeue, err
	}
	res, err := r.precheck(ctx, sys, db, target)
	if err != nil {
		return 0, err
	}
	if res == nil || !res.OK || zabbix.Change(res.Change) != zabbix.SameSchema {
		// Keep running the current version while the upgrade is checked or blocked.
		requeue, _, err := r.converge(ctx, sys, db, sys.Status.RunningVersion, sys.Status.RunningVersion, "", obs)
		switch {
		case res == nil:
			obs.phase, obs.reason = zabbixv1alpha1.PhaseUpgrading, "Checking the database for Zabbix "+sys.Spec.Version
			return 5 * time.Second, err
		case !res.OK:
			obs.phase, obs.reason, obs.blocked = zabbixv1alpha1.PhaseBlocked, res.Message, res.Reason
		default:
			obs.phase, obs.blocked = zabbixv1alpha1.PhaseBlocked, "SchemaUpgradeRequired"
			obs.reason = fmt.Sprintf("Upgrading from %s to %s changes the database schema, which this operator build does not perform",
				sys.Status.RunningVersion, sys.Spec.Version)
		}
		return max(requeue, 30*time.Second), err
	}

	// Patch upgrade: servers move first, standby before active; the frontend and web
	// service follow once every server runs the new version.
	requeue, serversDone, err := r.converge(ctx, sys, db, sys.Spec.Version, sys.Status.RunningVersion, "", obs)
	if err != nil {
		return 0, err
	}
	if serversDone && obs.activeN == 1 {
		obs.running = sys.Spec.Version
		obs.phase = zabbixv1alpha1.PhaseUpgrading
		obs.reason = "Servers upgraded to " + sys.Spec.Version + "; upgrading the frontend and web service"
		r.event(sys, corev1.EventTypeNormal, "Upgraded", fmt.Sprintf("Servers upgraded from %s to %s", sys.Status.RunningVersion, sys.Spec.Version))
		return time.Second, nil
	}
	obs.phase = zabbixv1alpha1.PhaseUpgrading
	if obs.reason == "" || strings.HasPrefix(obs.reason, "Server: ") {
		obs.reason = fmt.Sprintf("Upgrading from %s to %s: %s", sys.Status.RunningVersion, sys.Spec.Version,
			strings.TrimPrefix(obs.reason, "Server: "))
	}
	return requeue, nil
}

// converge keeps every component at the given versions: servers at serverVersion, the
// frontend and web service at otherVersion. It reports whether every server pod runs the
// current server template and is healthy.
func (r *SystemReconciler) converge(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	serverVersion, otherVersion, hold string, obs *observation) (time.Duration, bool, error) {
	// The schema-creation server must be gone before HA servers start.
	initSt, err := r.podsetReconcile(ctx, sys, system.ServerInit, nil, system.IsReady, "")
	if err != nil {
		return 0, false, err
	}
	serverHold := hold
	if initSt.Action != "" {
		serverHold = "Waiting for the schema-creation server to stop"
	}

	// Servers.
	pods, err := r.componentPods(ctx, sys, system.Server)
	if err != nil {
		return 0, false, err
	}
	in := r.input(ctx, sys, db, serverVersion, system.ServerSettings(sys), true)
	var members []podset.Member
	for i := 0; i < int(sys.Spec.ServerReplicas()); i++ {
		name := system.PodName(sys, system.Server, i)
		if p, ok := pods[name]; ok && p.DeletionTimestamp == nil {
			obs.serverPods = append(obs.serverPods, name)
		}
		m := podset.Member{Template: system.ServerPod(in, name, false), LiveLabels: map[string]string{system.LabelRole: system.RoleStandby}}
		if p, ok := pods[name]; ok && r.isActive(ctx, p) {
			m.Order, m.LiveLabels[system.LabelRole] = 1, system.RoleActive
			obs.active = p
			obs.activeN++
		}
		members = append(members, m)
	}
	serverSt, err := r.podsetReconcile(ctx, sys, system.Server, members, nil, serverHold)
	if err != nil {
		return 0, false, err
	}
	obs.add(system.Server, serverSt)

	// Frontend and web service.
	webSt, err := r.statelessSet(ctx, sys, db, system.Web, sys.Spec.Web.IsEnabled(), sys.Spec.WebReplicas(),
		&sys.Spec.Web.PodSettings, true, otherVersion, hold, system.WebPod)
	if err != nil {
		return 0, false, err
	}
	obs.webReady = webSt.Converged || !sys.Spec.Web.IsEnabled()
	wsSt, err := r.statelessSet(ctx, sys, db, system.WebService, sys.Spec.WebService.IsEnabled(), sys.Spec.WebServiceReplicas(),
		&sys.Spec.WebService.PodSettings, false, otherVersion, hold, system.WebServicePod)
	if err != nil {
		return 0, false, err
	}
	if sys.Spec.Web.IsEnabled() {
		obs.add(system.Web, webSt)
	}
	if sys.Spec.WebService.IsEnabled() {
		obs.add(system.WebService, wsSt)
	}

	conflicts, err := r.reconcileNetwork(ctx, sys)
	if err != nil {
		return 0, false, err
	}
	obs.conflicts = append(obs.conflicts, conflicts...)

	// Phase and reason, most important first.
	obs.phase = zabbixv1alpha1.PhaseRunning
	switch {
	case hold != "":
		obs.phase, obs.reason = zabbixv1alpha1.PhaseDegraded, hold
	case len(obs.conflicts) > 0:
		obs.phase, obs.reason = zabbixv1alpha1.PhaseDegraded, "Conflict: "+obs.conflicts[0]
	case serverSt.Action != "":
		obs.phase, obs.reason = zabbixv1alpha1.PhaseDegraded, "Server: "+serverSt.Action
	case obs.activeN == 0:
		obs.phase, obs.reason = zabbixv1alpha1.PhaseDegraded, "No active server node"
	case webSt.Action != "":
		obs.phase, obs.reason = zabbixv1alpha1.PhaseDegraded, "Web: "+webSt.Action
	case wsSt.Action != "":
		obs.phase, obs.reason = zabbixv1alpha1.PhaseDegraded, "Web service: "+wsSt.Action
	default:
		obs.reason = fmt.Sprintf("%s active, %d standby", obs.active.Name, int(sys.Spec.ServerReplicas())-1)
	}
	serversDone := serverSt.Converged && hold == ""
	// The active node is re-checked continuously so the Service follows a failover.
	return activeCheckInterval, serversDone, nil
}

// statelessSet reconciles the frontend or web service.
func (r *SystemReconciler) statelessSet(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	component string, enabled bool, replicas int32, settings *zabbixv1alpha1.PodSettings, usesDB bool, version, hold string,
	build func(system.Input, string) *corev1.Pod) (podset.Status, error) {
	var members []podset.Member
	if enabled {
		in := r.input(ctx, sys, db, version, settings, usesDB)
		for i := 0; i < int(replicas); i++ {
			members = append(members, podset.Member{Template: build(in, system.PodName(sys, component, i))})
		}
	}
	return r.podsetReconcile(ctx, sys, component, members, nil, hold)
}

func (r *SystemReconciler) podsetReconcile(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, component string,
	members []podset.Member, healthy func(*corev1.Pod) bool, hold string) (podset.Status, error) {
	m := &podset.Manager{Client: r.Client, Scheme: r.Scheme}
	st, err := m.Reconcile(ctx, podset.Set{
		Owner: sys, System: sys.Name, Component: component, Members: members,
		Healthy: healthy, Hold: hold, DisruptionBudget: component != system.ServerInit,
	})
	for _, d := range st.Deleted {
		metrics.PodReplacements.WithLabelValues(sys.Namespace, sys.Name, component, string(d.Reason)).Inc()
	}
	return st, err
}

// collectHANodes runs ha-gc every haNodeGCInterval while the system runs, keeping the live
// server pods and removing rows that have not heartbeated for haNodeGCStaleSeconds.
func (r *SystemReconciler) collectHANodes(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem,
	db *zabbixv1alpha1.ZabbixDatabase, obs *observation) error {
	now := r.now()
	if last := sys.Status.LastHANodeGCTime; last != nil && now.Sub(last.Time) < haNodeGCInterval {
		return nil
	}
	res, err := r.runJob(ctx, jobs.Spec{
		Owner: sys, System: sys.Name, Command: jobs.CommandHAGC,
		Args: []string{
			"--keep=" + strings.Join(obs.serverPods, ","),
			fmt.Sprintf("--stale-seconds=%d", haNodeGCStaleSeconds),
		},
		Image: r.OperatorImage, Database: db, Host: db.DirectHostOrDefault(),
		RunID: fmt.Sprint(now.Unix() / int64(haNodeGCInterval/time.Second)),
	})
	if err != nil || res == nil {
		return err
	}
	if !res.OK {
		r.event(sys, corev1.EventTypeWarning, "HANodeGCFailed", res.Message)
		return nil
	}
	t := metav1.NewTime(now)
	obs.lastGC = &t
	metrics.HANodeGCRowsDeleted.WithLabelValues(sys.Namespace, sys.Name).Add(float64(res.Deleted))
	if res.Deleted > 0 {
		r.event(sys, corev1.EventTypeNormal, "HANodesRemoved", res.Message)
	}
	return nil
}

// recordMetrics exports what the pass observed.
func (r *SystemReconciler) recordMetrics(sys *zabbixv1alpha1.ZabbixSystem, obs *observation) {
	ns, name := sys.Namespace, sys.Name
	metrics.SetSystemPhase(ns, name, string(obs.phase))
	metrics.SetSystemInfo(ns, name, sys.Spec.Version, obs.running)
	metrics.SetUpgradeBlocked(ns, name, obs.blocked)
	upgrading := 0.0
	if obs.phase == zabbixv1alpha1.PhaseUpgrading || (obs.running != "" && obs.running != sys.Spec.Version) {
		upgrading = 1
	}
	metrics.UpgradeInProgress.WithLabelValues(ns, name).Set(upgrading)
	metrics.ServerActiveNodes.WithLabelValues(ns, name).Set(float64(obs.activeN))
	metrics.ServerFailovers.WithLabelValues(ns, name)
	for _, c := range obs.components {
		metrics.ComponentPodsDesired.WithLabelValues(ns, name, c.Name).Set(float64(c.Desired))
		metrics.ComponentPodsReady.WithLabelValues(ns, name, c.Name).Set(float64(c.Ready))
	}
	if obs.lastGC != nil {
		metrics.HANodeGCLastSuccess.WithLabelValues(ns, name).Set(float64(obs.lastGC.Unix()))
	}
	if obs.active != nil {
		key := ns + "/" + name
		if prev, ok := r.lastActive.Load(key); ok && prev.(string) != obs.active.Name {
			metrics.ServerFailovers.WithLabelValues(ns, name).Inc()
		}
		r.lastActive.Store(key, obs.active.Name)
	}
}

// observeOnly records the state of existing servers without changing anything.
func (r *SystemReconciler) observeOnly(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, _ *zabbixv1alpha1.ZabbixDatabase, obs *observation) error {
	pods, err := r.componentPods(ctx, sys, system.Server)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if r.isActive(ctx, p) {
			obs.active, obs.activeN = p, obs.activeN+1
		}
	}
	return nil
}

func (r *SystemReconciler) componentPods(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, component string) (map[string]*corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(sys.Namespace), client.MatchingLabels(podset.SelectorLabels(sys.Name, component))); err != nil {
		return nil, err
	}
	out := map[string]*corev1.Pod{}
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out, nil
}

// precheck runs the precheck Job for target and returns its result once it is known.
func (r *SystemReconciler) precheck(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	target zabbix.Version) (*jobs.Result, error) {
	return r.runJob(ctx, jobs.Spec{
		Owner: sys, System: sys.Name, Command: jobs.CommandPrecheck,
		Args:  []string{"--target-version=" + target.String()},
		Image: r.OperatorImage, Database: db, Host: db.DirectHostOrDefault(),
	})
}

// input builds the builder input, hashing the versions of every referenced Secret and
// ConfigMap so a change to them rolls the pods.
func (r *SystemReconciler) input(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	version string, settings *zabbixv1alpha1.PodSettings, usesDB bool) system.Input {
	secrets, configMaps := system.ReferencedObjects(db, settings, usesDB)
	h := sha256.New()
	for _, ref := range []struct {
		kind  string
		names []string
	}{{"Secret", secrets}, {"ConfigMap", configMaps}} {
		for _, name := range ref.names {
			meta := &metav1.PartialObjectMetadata{}
			meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(ref.kind))
			version := "missing"
			if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: name}, meta); err == nil {
				version = string(meta.UID) + "/" + meta.ResourceVersion
			}
			_, _ = fmt.Fprintf(h, "%s/%s=%s\n", ref.kind, name, version)
		}
	}
	hash := ""
	if len(secrets)+len(configMaps) > 0 {
		hash = hex.EncodeToString(h.Sum(nil))[:16]
	}
	return system.Input{System: sys, Database: db, Version: version, ConfigHash: hash}
}

// reconcileNetwork keeps the Services and Ingresses, and removes those no longer wanted.
func (r *SystemReconciler) reconcileNetwork(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem) ([]string, error) {
	var conflicts []string
	wantSvc := map[string]bool{}
	for _, s := range system.Services(sys) {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: sys.Namespace, Name: system.ServiceName(sys, s.Component)}}
		wantSvc[svc.Name] = true
		ok, err := r.applyOwned(ctx, sys, svc, func() { system.ApplyService(sys, s, svc) })
		if err != nil {
			return nil, err
		}
		if !ok {
			conflicts = append(conflicts, "Service "+svc.Name+" exists and is not managed by this ZabbixSystem")
		}
	}
	wantIng := map[string]bool{}
	for _, s := range system.Ingresses(sys) {
		ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: sys.Namespace, Name: system.IngressName(sys, s.Component)}}
		wantIng[ing.Name] = true
		ok, err := r.applyOwned(ctx, sys, ing, func() { system.ApplyIngress(sys, s, ing) })
		if err != nil {
			return nil, err
		}
		if !ok {
			conflicts = append(conflicts, "Ingress "+ing.Name+" exists and is not managed by this ZabbixSystem")
		}
	}
	if err := r.pruneOwned(ctx, sys, &corev1.ServiceList{}, wantSvc); err != nil {
		return nil, err
	}
	if err := r.pruneOwned(ctx, sys, &networkingv1.IngressList{}, wantIng); err != nil {
		return nil, err
	}
	return conflicts, nil
}

// applyOwned creates or patches obj with mutate. It returns false, without touching the
// object, when obj exists and is controlled by something else.
func (r *SystemReconciler) applyOwned(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, obj client.Object, mutate func()) (bool, error) {
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	if err == nil {
		if ref := metav1.GetControllerOf(obj); ref == nil || ref.UID != sys.UID {
			return false, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	_, err = controllerutil.CreateOrPatch(ctx, r.Client, obj, func() error {
		mutate()
		return controllerutil.SetControllerReference(sys, obj, r.Scheme)
	})
	return true, err
}

// pruneOwned deletes objects of list's kind that sys controls but no longer wants.
func (r *SystemReconciler) pruneOwned(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, list client.ObjectList, want map[string]bool) error {
	if err := r.List(ctx, list, client.InNamespace(sys.Namespace), client.MatchingLabels{
		podset.LabelManagedBy: podset.ManagedBy, podset.LabelSystem: sys.Name}); err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	for _, it := range items {
		obj := it.(client.Object)
		ref := metav1.GetControllerOf(obj)
		if ref == nil || ref.UID != sys.UID || want[obj.GetName()] {
			continue
		}
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func (o *observation) add(component string, st podset.Status) {
	o.components = append(o.components, zabbixv1alpha1.ComponentStatus{Name: component, Desired: st.Desired, Ready: st.Healthy})
	o.conflicts = append(o.conflicts, st.Conflicts...)
}

func (r *SystemReconciler) event(sys *zabbixv1alpha1.ZabbixSystem, kind, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Eventf(sys, nil, kind, reason, "Reconcile", "%s", msg)
	}
}

// updateStatus writes what the pass observed.
func (r *SystemReconciler) updateStatus(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, obs *observation) error {
	st := *sys.Status.DeepCopy()
	gen := sys.Generation
	st.ObservedGeneration = gen
	if obs.phase == zabbixv1alpha1.PhaseBlocked && st.Phase != zabbixv1alpha1.PhaseBlocked {
		r.event(sys, corev1.EventTypeWarning, obs.blocked, obs.reason)
	}
	st.Phase, st.PhaseReason, st.RunningVersion = obs.phase, obs.reason, obs.running
	st.LastHANodeGCTime = obs.lastGC
	if obs.components != nil {
		st.Components = obs.components
	}
	if obs.active != nil {
		st.ActiveServer = &zabbixv1alpha1.ActiveServer{Pod: obs.active.Name, IP: obs.active.Status.PodIP}
	} else if obs.phase != zabbixv1alpha1.PhaseInstalling {
		st.ActiveServer = nil
	}
	switch prev, cur := sys.Status.ActiveServer, st.ActiveServer; {
	case prev != nil && cur == nil && obs.phase != zabbixv1alpha1.PhaseInstalling:
		r.event(sys, corev1.EventTypeWarning, "ActiveServerLost", prev.Pod+" is no longer active; no server node is active")
	case cur != nil && (prev == nil || prev.Pod != cur.Pod):
		r.event(sys, corev1.EventTypeNormal, "ActiveServer", cur.Pod+" is the active server node")
	}

	set := func(t string, ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen})
	}
	if obs.dbReady != nil {
		set(zabbixv1alpha1.SystemDatabaseReady, obs.dbReady.Status == metav1.ConditionTrue, obs.dbReady.Reason, obs.dbReady.Message)
	} else {
		set(zabbixv1alpha1.SystemDatabaseReady, false, "DatabaseNotFound", obs.reason)
	}
	switch {
	case obs.activeN == 1:
		set(zabbixv1alpha1.SystemServerActive, true, "Active", obs.active.Name+" is active")
	case obs.activeN > 1:
		set(zabbixv1alpha1.SystemServerActive, false, "MultipleActive", fmt.Sprintf("%d server pods report active", obs.activeN))
	default:
		set(zabbixv1alpha1.SystemServerActive, false, "NoActiveNode", "no server pod is active")
	}
	switch {
	case !obs.webEnabled:
		set(zabbixv1alpha1.SystemWebReady, true, "Disabled", "the frontend is disabled")
	case obs.webReady:
		set(zabbixv1alpha1.SystemWebReady, true, "Ready", "")
	default:
		set(zabbixv1alpha1.SystemWebReady, false, "NotReady", "frontend pods are not all ready")
	}
	if obs.blocked != "" {
		set(zabbixv1alpha1.SystemUpgradeBlocked, true, obs.blocked, obs.reason)
	} else {
		set(zabbixv1alpha1.SystemUpgradeBlocked, false, "NotBlocked", "")
	}
	upgrading := obs.phase == zabbixv1alpha1.PhaseUpgrading || (sys.Spec.Version != obs.running && obs.running != "")
	if upgrading {
		set(zabbixv1alpha1.SystemUpgrading, true, "Upgrading", fmt.Sprintf("from %s to %s", obs.running, sys.Spec.Version))
	} else {
		set(zabbixv1alpha1.SystemUpgrading, false, "UpToDate", "")
	}
	if len(obs.conflicts) > 0 {
		set(zabbixv1alpha1.SystemConflict, true, "Conflict", strings.Join(obs.conflicts, "; "))
	} else {
		set(zabbixv1alpha1.SystemConflict, false, "NoConflict", "")
	}

	if equality.Semantic.DeepEqual(st, sys.Status) {
		return nil
	}
	sys.Status = st
	return r.Status().Update(ctx, sys)
}

// SetupWithManager registers the controller, its indexes and watches.
func (r *SystemReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &zabbixv1alpha1.ZabbixSystem{}, indexSystemDatabase, func(o client.Object) []string {
		return []string{o.(*zabbixv1alpha1.ZabbixSystem).Spec.DatabaseRef.Name}
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &zabbixv1alpha1.ZabbixSystem{}, indexSystemRefs, referencedKeys); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &zabbixv1alpha1.ZabbixDatabase{}, indexDatabaseSecrets, func(o client.Object) []string {
		secrets, _ := system.ReferencedObjects(o.(*zabbixv1alpha1.ZabbixDatabase), &zabbixv1alpha1.PodSettings{}, true)
		return secrets
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("zabbixsystem").
		For(&zabbixv1alpha1.ZabbixSystem{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&batchv1.Job{}).
		Watches(&zabbixv1alpha1.ZabbixDatabase{}, handler.EnqueueRequestsFromMapFunc(r.systemsBy(indexSystemDatabase, ""))).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.systemsForSecret), builder.OnlyMetadata).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.systemsBy(indexSystemRefs, "ConfigMap/")), builder.OnlyMetadata).
		Complete(r)
}

// referencedKeys indexes a system by every Secret and ConfigMap its pods read, plus the
// database's credentials and TLS Secrets.
func referencedKeys(o client.Object) []string {
	sys := o.(*zabbixv1alpha1.ZabbixSystem)
	var keys []string
	for _, s := range []*zabbixv1alpha1.PodSettings{&sys.Spec.Server.PodSettings, &sys.Spec.Web.PodSettings, &sys.Spec.WebService.PodSettings} {
		secrets, cms := system.ReferencedObjects(&zabbixv1alpha1.ZabbixDatabase{}, s, false)
		for _, n := range secrets {
			keys = append(keys, "Secret/"+n)
		}
		for _, n := range cms {
			keys = append(keys, "ConfigMap/"+n)
		}
	}
	return keys
}

// systemsForSecret maps a Secret to the systems that read it directly or through their
// ZabbixDatabase (credentials and TLS).
func (r *SystemReconciler) systemsForSecret(ctx context.Context, o client.Object) []reconcile.Request {
	reqs := r.systemsBy(indexSystemRefs, "Secret/")(ctx, o)
	dbs := &zabbixv1alpha1.ZabbixDatabaseList{}
	if err := r.List(ctx, dbs, client.InNamespace(o.GetNamespace()), client.MatchingFields{indexDatabaseSecrets: o.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "listing ZabbixDatabases")
		return reqs
	}
	for _, db := range dbs.Items {
		reqs = append(reqs, r.systemsBy(indexSystemDatabase, "")(ctx, &db)...)
	}
	return reqs
}

// systemsBy maps an object to the systems in its namespace indexed under prefix+name.
func (r *SystemReconciler) systemsBy(field, prefix string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		list := &zabbixv1alpha1.ZabbixSystemList{}
		if err := r.List(ctx, list, client.InNamespace(o.GetNamespace()), client.MatchingFields{field: prefix + o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "listing ZabbixSystems", "field", field)
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for _, s := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&s)})
		}
		return reqs
	}
}
