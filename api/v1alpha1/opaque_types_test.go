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

package v1alpha1

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

func TestOpaqueTypesRoundTrip(t *testing.T) {
	in := PodSettings{
		ExtraContainers: []Container{ContainerOf(corev1.Container{
			Name: "agent", Image: "zabbix/zabbix-agent2", Args: []string{"-f"},
			Env: []corev1.EnvVar{{Name: "ZBX_HOSTNAME", Value: "Zabbix server"}},
		})},
		Volumes: []Volume{VolumeOf(corev1.Volume{Name: "conf", VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cm"}}}})},
		TopologySpreadConstraints: []TopologySpreadConstraint{TopologySpreadConstraintOf(corev1.TopologySpreadConstraint{
			MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", WhenUnsatisfiable: corev1.DoNotSchedule})},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out PodSettings
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !equality.Semantic.DeepEqual(in.ExtraContainers[0].Get(), out.ExtraContainers[0].Get()) ||
		!equality.Semantic.DeepEqual(in.Volumes[0].Get(), out.Volumes[0].Get()) ||
		!equality.Semantic.DeepEqual(in.TopologySpreadConstraints[0].Get(), out.TopologySpreadConstraints[0].Get()) {
		t.Fatalf("round trip changed values: %s", b)
	}

	// DeepCopy must not share memory with the original.
	cp := in.DeepCopy()
	cp.ExtraContainers[0].v.Env[0].Value = "changed"
	if in.ExtraContainers[0].Get().Env[0].Value != "Zabbix server" {
		t.Fatal("DeepCopy shares memory with the original")
	}
}
