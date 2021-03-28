package main

import (
	"context"
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

// The controller waits this long to start validating once it acquire the lock. This
// is to mitigate the risk of doubling signing.
var startValidationDelay = 20 * time.Second

type ErrorResponse struct {
	Jsonrpc string
	ID      int `json:"id"`
	Error   struct {
		Code    int
		Message string
	}
}

type EthBlockNumber struct {
	Jsonrpc string
	ID      int `json:"id"`
	Result  string
}

type EthGetBlockByNumber struct {
	Jsonrpc string
	ID      int `json:"id"`
	Result  struct {
		Timestamp string
	}
}

type IstanbulStopValidating struct {
	Jsonrpc string
	ID      int `json:"id"`
}

type IstanbulStartValidating struct {
	Jsonrpc string
	ID      int `json:"id"`
}

func main() {
	klog.InitFlags(nil)

	var kubeconfig string
	var nodeID string
	var leaseNamespace string
	var leaseName string
	var rpcURL string
	var allowPrimary bool

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&nodeID, "node-id", "", "node id used for leader election")
	flag.StringVar(&leaseNamespace, "lease-namespace", "", "namespace of lease object")
	flag.StringVar(&leaseName, "lease-name", "", "name of lease object")
	flag.StringVar(&rpcURL, "rpc-url", "http://127.0.0.1:8545", "RPC URL")
	flag.BoolVar(&allowPrimary, "allow-primary", true, "If holding the lock, run the validator as a primary")
	flag.Parse()

	if leaseNamespace == "" {
		klog.Fatal("-lease-namespace required")
	}
	if leaseName == "" {
		klog.Fatal("-lease-name required")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())

	validator := newValidator(rpcURL, allowPrimary)

	clientset, err := newClientset(kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to connect to cluster: %v", err)
	}

	elector := newElector(clientset, validator, leaseName, leaseNamespace, nodeID)

	go func() {
		<-sigChan
		klog.Info("Shutting down")
		elector.Shutdown()
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
			break
		}

		synced, err := validator.isSynced()
		if err != nil {
			klog.Warningf("Error getting validator status: %v", err)
			elector.Stop()
		} else if synced {
			if !elector.Running() {
				klog.Infof("Validator is synced, starting elector")
				elector.Start(ctx)
			}
		} else {
			if elector.Running() {
				klog.Infof("Validator is not synced, stopping elector")
				elector.Stop()
			}
		}
	}
}

// Validator controls a Celo Validator via JSONRPC.
type Validator struct {
	channel chan string
	wg      *sync.WaitGroup
	rpcURL  string
}

func newValidator(rpcURL string, allowPrimary bool) *Validator {
	validator := Validator{}
	validator.rpcURL = rpcURL
	validator.channel = make(chan string, 1)
	validator.wg = &sync.WaitGroup{}

	// Loop continuously and try to ensure the Celo Validator behavior/state matches
	// the desired state. The loop handles intermittent JSONRPC failures.
	validator.wg.Add(1)
	go func() {
		defer validator.wg.Done()

		op := "stop"
		opStartTime := time.Now()

		ticker := time.NewTicker(refreshPeriod)
		for {
			select {
			case <-ticker.C:
				break
			case newOp := <-validator.channel:
				if newOp != op {
					opStartTime = time.Now()
					op = newOp
					klog.Infof("New desired validator state %v", op)
				}
			}

			elapsedOpTime := time.Since(opStartTime)

			switch op {
			case "shutdown":
				klog.Info("Shutting down validator")
				_, err := validator.rpc("istanbul_stopValidating", nil, IstanbulStopValidating{})
				if err != nil {
					klog.Warningf("RPC to stop validating failed: %v", err)
				}
				return
			case "start":
				if allowPrimary && elapsedOpTime > startValidationDelay {
					_, err := validator.rpc("istanbul_startValidating", nil, IstanbulStartValidating{})
					if err != nil {
						klog.Warningf("RPC to start validating failed: %v", err)
					}
				}
			case "stop":
				_, err := validator.rpc("istanbul_stopValidating", nil, IstanbulStopValidating{})
				if err != nil {
					klog.Warningf("RPC to stop validating failed: %v", err)
				}
			}
		}
	}()

	return &validator
}

func (validator *Validator) rpc(method string, params []interface{}, result interface{}) (interface{}, error) {
	client := resty.New()

	resp, err := client.R().
		SetBody(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params, "id": 89999}).
		SetResult(result).
		Post(validator.rpcURL)
	if err != nil {
		klog.Warningf("HTTP %v failed: %v", method, err)
		return nil, err
	}

	var errorResponse ErrorResponse
	if err := json.Unmarshal(resp.Body(), &errorResponse); err != nil {
		klog.Warningf("failed to unmarshal response '%v': %v", resp, err)
		return nil, err
	}
	if errorResponse.Error.Code != 0 {
		return nil, fmt.Errorf("RPC error: %v", string(resp.Body()))
	}

	return resp.Result(), nil
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

	response, err := validator.rpc("eth_blockNumber", nil, EthBlockNumber{})
	if err != nil {
		return "", err
	}
	blockNumber := response.(*EthBlockNumber).Result

	return blockNumber, nil
}

func (validator *Validator) blockTimestamp(block string) (int64, error) {
	// Read operations have no effect on the desired validator state, so execute
	// them without serializing them via the controller loop.

	params := []interface{}{block, true}
	response, err := validator.rpc("eth_getBlockByNumber", params, EthGetBlockByNumber{})
	if err != nil {
		return 0, err
	}
	result := response.(*EthGetBlockByNumber).Result

	timestamp, err := strconv.ParseInt(result.Timestamp, 0, 64)
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

	diff := now.Sub(blockTime)
	if diff < maxAgeOfLastBlock {
		return true, nil
	}
	blockNumber, _ := strconv.ParseUint(block, 0, 64)
	klog.Infof("Block %v is from %v", blockNumber, blockTime)

	return false, nil
}

// Elector controls the Kubernetes leader election.
type Elector struct {
	clientset *kubernetes.Clientset
	validator *Validator

	leaseName      string
	leaseNamespace string
	nodeID         string

	wg     *sync.WaitGroup
	cancel context.CancelFunc
}

func newElector(clientset *kubernetes.Clientset, validator *Validator, leaseName, leaseNamespace, nodeID string) *Elector {
	elector := Elector{clientset, validator, leaseName, leaseNamespace, nodeID, nil, nil}
	return &elector
}

func (elector *Elector) Running() bool {
	return elector.cancel != nil
}

func (elector *Elector) Start(ctx context.Context) {
	if elector.Running() {
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
		for {
			// https://pkg.go.dev/k8s.io/client-go/tools/leaderelection
			lock := &resourcelock.LeaseLock{
				LeaseMeta: metav1.ObjectMeta{
					Name:      elector.leaseName,
					Namespace: elector.leaseNamespace,
				},
				Client: elector.clientset.CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{
					Identity: elector.nodeID,
				},
			}

			config := leaderelection.LeaderElectionConfig{
				Lock:            lock,
				ReleaseOnCancel: true,
				LeaseDuration:   leaseDuration,
				RenewDeadline:   renewDeadline,
				RetryPeriod:     retryPeriod,
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

			select {
			case <-ctx.Done():
				klog.Info("Elector stopped")
				return
			default:
				// Not canceled, so retry RunOrDie. There are some conditions that cause RunOrDie
				// to exit. Run RunOrDie again when that happens.
				klog.Warning("leaderelection.RunOrDie exited. Re-trying.")
				// Delay a bit avoid retrying superfluously when there's an ongoing transient cluster issue.
				time.Sleep(retryPeriod)
				continue
			}
		}
	}()
}

func (elector *Elector) Stop() {
	if elector.Running() {
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
