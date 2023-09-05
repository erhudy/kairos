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

package main

import (
	"flag"

	"k8s.io/klog/v2"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	var kubeconfig string
	var master string
	var namespace string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.StringVar(&namespace, "namespace", "", "namespace")
	flag.Parse()

	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	workchan := make(chan ObjectAndSchedulerAction)

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	deploymentController := generateDeploymentController(clientset, namespace, workchan)
	statefulSetController := generateStatefulSetController(clientset, namespace, workchan)
	daemonSetController := generateDaemonSetController(clientset, namespace, workchan)

	scheduler := NewScheduler(workchan, clientset)

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
