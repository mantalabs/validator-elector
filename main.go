package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
	
	"github.com/apex/log"
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
	
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-sigChan
		fmt.Println("Shutting down...")
		cancel()
	}()

	if *elector {
		newElector(ctx, *nodeID, *leaseNamespace, *leaseName, *kubeconfig)
	}

	if *listen {
		newHTTP(ctx, *port)		
	}
}

func newElector(ctx context.Context, nodeID string, leaseNamespace string, leaseName string, kubeconfig string) {
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
				log.WithField("id", nodeID).Info("started leading")
			},
			OnStoppedLeading: func() {
				atomic.StoreInt32(&leading, 0)
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
