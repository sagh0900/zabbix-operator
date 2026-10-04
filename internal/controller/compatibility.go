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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sagh0900/zabbix-operator/internal/metrics"
	"github.com/sagh0900/zabbix-operator/internal/zabbix"
)

// CompatibilityKey is the ConfigMap key holding the compatibility configuration.
const CompatibilityKey = "compatibility.yaml"

// CompatibilitySource provides the release lines the operator runs: the built-in lines,
// plus lines added or changed by an optional ConfigMap in the operator's namespace. An
// invalid ConfigMap is reported and ignored, so the built-in lines always apply.
type CompatibilitySource struct {
	// Reader reads the ConfigMap directly from the API server.
	Reader client.Reader
	// Namespace and Name locate the ConfigMap; an empty Name disables it.
	Namespace, Name string
	// TTL is how long a read is reused; changes apply within it.
	TTL time.Duration

	mu     sync.Mutex
	cached *zabbix.Compatibility
	at     time.Time
}

// Get returns the current compatibility.
func (s *CompatibilitySource) Get(ctx context.Context) *zabbix.Compatibility {
	if s == nil || s.Name == "" {
		return zabbix.Builtin()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && time.Since(s.at) < s.TTL {
		return s.cached
	}
	s.cached, s.at = s.read(ctx), time.Now()
	return s.cached
}

func (s *CompatibilitySource) read(ctx context.Context) *zabbix.Compatibility {
	valid := func(ok bool) {
		v := 0.0
		if ok {
			v = 1
		}
		metrics.CompatibilityConfigValid.Set(v)
	}
	cm := &corev1.ConfigMap{}
	err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: s.Name}, cm)
	switch {
	case apierrors.IsNotFound(err):
		valid(true)
		return zabbix.Builtin()
	case err != nil:
		log.FromContext(ctx).Error(err, "reading the compatibility ConfigMap; using the built-in lines", "configMap", s.Name)
		valid(false)
		return zabbix.Builtin()
	}
	c, err := zabbix.ParseCompatibility([]byte(cm.Data[CompatibilityKey]))
	if err != nil {
		log.FromContext(ctx).Error(err, "invalid compatibility ConfigMap; using the built-in lines", "configMap", s.Name)
		valid(false)
		return zabbix.Builtin()
	}
	valid(true)
	return c
}

// unverifiedLines describes the release lines in use, the target's and the running one's,
// that come from the compatibility ConfigMap instead of the lines validated with this
// operator version; empty when there are none.
func unverifiedLines(c *zabbix.Compatibility, src *CompatibilitySource, target zabbix.Version, running string) string {
	var lines []string
	versions := []zabbix.Version{target}
	if v, err := zabbix.ParseVersion(running); err == nil && v.Line() != target.Line() {
		versions = append(versions, v)
	}
	for _, v := range versions {
		if l, ok := c.Lookup(v); ok && !l.Verified {
			lines = append(lines, l.Line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	where := "the compatibility ConfigMap"
	if src != nil {
		where = "ConfigMap " + src.Namespace + "/" + src.Name
	}
	return fmt.Sprintf("Zabbix %s comes from %s and is not validated with this operator version; take a full backup before upgrading",
		strings.Join(lines, " and "), where)
}
