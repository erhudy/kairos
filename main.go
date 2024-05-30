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
	"os"
	"os/signal"
	"syscall"
	"time"

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

	flag.BoolVar(&debug, "debug", false, "debug mode")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.StringVar(&namespace, "namespace", "", "namespace")
	flag.StringVar(&tzstring, "timezone", "Local", "timezone that the scheduler should consider the system clock to be")
	flag.Parse()

	var logger *zap.Logger
	if debug {
		logger, _ = zap.NewDevelopment()
	} else {
		logger, _ = zap.NewProduction()
	}
	defer logger.Sync()

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

	deploymentController := pkg.GenerateDeploymentController(logger, clientset, namespace, workchan)
	statefulSetController := pkg.GenerateStatefulSetController(logger, clientset, namespace, workchan)
	daemonSetController := pkg.GenerateDaemonSetController(logger, clientset, namespace, workchan)

	scheduler := pkg.NewScheduler(timezone, logger, workchan, clientset)

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGUSR1)
	go func() {
		for range signalChan {
			scheduler.ShowJobStatus()
		}
	}()

	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.

	// Now let's start the controller
	stop := make(chan struct{})
	defer close(stop)
	go deploymentController.Run(1, stop)
	go statefulSetController.Run(1, stop)
	go daemonSetController.Run(1, stop)
	go scheduler.Run(stop)

	// Wait forever
	select {}
}
