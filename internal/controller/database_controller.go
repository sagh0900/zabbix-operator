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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
)

// CNPGClusterGVK identifies a CloudNativePG Cluster.
var CNPGClusterGVK = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}

const (
	indexClusterRef = ".spec.clusterRef.name"
	indexSecretRef  = ".spec.credentialsRef.secretName"
)

// DatabaseReconciler keeps ZabbixDatabase status in line with its CNPG cluster and
// credentials Secret. It only reads; it never writes to CNPG objects or Secrets.
type DatabaseReconciler struct {
	client.Client
	// APIReader reads Secrets directly from the API server, so Secret data is never
	// cached by the operator.
	APIReader client.Reader
	Recorder  recorder.EventRecorder
	// Now returns the current time; tests replace it.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=zabbix.io,resources=zabbixdatabases,verbs=get;list;watch
// +kubebuilder:rbac:groups=zabbix.io,resources=zabbixdatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile recomputes the status of one ZabbixDatabase.
func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	db := &zabbixv1alpha1.ZabbixDatabase{}
	if err := r.Get(ctx, req.NamespacedName, db); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	err := r.Get(ctx, types.NamespacedName{Namespace: db.Namespace, Name: db.Spec.ClusterRef.Name}, cluster)
	switch {
	case apierrors.IsNotFound(err):
		cluster = nil
	case err != nil:
		return ctrl.Result{}, err
	}

	var secretData map[string][]byte
	secret := &corev1.Secret{}
	err = r.APIReader.Get(ctx, types.NamespacedName{Namespace: db.Namespace, Name: db.Spec.CredentialsRef.SecretName}, secret)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return ctrl.Result{}, err
	default:
		secretData = secret.Data
		if secretData == nil {
			secretData = map[string][]byte{}
		}
	}

	status, requeue := computeDatabaseStatus(db, parseCNPGCluster(cluster), inspectCredentials(db, secretData), r.now())
	if !equality.Semantic.DeepEqual(status, db.Status) {
		r.recordReadyTransition(db, status)
		db.Status = status
		if err := r.Status().Update(ctx, db); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *DatabaseReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// recordReadyTransition emits an event when Ready changes.
func (r *DatabaseReconciler) recordReadyTransition(db *zabbixv1alpha1.ZabbixDatabase, next zabbixv1alpha1.ZabbixDatabaseStatus) {
	prev := meta.FindStatusCondition(db.Status.Conditions, zabbixv1alpha1.DatabaseReady)
	cur := meta.FindStatusCondition(next.Conditions, zabbixv1alpha1.DatabaseReady)
	if cur == nil || (prev != nil && prev.Status == cur.Status) {
		return
	}
	if cur.Status == metav1.ConditionTrue {
		r.Recorder.Eventf(db, nil, corev1.EventTypeNormal, cur.Reason, "Evaluate", "%s", cur.Message)
		return
	}
	r.Recorder.Eventf(db, nil, corev1.EventTypeWarning, cur.Reason, "Evaluate", "%s", cur.Message)
}

// SetupWithManager registers the controller, its field indexes and its watches.
func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	idx := mgr.GetFieldIndexer()
	if err := idx.IndexField(ctx, &zabbixv1alpha1.ZabbixDatabase{}, indexClusterRef, func(o client.Object) []string {
		return []string{o.(*zabbixv1alpha1.ZabbixDatabase).Spec.ClusterRef.Name}
	}); err != nil {
		return err
	}
	if err := idx.IndexField(ctx, &zabbixv1alpha1.ZabbixDatabase{}, indexSecretRef, func(o client.Object) []string {
		return []string{o.(*zabbixv1alpha1.ZabbixDatabase).Spec.CredentialsRef.SecretName}
	}); err != nil {
		return err
	}

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)

	return ctrl.NewControllerManagedBy(mgr).
		Named("zabbixdatabase").
		For(&zabbixv1alpha1.ZabbixDatabase{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(cluster, handler.EnqueueRequestsFromMapFunc(r.byIndex(indexClusterRef))).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.byIndex(indexSecretRef)),
			builder.OnlyMetadata).
		Complete(r)
}

// byIndex maps an object to the ZabbixDatabases in its namespace whose indexed field
// equals the object's name.
func (r *DatabaseReconciler) byIndex(field string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		list := &zabbixv1alpha1.ZabbixDatabaseList{}
		if err := r.List(ctx, list, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{field: o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "listing ZabbixDatabases", "field", field)
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for _, db := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&db)})
		}
		return reqs
	}
}
