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

	validator, err := newValidator(rpcURL)
	if err != nil {
		klog.Fatalf("Failed to create Validator: %v", err)
	}

	clientset, err := newClientset(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to connect to cluster: %v", err)
	}

	elector, err := newElector(clientset, validator, leaseName, leaseNamespace, nodeID)
	if err != nil {
		klog.Fatalf("Failed to create Elector: %v", err)
	}

	go func() {
		<-sigChan
		klog.Info("Shutting down")
		elector.Shutdown()
		validator.Shutdown()
		cancel()
	}()

	ticker := time.NewTicker(refreshPeriod)
	for {
		select {
		case <-ctx.Done():
			klog.Info("Exiting main")
			return
		case <-ticker.C:
			klog.Infof("Checking validator state")
		}

		synced := validator.IsSynced()
		if synced {
			klog.Infof("Starting elector")
			elector.Start(ctx)
		} else {
			klog.Infof("Stopping elector")
			elector.Stop()
		}
	}
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

type Validator struct {
	channel chan string
	wg *sync.WaitGroup
}

func newValidator(rpcURL string) (*Validator, error) {
	validator := Validator{}
	validator.channel = make(chan string, 1)
	validator.wg = &sync.WaitGroup{}

	validator.wg.Add(1)
	go func() {
		defer validator.wg.Done()

		op := "stop"
		ticker := time.NewTicker(refreshPeriod)
		for {
			select {
			case <-ticker.C:
				klog.Infof("Refreshing desired validator state %v", op)
			case op = <-validator.channel:
				klog.Infof("New desired validator state %v", op)
			}

			switch op {
			case "shutdown":
				rpc(rpcURL, "istanbul_stopValidating")
				klog.Info("Validator shutdown")
				return
			case "start":
				rpc(rpcURL, "istanbul_startValidating")
			case "stop":
				rpc(rpcURL, "istanbul_stopValidating")
			}
		}
	}()

	return &validator, nil
}

func (validator *Validator) Start() {
	validator.channel <- "start"
}

func (validator *Validator) Stop() {
	validator.channel <- "stop"
}

func (validator *Validator) Shutdown() {
	validator.channel <- "shutdown"
	validator.wg.Wait()
}

func (validator *Validator) IsSynced() bool {
	// TODO(sbw): add RPC calls and "synced" logic
	return true
}

type Elector struct {
	clientset *kubernetes.Clientset
	validator *Validator

	leaseName string
	leaseNamespace string
	nodeID string

	wg *sync.WaitGroup
	cancel context.CancelFunc
}

func newElector(clientset *kubernetes.Clientset, validator *Validator, leaseName string, leaseNamespace string, nodeID string) (*Elector, error) {
	elector := Elector{clientset, validator, leaseName, leaseNamespace, nodeID, nil, nil}
	return &elector, nil
}

func (elector *Elector) Start(ctx context.Context) {
	if elector.cancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	elector.cancel = cancel
	elector.wg = &sync.WaitGroup{}

	elector.wg.Add(1)
	go func() {
		defer elector.wg.Done()
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name: elector.leaseName,
				Namespace: elector.leaseNamespace,
			},
			Client: elector.clientset.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: elector.nodeID,
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
					elector.validator.Start()
					klog.Infof("%v started leading", elector.nodeID)
				},
				// If this is a graceful shutdown, the signal handler will have already sent "shutdown".
				// Send "stop" below in case we lost the lock unexpectedly and should stop validating ASAP.
				OnStoppedLeading: func() {
					elector.validator.Stop()
					klog.Infof("%v stopped leading", elector.nodeID)
				},
				OnNewLeader: func(identity string) {
					klog.Infof("%v started leading", identity)
				},
			},
		}

		leaderelection.RunOrDie(ctx, config)
		klog.Info("Elector stopped")
	}()
}

func (elector *Elector) Stop() {
	if elector.cancel != nil {
		elector.cancel()
		elector.wg.Wait()
		elector.cancel = nil
	}
}

func (elector *Elector) Shutdown() {
	elector.Stop()
	klog.Info("Elector shutdown")
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
