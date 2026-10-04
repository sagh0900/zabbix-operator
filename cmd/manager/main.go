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

// Command manager runs the Zabbix operator.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	zabbixv1alpha1 "github.com/sagh0900/zabbix-operator/api/v1alpha1"
	"github.com/sagh0900/zabbix-operator/internal/controller"
	"github.com/sagh0900/zabbix-operator/internal/jobs"
	"github.com/sagh0900/zabbix-operator/internal/version"
)

// The metrics endpoint authenticates scrapers with TokenReview and authorises them with
// SubjectAccessReview.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(zabbixv1alpha1.AddToScheme(scheme))
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "job" {
		os.Exit(jobs.Main(os.Args[2:]))
	}

	var (
		metricsAddr            string
		metricsSecure          bool
		probeAddr              string
		leaderElection         bool
		showVersion            bool
		compatibilityConfigMap string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8443", "Address the metrics endpoint binds to; 0 disables it.")
	flag.BoolVar(&metricsSecure, "metrics-secure", true,
		"Serve metrics over HTTPS and require an authorised Kubernetes token.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health probe endpoint binds to.")
	flag.BoolVar(&leaderElection, "leader-elect", true, "Enable leader election so only one manager is active.")
	flag.BoolVar(&showVersion, "version", false, "Print the version and exit.")
	flag.StringVar(&compatibilityConfigMap, "compatibility-configmap", "zabbix-operator-compatibility",
		"ConfigMap in the operator's namespace (POD_NAMESPACE) that adds Zabbix release lines; empty disables it.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if showVersion {
		fmt.Println("zabbix-operator", version.String())
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")
	log.Info("starting", "version", version.String())

	metricsOpts := metricsserver.Options{BindAddress: metricsAddr, SecureServing: metricsSecure}
	if metricsSecure {
		metricsOpts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsOpts,
		HealthProbeBindAddress:        probeAddr,
		LeaderElection:                leaderElection,
		LeaderElectionID:              "zabbix-operator-leader",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.DatabaseReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder("zabbix-operator"),
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up controller", "controller", "ZabbixDatabase")
		os.Exit(1)
	}

	operatorImage := os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		log.Error(nil, "OPERATOR_IMAGE must name the operator image; database Jobs run it")
		os.Exit(1)
	}
	if err := (&controller.SystemReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Recorder:      mgr.GetEventRecorder("zabbix-operator"),
		OperatorImage: operatorImage,
		Compatibility: &controller.CompatibilitySource{
			Reader: mgr.GetAPIReader(), Namespace: os.Getenv("POD_NAMESPACE"),
			Name: compatibilityConfigMap, TTL: 30 * time.Second,
		},
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up controller", "controller", "ZabbixSystem")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		os.Exit(1)
	}
}
