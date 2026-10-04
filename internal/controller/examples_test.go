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
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/sagh0900/zabbix-operator/internal/registration"
)

// Every manifest in examples/ is accepted by the API server with strict field validation,
// and proxy lists in ConfigMaps parse.
func TestExamples(t *testing.T) {
	requireEnvtest(t)
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	ctx := context.Background()
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
			for {
				obj := &unstructured.Unstructured{}
				if err := dec.Decode(&obj.Object); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if len(obj.Object) == 0 {
					continue
				}
				if obj.GetKind() == "Namespace" {
					if err := k8s.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
						t.Fatal(err)
					}
					continue
				}
				if obj.GetNamespace() == "" {
					t.Errorf("%s %s has no namespace", obj.GetKind(), obj.GetName())
				}
				ensureNamespace(t, obj.GetNamespace())
				if err := k8s.Create(ctx, obj, client.DryRunAll, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
					t.Errorf("%s %s: %v", obj.GetKind(), obj.GetName(), err)
				}
				if obj.GetKind() == "ConfigMap" {
					if list, ok, _ := unstructured.NestedString(obj.Object, "data", "proxies.yaml"); ok {
						if _, err := registration.Parse([]byte(list)); err != nil {
							t.Errorf("ConfigMap %s: %v", obj.GetName(), err)
						}
					}
				}
			}
		})
	}
}

func ensureNamespace(t *testing.T, name string) {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(name)
	if err := k8s.Create(context.Background(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}
