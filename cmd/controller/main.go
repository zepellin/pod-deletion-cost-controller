package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/zepellin/pod-deletion-cost-controller/internal/config"
	"github.com/zepellin/pod-deletion-cost-controller/internal/controller"
	"github.com/zepellin/pod-deletion-cost-controller/internal/metrics"
)

func main() {
	configPath := flag.String("config", "/etc/controller/config.yaml", "path to config file")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (leave empty for in-cluster)")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	healthAddr := flag.String("health-addr", ":8080", "address for the health check HTTP server")
	leaderElect := flag.Bool("leader-elect", true, "enable leader election (required when running more than one replica)")
	leaderElectNS := flag.String("leader-elect-namespace", "", "namespace for the leader election Lease object (defaults to POD_NAMESPACE env var)")
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

	if !*leaderElect {
		if err := syncer.Run(ctx); err != nil && err != context.Canceled {
			log.Error("syncer exited with error", "err", err)
			os.Exit(1)
		}
		log.Info("controller stopped")
		return
	}

	ns := *leaderElectNS
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	if ns == "" {
		log.Error("leader election namespace not set; use --leader-elect-namespace or set POD_NAMESPACE")
		os.Exit(1)
	}

	// Use pod name as identity so the current leader is visible in the Lease object.
	id := os.Getenv("POD_NAME")
	if id == "" {
		id, err = os.Hostname()
		if err != nil {
			log.Error("failed to determine leader election identity", "err", err)
			os.Exit(1)
		}
	}

	// Broadcaster with no handlers: events are silently dropped, but the recorder
	// won't panic when the resource lock tries to emit leader-transition events.
	broadcaster := record.NewBroadcaster()
	recorder := broadcaster.NewRecorder(clientgoscheme.Scheme, corev1.EventSource{Component: "pod-deletion-cost-controller"})

	rl, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		ns,
		"pod-deletion-cost-controller",
		k8sClient.CoreV1(),
		k8sClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: recorder,
		},
	)
	if err != nil {
		log.Error("failed to create leader election lock", "err", err)
		os.Exit(1)
	}

	log.Info("starting leader election", "namespace", ns, "identity", id)

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Info("acquired leader lease, starting syncer")
				if err := syncer.Run(ctx); err != nil && err != context.Canceled {
					log.Error("syncer exited with error", "err", err)
					os.Exit(1)
				}
			},
			OnStoppedLeading: func() {
				if ctx.Err() != nil {
					// Graceful shutdown — SIGTERM cancelled the outer context.
					log.Info("leader lease released on shutdown")
					return
				}
				// Unexpected lease loss (e.g. network partition). Exit so Kubernetes
				// restarts the pod and the process can re-compete for the lease.
				log.Error("lost leader lease unexpectedly, exiting to trigger restart")
				os.Exit(1)
			},
			OnNewLeader: func(identity string) {
				if identity != id {
					log.Info("new leader elected", "leader", identity)
				}
			},
		},
	})

	log.Info("controller stopped")
}

func buildRestConfig(kubeconfigPath string) (*rest.Config, error) {
	var (
		cfg *rest.Config
		err error
	)
	if kubeconfigPath == "" {
		cfg, err = rest.InClusterConfig()
	} else {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	if err != nil {
		return nil, err
	}
	// client-go rate-limits all API calls via a token bucket; be explicit so
	// operators can see and tune these values. Default (5 QPS / 10 burst) is
	// designed for scripts, not controllers.
	cfg.QPS = 20
	cfg.Burst = 30
	return cfg, nil
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
