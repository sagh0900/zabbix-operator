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
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/flap"
)

// Condition reasons for ZabbixDatabase.
const (
	ReasonClusterNotFound      = "ClusterNotFound"
	ReasonNoPrimary            = "NoPrimary"
	ReasonSwitchoverInProgress = "SwitchoverInProgress"
	ReasonPrimaryNotHealthy    = "PrimaryNotHealthy"
	ReasonPrimaryHealthy       = "PrimaryHealthy"
	ReasonSecretNotFound       = "SecretNotFound"
	ReasonKeysMissing          = "KeysMissing"
	ReasonCredentialsFound     = "CredentialsFound"
	ReasonPrimaryFlapping      = "PrimaryFlapping"
	ReasonPrimaryStable        = "Stable"
	ReasonReady                = "Ready"
)

// cnpgCluster is the part of a CNPG Cluster's status the operator relies on.
type cnpgCluster struct {
	found          bool
	phase          string
	currentPrimary string
	targetPrimary  string
	healthy        []string
}

// parseCNPGCluster extracts the fields used by computeDatabaseStatus. A nil object means
// the Cluster does not exist.
func parseCNPGCluster(u *unstructured.Unstructured) cnpgCluster {
	if u == nil {
		return cnpgCluster{}
	}
	c := cnpgCluster{found: true}
	c.phase, _, _ = unstructured.NestedString(u.Object, "status", "phase")
	c.currentPrimary, _, _ = unstructured.NestedString(u.Object, "status", "currentPrimary")
	c.targetPrimary, _, _ = unstructured.NestedString(u.Object, "status", "targetPrimary")
	c.healthy, _, _ = unstructured.NestedStringSlice(u.Object, "status", "instancesStatus", "healthy")
	return c
}

// credentialsState describes the referenced credentials Secret.
type credentialsState struct {
	found       bool
	missingKeys []string
}

// Default keys of the credentials Secret, matching the CRD defaults.
const (
	defaultUsernameKey = "username"
	defaultPasswordKey = "password"
)

// credentialKeys returns the username and password keys with defaults applied.
func credentialKeys(db *zabbixv1alpha1.ZabbixDatabase) (string, string) {
	user, pass := db.Spec.CredentialsRef.UsernameKey, db.Spec.CredentialsRef.PasswordKey
	if user == "" {
		user = defaultUsernameKey
	}
	if pass == "" {
		pass = defaultPasswordKey
	}
	return user, pass
}

// inspectCredentials reports which required keys are missing or empty in data.
// A nil map means the Secret does not exist.
func inspectCredentials(db *zabbixv1alpha1.ZabbixDatabase, data map[string][]byte) credentialsState {
	if data == nil {
		return credentialsState{}
	}
	s := credentialsState{found: true}
	user, pass := credentialKeys(db)
	for _, k := range []string{user, pass} {
		if len(data[k]) == 0 {
			s.missingKeys = append(s.missingKeys, k)
		}
	}
	return s
}

// computeDatabaseStatus derives the status of db from the observed CNPG cluster and
// credentials. It returns the new status and, while primary changes are being counted,
// how long to wait before re-evaluating so the count can decay.
func computeDatabaseStatus(
	db *zabbixv1alpha1.ZabbixDatabase,
	cluster cnpgCluster,
	creds credentialsState,
	now time.Time,
) (zabbixv1alpha1.ZabbixDatabaseStatus, time.Duration) {
	st := *db.Status.DeepCopy()
	gen := db.Generation
	st.ObservedGeneration = gen

	set := func(condType string, ok bool, reason, msg string) {
		status := metav1.ConditionFalse
		if ok {
			status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{
			Type: condType, Status: status, Reason: reason, Message: msg, ObservedGeneration: gen,
			LastTransitionTime: metav1.NewTime(now),
		})
	}

	// Cluster health.
	clusterOK, clusterReason, clusterMsg := clusterReadiness(db, cluster)
	set(zabbixv1alpha1.DatabaseClusterReady, clusterOK, clusterReason, clusterMsg)

	// Credentials.
	credOK, credReason, credMsg := credentialsReadiness(db, creds)
	set(zabbixv1alpha1.DatabaseCredentialsReady, credOK, credReason, credMsg)

	// Primary stability. Only evaluated while the cluster is visible, so a missing
	// Cluster does not reset the history.
	threshold, window := flapSettings(db)
	var requeue time.Duration
	if cluster.found {
		st.Phase = cluster.phase
		r := flap.Evaluate(st.CurrentPrimary, cluster.currentPrimary, st.PrimaryChanges,
			st.LastPrimaryChange, now, window, threshold)
		if cluster.currentPrimary != "" {
			st.CurrentPrimary = cluster.currentPrimary
		}
		st.PrimaryChanges = r.Count
		st.LastPrimaryChange = r.LastChange
	}
	stable := st.PrimaryChanges < threshold
	if stable {
		set(zabbixv1alpha1.DatabasePrimaryStable, true, ReasonPrimaryStable, "")
	} else {
		set(zabbixv1alpha1.DatabasePrimaryStable, false, ReasonPrimaryFlapping,
			fmt.Sprintf("primary changed %d times within %s", st.PrimaryChanges, window))
	}
	if st.PrimaryChanges > 0 && st.LastPrimaryChange != nil {
		requeue = st.LastPrimaryChange.Add(window).Sub(now) + time.Second
		if requeue < time.Second {
			requeue = time.Second
		}
	}

	// Aggregate.
	switch {
	case !clusterOK:
		set(zabbixv1alpha1.DatabaseReady, false, clusterReason, clusterMsg)
	case !credOK:
		set(zabbixv1alpha1.DatabaseReady, false, credReason, credMsg)
	case !stable:
		set(zabbixv1alpha1.DatabaseReady, false, ReasonPrimaryFlapping,
			meta.FindStatusCondition(st.Conditions, zabbixv1alpha1.DatabasePrimaryStable).Message)
	default:
		set(zabbixv1alpha1.DatabaseReady, true, ReasonReady, "Zabbix may connect")
	}
	return st, requeue
}

func clusterReadiness(db *zabbixv1alpha1.ZabbixDatabase, c cnpgCluster) (bool, string, string) {
	switch {
	case !c.found:
		return false, ReasonClusterNotFound,
			fmt.Sprintf("CNPG Cluster %q not found", db.Spec.ClusterRef.Name)
	case c.currentPrimary == "":
		return false, ReasonNoPrimary, "the cluster has no primary"
	case c.targetPrimary != "" && c.targetPrimary != c.currentPrimary:
		return false, ReasonSwitchoverInProgress,
			fmt.Sprintf("primary is moving from %s to %s", c.currentPrimary, c.targetPrimary)
	case !slices.Contains(c.healthy, c.currentPrimary):
		return false, ReasonPrimaryNotHealthy,
			fmt.Sprintf("primary %s is not reported healthy", c.currentPrimary)
	default:
		return true, ReasonPrimaryHealthy, fmt.Sprintf("primary %s is healthy", c.currentPrimary)
	}
}

func credentialsReadiness(db *zabbixv1alpha1.ZabbixDatabase, s credentialsState) (bool, string, string) {
	name := db.Spec.CredentialsRef.SecretName
	switch {
	case !s.found:
		return false, ReasonSecretNotFound, fmt.Sprintf("Secret %q not found", name)
	case len(s.missingKeys) > 0:
		return false, ReasonKeysMissing,
			fmt.Sprintf("Secret %q has no value for %v", name, s.missingKeys)
	default:
		return true, ReasonCredentialsFound, ""
	}
}

func flapSettings(db *zabbixv1alpha1.ZabbixDatabase) (int, time.Duration) {
	threshold, window := flap.DefaultThreshold, flap.DefaultWindow
	if f := db.Spec.Flap; f != nil {
		if f.Threshold > 0 {
			threshold = f.Threshold
		}
		if f.Window != nil && f.Window.Duration > 0 {
			window = f.Window.Duration
		}
	}
	return threshold, window
}
