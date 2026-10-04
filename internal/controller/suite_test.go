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
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/registration"
	"github.com/sagh0900/zabbix-operator/internal/registration/fakeapi"
)

// k8s is the client for envtest-based tests; nil when envtest assets are unavailable.
var k8s client.Client

// testOperatorImage is the image database Jobs run in tests.
const testOperatorImage = "zabbix-operator:test"

// fakeActive plays Zabbix HA in tests: it holds which server pods ("namespace/name")
// accept trapper connections.
var fakeActive sync.Map

// fakeAPIs plays the Zabbix API in tests: URL -> *fakeapi.API. Unknown URLs and tokens
// other than testAPIToken get an API whose every call fails.
var fakeAPIs sync.Map

const testAPIToken = "test-token"

func fakeZabbixAPI(url, token string) registration.API {
	if v, ok := fakeAPIs.Load(url); ok && token == testAPIToken {
		return v.(*fakeapi.API)
	}
	down := fakeapi.New()
	for _, m := range []string{"proxy.get", "proxygroup.get"} {
		down.SetFail(m, true)
	}
	return down
}

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS not set: envtest tests are skipped (run make test)")
		os.Exit(m.Run())
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr), zap.Level(zapLevelFromEnv())))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases"), filepath.Join("..", "..", "test", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(zabbixv1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating manager:", err)
		os.Exit(1)
	}
	if err := (&DatabaseReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder("zabbix-operator"),
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintln(os.Stderr, "setting up controller:", err)
		os.Exit(1)
	}

	if err := (&SystemReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorder("zabbix-operator"),
		OperatorImage: testOperatorImage,
		ActiveProbe: func(_ context.Context, p *corev1.Pod) bool {
			v, ok := fakeActive.Load(p.Namespace + "/" + p.Name)
			return ok && v.(bool)
		},
		ZabbixAPI: fakeZabbixAPI,
	}).SetupWithManager(mgr); err != nil {
		fmt.Fprintln(os.Stderr, "setting up system controller:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "manager:", err)
		}
	}()
	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "creating client:", err)
		os.Exit(1)
	}

	code := m.Run()
	cancel()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stopping envtest:", err)
	}
	os.Exit(code)
}

// zapLevelFromEnv keeps controller logs quiet unless TEST_VERBOSE is set.
func zapLevelFromEnv() zapcore.Level {
	if os.Getenv("TEST_VERBOSE") != "" {
		return zapcore.DebugLevel
	}
	return zapcore.ErrorLevel
}

// requireEnvtest skips t when envtest is not running.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8s == nil {
		t.Skip("envtest not available")
	}
}

// newNamespace creates a namespace unique to t.
func newNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-"}}
	if err := k8s.Create(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

// waitFor bounds how long envtest assertions wait for the controller.
const waitFor = 10 * time.Second

// eventually polls check until it returns nil or waitFor expires.
func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %v", waitFor, err)
}
