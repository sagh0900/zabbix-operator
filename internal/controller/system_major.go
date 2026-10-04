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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/jobs"
	"github.com/sagh0900/zabbix-operator/internal/podset"
	"github.com/sagh0900/zabbix-operator/internal/system"
	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

// CNPGBackupListGVK identifies a list of CloudNativePG Backups.
var CNPGBackupListGVK = schema.GroupVersionKind{Group: cnpgGroup, Version: "v1", Kind: "BackupList"}

// defaultBackupWindow applies when requireBackupWithin is unset.
const defaultBackupWindow = 24 * time.Hour

// majorGates checks what a schema upgrade needs before anything stops: an approval when
// the release line changes, every PostgreSQL instance healthy, and a recent CNPG Backup.
// It returns a blocking reason and message, or empty strings.
func (r *SystemReconciler) majorGates(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	running, target zabbix.Version) (string, string, error) {
	if target.Line() != running.Line() && sys.Spec.Upgrade.ApproveMajor != target.Line() {
		return "MajorUpgradeNotApproved", fmt.Sprintf(
			"Upgrading from %s to %s changes the database schema irreversibly; set spec.upgrade.approveMajor: %q to approve it",
			running.Line(), target.Line(), target.Line()), nil
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: db.Spec.ClusterRef.Name}, cluster); err != nil {
		return "", "", err
	}
	instances, _, _ := unstructured.NestedInt64(cluster.Object, "spec", "instances")
	healthy, _, _ := unstructured.NestedStringSlice(cluster.Object, "status", "instancesStatus", "healthy")
	if int64(len(healthy)) < instances {
		return "ReplicasNotInSync", fmt.Sprintf(
			"%d of %d PostgreSQL instances are healthy; the schema upgrade waits for all of them", len(healthy), instances), nil
	}

	window := defaultBackupWindow
	if w := sys.Spec.Upgrade.RequireBackupWithin; w != nil {
		window = w.Duration
	}
	if window <= 0 {
		return "", "", nil
	}
	latest, err := r.latestBackup(ctx, sys.Namespace, db.Spec.ClusterRef.Name)
	if err != nil {
		return "", "", err
	}
	if latest.IsZero() || r.now().Sub(latest) > window {
		last := "none"
		if !latest.IsZero() {
			last = latest.UTC().Format(time.RFC3339)
		}
		return "BackupRequired", fmt.Sprintf(
			"No completed CNPG Backup of %s within %s (latest: %s); take one, or set spec.upgrade.requireBackupWithin: 0s to skip this check",
			db.Spec.ClusterRef.Name, window, last), nil
	}
	return "", "", nil
}

// latestBackup returns when the newest completed CNPG Backup of cluster finished.
func (r *SystemReconciler) latestBackup(ctx context.Context, ns, cluster string) (time.Time, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(CNPGBackupListGVK)
	if err := r.List(ctx, list, client.InNamespace(ns)); err != nil {
		return time.Time{}, err
	}
	var latest time.Time
	for _, b := range list.Items {
		name, _, _ := unstructured.NestedString(b.Object, "spec", "cluster", "name")
		phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
		stopped, _, _ := unstructured.NestedString(b.Object, "status", "stoppedAt")
		if name != cluster || phase != "completed" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, stopped); err == nil && t.After(latest) {
			latest = t
		}
	}
	return latest, nil
}

// majorStep advances a schema upgrade by one step. The step and target are kept in the
// status, so the upgrade resumes after an operator restart.
func (r *SystemReconciler) majorStep(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	hold string, obs *observation) (time.Duration, error) {
	from, to := sys.Status.RunningVersion, sys.Status.UpgradeTarget
	obs.step, obs.target, obs.phase = sys.Status.UpgradeStep, to, zabbixv1alpha1.PhaseUpgrading
	say := func(format string, args ...any) {
		obs.reason = fmt.Sprintf("Upgrading from %s to %s: ", from, to) + fmt.Sprintf(format, args...)
		if sys.Spec.Version != to {
			obs.reason += fmt.Sprintf(" (spec.version %s waits until this upgrade has finished)", sys.Spec.Version)
		}
	}

	// The frontend and web service keep running the old version until the servers are
	// back; they cannot work against the new schema before that.
	for _, c := range []struct {
		name     string
		enabled  bool
		replicas int32
		settings *zabbixv1alpha1.PodSettings
		usesDB   bool
		build    func(system.Input, string) *corev1.Pod
	}{
		{system.Web, sys.Spec.Web.IsEnabled(), sys.Spec.WebReplicas(), &sys.Spec.Web.PodSettings, true, system.WebPod},
		{system.WebService, sys.Spec.WebService.IsEnabled(), sys.Spec.WebServiceReplicas(), &sys.Spec.WebService.PodSettings, false, system.WebServicePod},
	} {
		st, err := r.statelessSet(ctx, sys, db, c.name, c.enabled, c.replicas, c.settings, c.usesDB, from, hold, c.build)
		if err != nil {
			return 0, err
		}
		if c.enabled {
			obs.add(c.name, st)
		}
	}

	// Proxies of the previous version keep sending data while the servers are down.
	if _, err := r.reconcileProxies(ctx, sys, db, from, hold, obs); err != nil {
		return 0, err
	}

	if hold != "" {
		// Nothing advances while the database is unavailable; a running schema upgrade is
		// left alone.
		if obs.step == zabbixv1alpha1.StepUpgradingSchema {
			if _, err := r.podsetReconcile(ctx, sys, system.ServerInit, r.schemaPod(ctx, sys, db, to), system.IsReady, hold); err != nil {
				return 0, err
			}
		}
		say("paused; %s", hold)
		return 10 * time.Second, nil
	}

	switch obs.step {
	case zabbixv1alpha1.StepStoppingServers:
		st, err := r.podsetReconcile(ctx, sys, system.Server, nil, nil, "")
		if err != nil {
			return 0, err
		}
		pods, err := r.componentPods(ctx, sys, system.Server)
		if err != nil {
			return 0, err
		}
		if len(pods) > 0 || st.Action != "" {
			say("stopping the servers (%d left)", len(pods))
			return 2 * time.Second, nil
		}
		obs.step = zabbixv1alpha1.StepResettingHA
		say("servers stopped")
		return time.Second, nil

	case zabbixv1alpha1.StepResettingHA:
		res, err := r.runJob(ctx, jobs.Spec{
			Owner: sys, System: sys.Name, Command: jobs.CommandHAReset, Args: []string{"--stale-seconds=30"},
			Image: r.OperatorImage, Database: db, Host: db.DirectHostOrDefault(), RunID: "upgrade-" + to,
		})
		switch {
		case err != nil:
			return 0, err
		case res == nil:
			say("clearing the HA node table")
			return 5 * time.Second, nil
		case !res.OK:
			say("waiting to clear the HA node table: %s", res.Message)
			return 15 * time.Second, nil
		}
		obs.step = zabbixv1alpha1.StepUpgradingSchema
		say("HA node table cleared")
		return time.Second, nil

	case zabbixv1alpha1.StepUpgradingSchema:
		name := system.PodName(sys, system.ServerInit, 0)
		st, err := r.podsetReconcile(ctx, sys, system.ServerInit, r.schemaPod(ctx, sys, db, to), system.IsReady, "")
		if err != nil {
			return 0, err
		}
		obs.conflicts = append(obs.conflicts, st.Conflicts...)
		if st.Healthy != 1 {
			say("schema upgrade running on %s (standalone)", name)
			return 5 * time.Second, nil
		}
		// The standalone server is up; confirm in the database that the schema matches the
		// target before anything else uses it.
		target, err := zabbix.ParseVersion(to)
		if err != nil {
			return 0, err
		}
		verify := jobs.Spec{
			Owner: sys, System: sys.Name, Command: jobs.CommandPrecheck, Args: []string{"--target-version=" + to},
			Image: r.OperatorImage, Database: db, Host: db.DirectHostOrDefault(), RunID: "verify-" + to,
		}
		res, err := r.runJob(ctx, verify)
		switch {
		case err != nil:
			return 0, err
		case res == nil:
			say("verifying the upgraded schema")
			return 3 * time.Second, nil
		case !res.OK || zabbix.Change(res.Change) != zabbix.SameSchema:
			// Check again shortly: the next pass runs a fresh verification.
			if err := r.deleteJob(ctx, verify); err != nil {
				return 0, err
			}
			say("the schema does not match %s yet (%s); waiting for %s", target, res.Message, name)
			return 10 * time.Second, nil
		}
		{
			obs.running, obs.step, obs.target = to, "", ""
			obs.reason = fmt.Sprintf("Schema upgraded to %s; starting the servers", to)
			r.event(sys, corev1.EventTypeNormal, "SchemaUpgraded", fmt.Sprintf("Database schema upgraded from %s to %s", from, to))
			return time.Second, nil
		}
	}
	return 0, fmt.Errorf("unknown upgrade step %q", obs.step)
}

// schemaPod is the standalone server that upgrades the schema to version.
func (r *SystemReconciler) schemaPod(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, db *zabbixv1alpha1.ZabbixDatabase,
	version string) []podset.Member {
	in := r.input(ctx, sys, db, version, system.ServerSettings(sys), true)
	return []podset.Member{{Template: system.ServerPod(in, system.PodName(sys, system.ServerInit, 0), true)}}
}
