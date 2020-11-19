package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-resty/resty/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

var refreshPeriod = time.Second
var leaseDuration = 15 * time.Second
var renewDeadline = (2 * leaseDuration) / 3
var retryPeriod = 2 * time.Second

func main() {
	klog.InitFlags(nil)

	var kubeconfig string
	var nodeID string
	var leaseNamespace string
	var leaseName string
	var rpcURL string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&nodeID, "node-id", "", "node id used for leader election")
	flag.StringVar(&leaseNamespace, "lease-namespace", "", "namespace of lease object")
	flag.StringVar(&leaseName, "lease-name", "", "name of lease object")
	flag.StringVar(&rpcURL, "rpc-url", "http://127.0.0.1:8545", "RPC URL")
	flag.Parse()

	if leaseNamespace == "" {
		klog.Fatal("-lease-namespace required")
	}
	if leaseName == "" {
		klog.Fatal("-lease required")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())

	controllerChan := make(chan string, 1)
	controllerWg := startController(controllerChan, rpcURL)

	go func() {
		<-sigChan
		klog.Info("Shutting down")
		controllerChan <- "shutdown"
		controllerWg.Wait()
		// Wait for shutdown to process before proceeding
		cancel()
	}()

	newElector(ctx, controllerChan, nodeID, leaseNamespace, leaseName, kubeconfig)
}

func rpc(rpcURL string, method string) {
	client := resty.New()

	resp, err := client.R().
		SetBody(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": []int{}, "id": 89999}).
		Post(rpcURL)
	if err != nil {
		klog.Warningf("HTTP %v failed: %v", method, err)
	} else {
		var body map[string]interface{}
		if err := json.Unmarshal(resp.Body(), &body); err != nil {
			klog.Warningf("failed to unmarshal response '%v': %v", resp, err)
			return
		}
		if _, error := body["error"]; error {
			klog.Warningf("RPC %v failed: %v", method, resp)
			return
		}
		klog.Infof("%v succeeded: %v", method, resp)
	}
}

func startController(c chan string, rpcURL string) (*sync.WaitGroup) {
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		op := "stop"
		ticker := time.NewTicker(refreshPeriod)
		for {
			select {
			case <-ticker.C:
				klog.Infof("Refreshing desired validator state %v", op)
			case op = <-c:
				klog.Infof("New desired validator state %v", op)
			}

			switch op {
			case "shutdown":
				rpc(rpcURL, "istanbul_stopValidating")
				return
			case "start":
				rpc(rpcURL, "istanbul_startValidating")
			case "stop":
				rpc(rpcURL, "istanbul_stopValidating")
			}
		}
	}()
	return &wg
}

func newElector(ctx context.Context, controllerChan chan string, nodeID string, leaseNamespace string, leaseName string, kubeconfig string) {
	clientset, err := newClientset(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to connect to cluster: %v", err)
	}

	var lock = &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name: leaseName,
			Namespace: leaseNamespace,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: nodeID,
		},
	}

	config := leaderelection.LeaderElectionConfig{
		Lock: lock,
		ReleaseOnCancel: true,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod: retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				controllerChan <- "start"
				klog.Infof("%v started leading", nodeID)
			},
			// If this is a graceful shutdown, the signal handler will have already sent "shutdown".
			// Send "stop" below in case we lost the lock unexpectedly and should stop validating ASAP.
			OnStoppedLeading: func() {
				controllerChan <- "stop"
				klog.Infof("%v stopped leading", nodeID)
			},
			OnNewLeader: func(identity string) {
				klog.Infof("%v started leading", identity)
			},
		},
	}

	leaderelection.RunOrDie(ctx, config)
}

func newClientset(filename string) (*kubernetes.Clientset, error) {
	config, err := getConfig(filename)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

func getConfig(cfg string) (*rest.Config, error) {
	if cfg == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", cfg)
}
