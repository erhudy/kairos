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
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/erhudy/kairos/pkg"
)

func main() {
	var debug bool
	var kubeconfig string
	var master string
	var namespace string
	var tzstring string
	var metricsAddr string
	var maxJitter time.Duration
	var lookback time.Duration
	var chainTimeout time.Duration

	flag.BoolVar(&debug, "debug", false, "debug mode")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.StringVar(&namespace, "namespace", "", "namespace")
	flag.StringVar(&tzstring, "timezone", "Local", "timezone that the scheduler should consider the system clock to be")
	flag.StringVar(&metricsAddr, "metrics-addr", ":9090", "address to serve Prometheus metrics on")
	flag.DurationVar(&maxJitter, "jitter", 0, "maximum random jitter to add before each restart (e.g. 15m); 0 disables jitter")
	flag.DurationVar(&lookback, "lookback", 0, "how far back to check for missed restarts on startup (e.g. 30m); 0 disables")
	flag.DurationVar(&chainTimeout, "chain-timeout", 10*time.Minute, "how long a chained restart waits for its predecessor to become healthy before aborting the cascade (e.g. 30m)")
	flag.Parse()

	var logger *zap.Logger
	if debug {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}
	defer func() { _ = logger.Sync() }()

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

	scheduler := pkg.NewScheduler(timezone, logger, workchan, clientset, metrics, maxJitter, lookback, chainTimeout)

	// listen synchronously so a bind failure fails fast before other components start;
	// logger.Fatal here is safe because it runs on the main goroutine
	listener, err := net.Listen("tcp", metricsAddr)
	if err != nil {
		logger.Fatal("unable to listen on metrics address", zap.String("addr", metricsAddr), zap.Error(err))
	}

	// start HTTP server (metrics + web UI)
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/api/jobs", scheduler.JobStatusJSON)
	mux.HandleFunc("/api/config", scheduler.ConfigJSON)
	mux.HandleFunc("/", scheduler.JobStatusPage)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("starting HTTP server", zap.String("addr", metricsAddr))
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.

	// Now let's start the controller
	stop := make(chan struct{})
	go deploymentController.Run(1, stop)
	go statefulSetController.Run(1, stop)
	go daemonSetController.Run(1, stop)

	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		scheduler.Run(stop)
	}()

	// wait for SIGINT/SIGTERM (or an HTTP server failure), then shut everything down
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	httpFailed := false
	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", zap.String("signal", sig.String()))
	case err := <-serverErr:
		logger.Error("HTTP server failed, initiating shutdown", zap.Error(err))
		httpFailed = true
	}
	close(stop)

	// stop accepting new HTTP requests, giving in-flight ones a bounded grace period
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("error shutting down HTTP server", zap.Error(err))
	}

	// wait for the scheduler to finish stopping (it wakes any in-flight jitter
	// sleeps and then waits for gocron to stop) before exiting
	<-schedulerDone
	logger.Info("shutdown complete")
	if httpFailed {
		_ = logger.Sync()
		os.Exit(1)
	}
}
