/*
Copyright 2017 The Kubernetes Authors.

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

// derived from https://github.com/kubernetes/client-go/blob/master/examples/workqueue/main.go

package main

import (
	"flag"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kairosv1alpha1 "github.com/erhudy/kairos/api/v1alpha1"
	"github.com/erhudy/kairos/pkg"
	"k8s.io/client-go/kubernetes"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kairosv1alpha1.AddToScheme(scheme))
}

func main() {
	var debug bool
	var kubeconfig string
	var master string
	var namespace string
	var tzstring string
	var metricsAddr string

	flag.BoolVar(&debug, "debug", false, "debug mode")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.StringVar(&namespace, "namespace", "", "namespace")
	flag.StringVar(&tzstring, "timezone", "Local", "timezone that the scheduler should consider the system clock to be")
	flag.StringVar(&metricsAddr, "metrics-addr", ":9090", "address to serve Prometheus metrics on")
	flag.Parse()

	var logger *zap.Logger
	if debug {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}
	defer func() { _ = logger.Sync() }()

	// Set controller-runtime logger
	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseDevMode(debug)))

	timezone, err := time.LoadLocation(tzstring)
	if err != nil {
		logger.Fatal("unable to process given timezone", zap.String("tz", tzstring), zap.Error(err))
	}

	logger.Info("operating with timezone", zap.String("tz", timezone.String()))
	logger.Info("current time", zap.String("given", time.Now().In(timezone).String()), zap.String("utc", time.Now().UTC().String()))

	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		logger.Fatal("unable to build Kubernetes client config", zap.Error(err))
	}

	workchan := make(chan pkg.ObjectAndSchedulerAction, 10)

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Fatal("unable to build Kubernetes clientset", zap.Error(err))
	}

	// set up metrics
	registry := prometheus.NewRegistry()
	metrics := pkg.NewKairosMetrics()
	metrics.Register(registry)

	deploymentController := pkg.GenerateDeploymentController(logger, clientset, namespace, workchan, metrics)
	statefulSetController := pkg.GenerateStatefulSetController(logger, clientset, namespace, workchan, metrics)
	daemonSetController := pkg.GenerateDaemonSetController(logger, clientset, namespace, workchan, metrics)

	scheduler := pkg.NewScheduler(timezone, logger, workchan, clientset, metrics)

	// Create controller-runtime manager (disable its built-in metrics server since we have our own)
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0", // disable controller-runtime metrics server
		},
	})
	if err != nil {
		logger.Fatal("unable to create controller-runtime manager", zap.Error(err))
	}

	// Create and register chain reconciler
	chainReconciler := pkg.NewChainReconciler(mgr.GetClient(), clientset, logger, timezone, metrics)
	if err := chainReconciler.SetupWithManager(mgr); err != nil {
		logger.Fatal("unable to set up chain reconciler", zap.Error(err))
	}

	// Register chain reconciler's cron scheduler as a runnable so it starts/stops with the manager
	if err := mgr.Add(chainReconciler); err != nil {
		logger.Fatal("unable to add chain reconciler runnable", zap.Error(err))
	}

	// start HTTP server (metrics + web UI)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/api/jobs", scheduler.JobStatusJSON)
	mux.HandleFunc("/api/chains", chainReconciler.ChainStatusJSON)
	mux.HandleFunc("/", scheduler.JobStatusPage)
	go func() {
		logger.Info("starting HTTP server", zap.String("addr", metricsAddr))
		if err := http.ListenAndServe(metricsAddr, mux); err != nil {
			logger.Fatal("HTTP server failed", zap.Error(err))
		}
	}()

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go deploymentController.Run(1, stop)
	go statefulSetController.Run(1, stop)
	go daemonSetController.Run(1, stop)
	go scheduler.Run(stop)

	// Start controller-runtime manager (blocks)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Fatal("controller-runtime manager exited with error", zap.Error(err))
	}
}
