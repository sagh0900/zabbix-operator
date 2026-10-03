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

package podset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
)

// TemplateHash returns a short, stable hash of the parts of a pod template that require
// replacing the pod when they change: labels, annotations and spec. The pod's own
// template-hash annotation is excluded. The API server's defaulting does not affect the
// hash because it is computed from the desired template, never from a live pod.
func TemplateHash(p *corev1.Pod) string {
	annotations := map[string]string{}
	for k, v := range p.Annotations {
		if k != AnnotationTemplateHash {
			annotations[k] = v
		}
	}
	data, err := json.Marshal(struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Spec        corev1.PodSpec    `json:"spec"`
	}{p.Labels, annotations, p.Spec})
	if err != nil {
		// PodSpec always marshals; a failure here is a programming error.
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
