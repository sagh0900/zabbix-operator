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

// Package envmerge merges operator-managed and user-supplied container environment variables.
package envmerge

import (
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var mergeLog = ctrl.Log.WithName("envmerge")

// Merge merges operatorEnv (defaults) with userEnv (overrides), respecting
// protectedKeys which the operator always controls regardless of user input.
//
// Merge semantics:
//  1. Start with all operator defaults.
//  2. For each user-supplied var:
//     - If key is in protectedKeys: emit a warning and skip (operator value wins).
//     - Otherwise: user value overrides operator default.
//  3. Any user vars not in operatorEnv are appended.
//  4. After the merge, re-apply all protectedKeys from operatorEnv (final pass).
//
// The result order is deterministic: operator keys first, then new user keys.
func Merge(operatorEnv, userEnv []corev1.EnvVar, protectedKeys []string) []corev1.EnvVar {
	// Build a set of protected keys for O(1) lookup.
	protected := make(map[string]struct{}, len(protectedKeys))
	for _, k := range protectedKeys {
		protected[k] = struct{}{}
	}

	// Index operator env by Name.
	opMap := make(map[string]corev1.EnvVar, len(operatorEnv))
	opOrder := make([]string, 0, len(operatorEnv))
	for _, e := range operatorEnv {
		opMap[e.Name] = e
		opOrder = append(opOrder, e.Name)
	}

	// Index user env by Name.
	userMap := make(map[string]corev1.EnvVar, len(userEnv))
	userOrder := make([]string, 0, len(userEnv))
	for _, e := range userEnv {
		userMap[e.Name] = e
		userOrder = append(userOrder, e.Name)
	}

	// merged holds the result, keyed by Name.
	merged := make(map[string]corev1.EnvVar, len(opMap)+len(userMap))
	mergedOrder := make([]string, 0, len(opMap)+len(userMap))

	// Start with operator defaults.
	for _, k := range opOrder {
		merged[k] = opMap[k]
		mergedOrder = append(mergedOrder, k)
	}

	// Apply user overrides. Protected keys are skipped with a warning.
	for _, k := range userOrder {
		uv := userMap[k]
		if _, isProtected := protected[k]; isProtected {
			mergeLog.Info(
				"user attempted to override a protected env key — operator value retained",
				"key", k,
			)
			continue
		}
		if _, exists := merged[k]; !exists {
			// New key introduced by the user — append to order.
			mergedOrder = append(mergedOrder, k)
		}
		merged[k] = uv
	}

	// Final pass: re-apply all protected keys from the operator (ensures operator wins).
	for _, k := range protectedKeys {
		if ov, ok := opMap[k]; ok {
			merged[k] = ov
		}
	}

	// Build the result slice in deterministic order.
	result := make([]corev1.EnvVar, 0, len(mergedOrder))
	seen := make(map[string]struct{}, len(mergedOrder))
	for _, k := range mergedOrder {
		if _, already := seen[k]; already {
			continue
		}
		seen[k] = struct{}{}
		if v, ok := merged[k]; ok {
			result = append(result, v)
		}
	}

	return result
}
