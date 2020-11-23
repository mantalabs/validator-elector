package main

import (
	"context"
	"errors"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
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
var maxAgeOfLastBlock = 60 * time.Second

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
		elector.shutdown()
		validator.shutdown()
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

		synced, err := validator.isSynced()

		if err != nil {
			klog.Warningf("Error getting validator status: %v", err)
			elector.stop()
		} else if synced {
			klog.Infof("Validator is synced, starting elector")
			elector.start(ctx)
		} else {
			klog.Infof("Validator is not synced, stopping elector")
			elector.stop()
		}
	}
}

// Validator controls a Celo Validator via JSONRPC.
type Validator struct {
	channel chan string
	wg *sync.WaitGroup
	rpcURL string
}

func newValidator(rpcURL string) (*Validator, error) {
	validator := Validator{}
	validator.rpcURL = rpcURL
	validator.channel = make(chan string, 1)
	validator.wg = &sync.WaitGroup{}

	// Loop continously and try to ensure the Celo Validator behavior/state matches
	// the desired state. The loop handles intermittent JSONRPC failures.
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
				validator.rpc("istanbul_stopValidating", nil)
				klog.Info("Validator shutdown")
				return
			case "start":
				validator.rpc("istanbul_startValidating", nil)
			case "stop":
				validator.rpc("istanbul_stopValidating", nil)
			}
		}
	}()

	return &validator, nil
}

func (validator *Validator) rpc(method string, params[]interface{}) (interface{}, error) {
	if params == nil {
		params = make([]interface{}, 0)
	}
	client := resty.New()

	resp, err := client.R().
		SetBody(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params, "id": 89999}).
		Post(validator.rpcURL)
	if err != nil {
		klog.Warningf("HTTP %v failed: %v", method, err)
		return nil, err
	}

	var body map[string]interface{}
	if err := json.Unmarshal(resp.Body(), &body); err != nil {
		klog.Warningf("failed to unmarshal response '%v': %v", resp, err)
		return nil, err
	}
	if _, error := body["error"]; error {
		klog.Warningf("RPC %v failed: %v", method, resp)
		return nil, errors.New("RPC failed")
	}
	return body["result"], nil
}

func (validator *Validator) start() {
	validator.channel <- "start"
}

func (validator *Validator) stop() {
	validator.channel <- "stop"
}

func (validator *Validator) shutdown() {
	validator.channel <- "shutdown"
	validator.wg.Wait()
}

func (validator *Validator) currentBlock() (string, error) {
	// Read operations have no effect on the desired validator state, so execute
	// them without serializing them via the controller loop.

	result, err := validator.rpc("eth_blockNumber", nil)
	if err != nil {
		return "", err
	}
	blockNumber, ok := result.(string)
	if !ok {
		return "", fmt.Errorf("ethBlockNumber unexpected value: %v", result)
	}
	return blockNumber, nil
}

func (validator *Validator) blockTimestamp(block string) (int64, error) {
	// Read operations have no effect on the desired validator state, so execute
	// them without serializing them via the controller loop.

	params := []interface{}{block, true}
	result, err := validator.rpc("eth_getBlockByNumber", params)
	if err != nil {
		return 0, err
	}
	blockData, ok := result.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("Unexpected result: %v", result)
	}
	timestampStr, ok := blockData["timestamp"].(string)
	if !ok {
		return 0, fmt.Errorf("Missing timestamp: %v", result)
	}
	timestamp, err := strconv.ParseInt(timestampStr, 0, 64)
	if err != nil {
		return 0, err
	}
	return timestamp, nil
}

func (validator *Validator) isSynced() (bool, error) {
	block, err := validator.currentBlock()
	if err != nil {
		return false, err
	}
	timestamp, err := validator.blockTimestamp(block)
	if err != nil {
		return false, err
	}
	blockTime := time.Unix(timestamp, 0)
	now := time.Now()

	klog.Infof("Block %v is from %v", block, blockTime)
	diff := now.Sub(blockTime)
	if diff < maxAgeOfLastBlock {
		return true, nil
	}
	return false, nil
}

// Elector controls the Kubernetes leader election.
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

func (elector *Elector) start(ctx context.Context) {
	if elector.cancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	elector.cancel = cancel
	elector.wg = &sync.WaitGroup{}

	// Start a Kubernetes LeaderElector client to participate in the leader election.
	// When the election elects this instance the code below sets the Validator state
	// to "start", otherwise it sets the state to "stop".
	elector.wg.Add(1)
	go func() {
		defer elector.wg.Done()
		// https://pkg.go.dev/k8s.io/client-go/tools/leaderelection
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
					elector.validator.start()
					klog.Infof("%v started leading", elector.nodeID)
				},
				OnStoppedLeading: func() {
					elector.validator.stop()
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

func (elector *Elector) stop() {
	if elector.cancel != nil {
		elector.cancel()
		elector.wg.Wait()
		elector.cancel = nil
	}
}

func (elector *Elector) shutdown() {
	elector.stop()
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
