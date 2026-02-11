package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"antrea-test-task/pkg/controller"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	flag.Parse()

	var config *rest.Config
	var err error

	if *kubeconfig != "" {
		fmt.Printf("Using kubeconfig file: %s\n", *kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		fmt.Println("Using in-cluster configuration")
		config, err = rest.InClusterConfig()
		if err != nil {
			fmt.Println("In-cluster config failed, trying default local config...")
			kubeconfigPath := os.Getenv("HOME") + "/.kube/config"
			config, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		}
	}

	if err != nil {
		fmt.Printf("Error building kubeconfig: %s\n", err.Error())
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("Error building clientset: %s\n", err.Error())
		os.Exit(1)
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		fmt.Println("WARNING: NODE_NAME environment variable is not set.")
		fmt.Println("If running locally, this is fine (watching all pods).")
		fmt.Println("If running in cluster, check your DaemonSet YAML.")
	} else {
		fmt.Printf("Running on node: %s. Filtering events for this node only.\n", nodeName)
	}

	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		clientset,
		time.Minute*10,
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			if nodeName != "" {
				// Only send the pods scheduled on this node
				options.FieldSelector = "spec.nodeName=" + nodeName
			}
		}),
	)

	ctrl := controller.NewController(clientset, informerFactory.Core().V1().Pods())

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh // Block until a signal is received
		fmt.Println("\nReceived termination signal. Shutting down gracefully...")
		close(stopCh)
	}()

	fmt.Println("Starting Informer Factory...")
	informerFactory.Start(stopCh)

	fmt.Println("Starting Controller...")

	if err := ctrl.Run(1, stopCh); err != nil {
		fmt.Printf("Error running controller: %s\n", err.Error())
		os.Exit(1)
	}
}
