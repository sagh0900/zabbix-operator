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
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/metrics"
	"github.com/sagh0900/zabbix-operator/internal/registration"
	"github.com/sagh0900/zabbix-operator/internal/system"
	"github.com/sagh0900/zabbix-operator/internal/zabbixapi"
)

const (
	// proxySyncInterval is how often registrations are re-checked when nothing changed,
	// which repairs changes made by hand in Zabbix; proxySyncRetry is the delay after a
	// failed sync.
	proxySyncInterval = 5 * time.Minute
	proxySyncRetry    = 30 * time.Second
)

// proxySync remembers the last registration sync of a system and its outcome, which is
// reported again until the next sync so a lost status update does not lose it.
type proxySync struct {
	hash   string
	at     time.Time
	result *registrationResult
}

// registrationResult is what a pass learned about proxy registration; nil fields leave
// the status unchanged.
type registrationResult struct {
	condition  *metav1.Condition
	registered []string
	clear      bool
}

// reconcileRegistration keeps the desired proxies registered in Zabbix. It syncs when the
// desired state changes, every proxySyncInterval, and proxySyncRetry after a failure.
func (r *SystemReconciler) reconcileRegistration(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem, obs *observation) {
	spec := sys.Spec.ProxyRegistration
	key := sys.Namespace + "/" + sys.Name
	if !spec.Enabled {
		r.proxySyncs.Delete(key)
		metrics.DeleteRegistration(sys.Namespace, sys.Name)
		obs.registration = &registrationResult{clear: true}
		return
	}
	fail := func(reason, msg string) {
		obs.registration = &registrationResult{condition: &metav1.Condition{Type: zabbixv1alpha1.SystemProxiesRegistered,
			Status: metav1.ConditionFalse, Reason: reason, Message: msg}}
		metrics.ProxyRegistrationFailing.WithLabelValues(sys.Namespace, sys.Name).Set(1)
	}

	desired, token, hash, err := r.registrationInput(ctx, sys)
	if err != nil {
		fail("InvalidConfiguration", err.Error())
		return
	}
	url := spec.URL
	if url == "" {
		if !sys.Spec.Web.IsEnabled() {
			fail("NoFrontend", "the frontend is disabled; set proxyRegistration.url")
			return
		}
		if !obs.webReady {
			return
		}
		url = defaultAPIURL(sys)
	}

	now := r.now()
	if v, ok := r.proxySyncs.Load(key); ok {
		last := v.(proxySync)
		wait := proxySyncInterval
		if last.result.condition.Status != metav1.ConditionTrue {
			wait = proxySyncRetry
		}
		if last.hash == hash && now.Sub(last.at) < wait {
			obs.registration = last.result
			return
		}
	}

	newAPI := r.ZabbixAPI
	if newAPI == nil {
		newAPI = func(url, token string) registration.API { return zabbixapi.New(url, token) }
	}
	res, err := registration.Sync(ctx, newAPI(url, token), desired, sys.Status.RegisteredProxies, spec.Prune,
		fmt.Sprintf("Created by ZabbixSystem %s/%s", sys.Namespace, sys.Name))
	ns, name := sys.Namespace, sys.Name
	metrics.ProxiesRegistered.WithLabelValues(ns, name).Set(float64(len(res.Registered)))
	if err != nil {
		metrics.ProxyRegistrationSyncs.WithLabelValues(ns, name, "failed").Inc()
		fail("SyncFailed", err.Error())
		obs.registration.registered = res.Registered
		r.proxySyncs.Store(key, proxySync{hash: hash, at: now, result: obs.registration})
		// Failed syncs are retried every proxySyncRetry; the recorder aggregates repeats.
		r.event(sys, corev1.EventTypeWarning, "ProxyRegistrationFailed", err.Error())
		return
	}
	metrics.ProxyRegistrationSyncs.WithLabelValues(ns, name, "succeeded").Inc()
	metrics.ProxyRegistrationFailing.WithLabelValues(ns, name).Set(0)
	if n := res.Created + res.Updated + res.Deleted; n > 0 {
		r.event(sys, corev1.EventTypeNormal, "ProxiesRegistered",
			fmt.Sprintf("created %d, updated %d, pruned %d proxies", res.Created, res.Updated, res.Deleted))
	}
	obs.registration = &registrationResult{registered: res.Registered, condition: &metav1.Condition{
		Type: zabbixv1alpha1.SystemProxiesRegistered, Status: metav1.ConditionTrue, Reason: "Synced",
		Message: fmt.Sprintf("%d proxies registered", len(desired))}}
	r.proxySyncs.Store(key, proxySync{hash: hash, at: now, result: obs.registration})
}

// registrationInput reads the proxy list and the API token and hashes everything a sync
// depends on.
func (r *SystemReconciler) registrationInput(ctx context.Context, sys *zabbixv1alpha1.ZabbixSystem) ([]registration.Entry, string, string, error) {
	spec := sys.Spec.ProxyRegistration
	var listed []registration.Entry
	if ref := spec.ConfigMapRef; ref != nil {
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: ref.Name}, cm); err != nil {
			return nil, "", "", fmt.Errorf("reading ConfigMap %s: %w", ref.Name, err)
		}
		data, ok := cm.Data[ref.Key]
		if !ok {
			return nil, "", "", fmt.Errorf("key %s missing in ConfigMap %s", ref.Key, ref.Name)
		}
		var err error
		if listed, err = registration.Parse([]byte(data)); err != nil {
			return nil, "", "", fmt.Errorf("proxy list in ConfigMap %s: %w", ref.Name, err)
		}
	}
	desired, err := registration.Desired(sys, listed)
	if err != nil {
		return nil, "", "", err
	}
	ref := spec.APITokenSecretRef
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sys.Namespace, Name: ref.Name}, secret); err != nil {
		return nil, "", "", fmt.Errorf("reading Secret %s: %w", ref.Name, err)
	}
	token := strings.TrimSpace(string(secret.Data[ref.Key]))
	if token == "" {
		return nil, "", "", fmt.Errorf("key %s missing in Secret %s", ref.Key, ref.Name)
	}
	h := sha256.New()
	_ = json.NewEncoder(h).Encode([]any{desired, spec.Prune, spec.URL, string(secret.UID), secret.ResourceVersion})
	return desired, token, hex.EncodeToString(h.Sum(nil)), nil
}

// defaultAPIURL is the API endpoint of the system's own frontend Service.
func defaultAPIURL(sys *zabbixv1alpha1.ZabbixSystem) string {
	port := sys.Spec.Web.Service.Port
	if port == 0 {
		port = 80
	}
	return fmt.Sprintf("http://%s.%s.svc:%d/api_jsonrpc.php", system.ServiceName(sys, system.Web), sys.Namespace, port)
}
