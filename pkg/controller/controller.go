package controller

import (
	"fmt"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	utilRuntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreInformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	coreListers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	ControllerName = "pcap-controller"
	AnnotationKey  = "tcpdump.antrea.io"
)

type CaptureState struct {
	PID             int
	AnnotationValue string
	PodName         string
	PodNamespace    string
}

type Controller struct {
	// clientSet allows us to talk to the API server as it has the information about the pods
	clientSet kubernetes.Interface

	// podsLister is the local cache of Pods so we don't hit the API server constantly
	podsLister coreListers.PodLister
	// podsSynced tells us if the cache is ready
	podsSynced cache.InformerSynced

	// workQueue is a rate-limited queue. When a Pod changes, we add its key here.
	// This ensures we process events one by one and handle failures gracefully.
	workQueue workqueue.RateLimitingInterface

	// activeCaptures maps a Pod UID to the PID of the running tcpdump process.
	// We use this to know if we need to start or stop a capture.
	activeCaptures map[string]CaptureState
}

// this function is a constructor
func NewController(clientSet kubernetes.Interface, podInformer coreInformers.PodInformer) *Controller {
	c := &Controller{
		clientSet:      clientSet,
		podsLister:     podInformer.Lister(),
		podsSynced:     podInformer.Informer().HasSynced,
		workQueue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName),
		activeCaptures: make(map[string]CaptureState),
	}

	fmt.Println("Setting up event handlers...")

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		// Registered the trigger when a pod is added/updated/deleted.
		AddFunc: c.enqueuePod,
		UpdateFunc: func(old, new interface{}) {
			c.enqueuePod(new)
		},
		DeleteFunc: c.enqueuePod,
	})

	return c
}

func (c *Controller) Run(threads int, stopCh <-chan struct{}) error {
	defer utilRuntime.HandleCrash()
	// Ensures that if the controller crashes, the queue stops accepting new work.
	defer c.workQueue.ShutDown()

	fmt.Println("Starting Pcap Controller")

	// Wait for the caches to be synced before starting threads
	fmt.Println("Waiting for informer caches to sync...")
	if ok := cache.WaitForCacheSync(stopCh, c.podsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	fmt.Println("Starting threads...")
	// launching worker threads
	for i := 0; i < threads; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	fmt.Println("Controller ready and running")
	<-stopCh
	fmt.Println("Shutting down threads")

	return nil
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *Controller) processNextItem() bool {
	obj, shutdown := c.workQueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workQueue.Done
	err := func(obj interface{}) error {
		defer c.workQueue.Done(obj)
		var key string
		var ok bool

		if key, ok = obj.(string); !ok {
			c.workQueue.Forget(obj)
			utilRuntime.HandleError(fmt.Errorf("expected string in workQueue but got %#v", obj))
			return nil
		}

		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workQueue to handle any transient errors.
			c.workQueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}

		c.workQueue.Forget(obj)
		fmt.Printf("Successfully synced '%s'\n", key)
		return nil
	}(obj)

	if err != nil {
		utilRuntime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) enqueuePod(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilRuntime.HandleError(err)
		return
	}
	c.workQueue.Add(key)
}

func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilRuntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	pod, err := c.podsLister.Pods(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Printf("Pod '%s' deleted. Checking for orphaned captures...\n", key)

			for uid, state := range c.activeCaptures {
				if state.PodName == name && state.PodNamespace == namespace {
					fmt.Printf("Stopping orphaned capture for deleted pod %s (PID: %d)\n", state.PodName, state.PID)
					_ = c.stopCapture(state.PID, state.PodName)
					delete(c.activeCaptures, uid)
				}
			}
			return nil
		}
		return err
	}

	// Check for the annotation
	annotationValue, annotationExists := pod.Annotations[AnnotationKey]

	state, isActive := c.activeCaptures[string(pod.UID)]

	if annotationExists {

		if _, err := strconv.Atoi(annotationValue); err != nil {
			utilRuntime.HandleError(fmt.Errorf("invalid capture limit '%s': %v", annotationValue, err))
			return nil
		}
		// SCENARIO 1: User wants capture (Annotation Present)

		if isActive {
			// Sub-case: Capture is running, but did the user change the value? (e.g. "5" -> "10")
			if state.AnnotationValue != annotationValue {
				fmt.Printf("Updating capture for pod %s: %s -> %s\n", pod.Name, state.AnnotationValue, annotationValue)

				_ = c.stopCapture(state.PID, pod.Name)

				pid, err := c.startCapture(pod, annotationValue)
				if err != nil {
					fmt.Printf("Error starting capture: %v\n", err)
					return err
				}

				c.activeCaptures[string(pod.UID)] = CaptureState{
					PID:             pid,
					AnnotationValue: annotationValue,
					PodName:         pod.Name,
					PodNamespace:    pod.Namespace,
				}
				fmt.Printf("Updated capture for pod %s (New PID: %d)\n", pod.Name, pid)
			}

		} else {
			// Sub-case: New Request (Start new capture)
			fmt.Printf("Detected new annotation on pod %s: %s\n", pod.Name, annotationValue)

			pid, err := c.startCapture(pod, annotationValue)
			if err != nil {
				fmt.Printf("Error starting capture: %v\n", err)
				return err
			}

			// Save to map
			c.activeCaptures[string(pod.UID)] = CaptureState{
				PID:             pid,
				AnnotationValue: annotationValue,
				PodName:         pod.Name,
				PodNamespace:    pod.Namespace,
			}
			fmt.Printf("Started capture for pod %s (PID: %d)\n", pod.Name, pid)
		}

	} else {
		// SCENARIO 2: User stopped capture (Annotation Deleted)

		if isActive {
			fmt.Printf("Stopping capture for pod %s (PID: %d)\n", pod.Name, state.PID)

			if err := c.stopCapture(state.PID, pod.Name); err != nil {
				fmt.Printf("Warning: Error stopping capture: %v\n", err)
			}

			delete(c.activeCaptures, string(pod.UID))
		}
	}

	return nil
}
