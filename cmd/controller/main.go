package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/mdanko/pod-deletion-cost-controller/internal/config"
	"github.com/mdanko/pod-deletion-cost-controller/internal/controller"
	"github.com/mdanko/pod-deletion-cost-controller/internal/metrics"
)

func main() {
	configPath := flag.String("config", "/etc/controller/config.yaml", "path to config file")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (leave empty for in-cluster)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	healthAddr := flag.String("health-addr", ":8080", "address for the health check HTTP server")
	flag.Parse()

	log := newLogger(*logLevel)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	restCfg, err := buildRestConfig(*kubeconfig)
	if err != nil {
		log.Error("failed to build kubeconfig", "err", err)
		os.Exit(1)
	}

	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Error("failed to create kubernetes client", "err", err)
		os.Exit(1)
	}

	metricsClientset, err := metricsclient.NewForConfig(restCfg)
	if err != nil {
		log.Error("failed to create metrics client", "err", err)
		os.Exit(1)
	}

	mc := metrics.New(metricsClientset)
	syncer := controller.New(k8sClient, mc, cfg, log)

	startHealthServer(*healthAddr, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := syncer.Run(ctx); err != nil && err != context.Canceled {
		log.Error("syncer exited with error", "err", err)
		os.Exit(1)
	}

	log.Info("controller stopped")
}

func buildRestConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
}

func startHealthServer(addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Error("health server error", "err", err)
		}
	}()
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
