package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	
	"github.com/apex/log"
	"github.com/go-resty/resty/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

var leading int32

func main() {
	listen := flag.Bool("listen", true, "enable HTTP service")
	elector := flag.Bool("elector", false, "enable participation in leader election")
	port := flag.Int("port", 8080, "HTTP port")
	

	var kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	var nodeID = flag.String("node-id", "", "node id used for leader election")

	leaseNamespace := flag.String("lease-namespace", "", "namespace of lease object")
	leaseName := flag.String("lease-name", "", "name of lease object")	
	flag.Parse()
	if *leaseNamespace == "" {
		log.Fatal("-lease-namespace required")
	}
	if *leaseName == "" {
		log.Fatal("-lease required")
	}
	rpcURL := flag.String("rpc-url", "http://127.0.0.1:8545", "RPC URL")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())

	controllerChan := make(chan string)
	controllerWg := startController(controllerChan, *rpcURL)

	go func() {
		<-sigChan
		fmt.Println("Shutting down...")
		controllerChan <- "shutdown"
		controllerWg.Wait()
		cancel()
	}()

	if *elector {
		newElector(ctx, controllerChan, *nodeID, *leaseNamespace, *leaseName, *kubeconfig)
	}

	if *listen {
		newHTTP(ctx, *port)		
	}
}

func rpc(rpcURL string, method string) {
	client := resty.New()

	resp, err := client.R().
		SetBody(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": []int{}, "id": 89999}).
		Post(rpcURL)
	if err != nil {
		log.Warnf("HTTP %v failed: %v", method, err)
	} else {
		var body map[string]interface{}
		if err := json.Unmarshal(resp.Body(), &body); err != nil {
			log.Warnf("failed to unmarshal response '%v': %v", resp, err)
			return
		}
		if _, error := body["error"]; error {
			log.Warnf("RPC %v failed: %v", method, resp)
			return
		}
		log.Infof("%v succeeded: %v", method, resp)
	}
}

func startController(c chan string, rpcURL string) (*sync.WaitGroup) {
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		op := "stop"
		ticker := time.NewTicker(time.Second)
		for {
			select {
			case <-ticker.C:
				log.Infof("Refreshing desired validator state %v", op)
			case op = <-c:
				log.Infof("New desired validator state %v", op)
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
		log.WithError(err).Fatal("failed to connect to cluster")
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
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod: 2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				atomic.StoreInt32(&leading, 1)
				controllerChan <- "start"
				log.WithField("id", nodeID).Info("started leading")
			},
			// If this is a graceful shutdown, the signal handler will have already sent "shutdown".
			// Send "stop" below in case we lost the lock unexpectedly and should stop validating ASAP.
			OnStoppedLeading: func() {
				atomic.StoreInt32(&leading, 0)
				controllerChan <- "stop"
				log.WithField("id", nodeID).Info("stopped leading")
			},
			OnNewLeader: func(identity string) {
				log.WithField("id", nodeID).
					WithField("leader", identity).
					Info("new leader")
			},
		},
	}

	go func() {
		leaderelection.RunOrDie(ctx, config)
		// TOD(sbw): if this fails should abort/exit.
	}()
}

func getHealthz(w http.ResponseWriter, r *http.Request) {
	response := map[string]string{
		"status": "ok",
	}
	content, err := json.Marshal(response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.Write(content)
}

func getLeading(w http.ResponseWriter, r *http.Request) {
	response := map[string]string{
		"status": "ok",
		"result": fmt.Sprintf("%v", atomic.LoadInt32(&leading) == 1),
	}
	content, err := json.Marshal(response)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(content)
}

func newHTTP(ctx context.Context, port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", getHealthz)
	mux.HandleFunc("/leading", getLeading)

	log.WithField("port", port).
		Info("HTTP listening")

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Fatal("listen and serve failed")
		}
	}()
	log.Info("HTTP server started")

	<-ctx.Done()

	log.Info("shutting down HTTP server")

	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		cancel()
	}()

	if err := srv.Shutdown(ctxShutDown); err != nil {
		log.WithError(err).Fatal("server shutdown failed")
	}
	
	log.Info("server exited properly")
	return
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
