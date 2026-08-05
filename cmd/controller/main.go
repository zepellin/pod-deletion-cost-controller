package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
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
	"github.com/zepellin/pod-deletion-cost-controller/internal/telemetry"
)

const (
	lockName = "pod-deletion-cost-controller"

	leaseDuration = 15 * time.Second
	renewDeadline = 10 * time.Second
	retryPeriod   = 2 * time.Second

	// A leader fails its liveness probe once it has gone stalenessFactor sync
	// intervals without completing a cycle, but never sooner than minStaleAfter.
	// The floor matters for short sync intervals, where a single legitimately
	// slow cycle could otherwise be mistaken for a wedged controller.
	stalenessFactor = 3
	minStaleAfter   = 2 * time.Minute
)

// livenessStaleAfter is how long a leader may go without completing a sync
// cycle before it is considered wedged.
func livenessStaleAfter(syncInterval time.Duration) time.Duration {
	return max(stalenessFactor*syncInterval, minStaleAfter)
}

type options struct {
	configPath    string
	kubeconfig    string
	logLevel      string
	healthAddr    string
	leaderElect   bool
	leaderElectNS string
}

func main() {
	if err := run(); err != nil {
		slog.Error("controller failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var opts options
	flag.StringVar(&opts.configPath, "config", "/etc/controller/config.yaml", "path to config file")
	flag.StringVar(&opts.kubeconfig, "kubeconfig", "", "path to kubeconfig (leave empty for in-cluster)")
	flag.StringVar(&opts.logLevel, "log-level", "info", "log level: debug, info, warn, error")
	flag.StringVar(&opts.healthAddr, "health-addr", ":8080", "address for the health and metrics HTTP server")
	flag.BoolVar(&opts.leaderElect, "leader-elect", true, "enable leader election (required when running more than one replica)")
	flag.StringVar(&opts.leaderElectNS, "leader-elect-namespace", "", "namespace for the leader election Lease object (defaults to POD_NAMESPACE env var)")
	flag.Parse()

	log := newLogger(opts.logLevel)

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	restCfg, err := buildRestConfig(opts.kubeconfig)
	if err != nil {
		return fmt.Errorf("build kubeconfig: %w", err)
	}

	k8sClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}

	metricsClientset, err := metricsclient.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create metrics client: %w", err)
	}

	rec := telemetry.NewRecorder()
	syncer := controller.New(k8sClient, metrics.New(metricsClientset), cfg, log, rec)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stopHealth, err := startHealthServer(opts.healthAddr, log, rec, livenessStaleAfter(cfg.SyncInterval))
	if err != nil {
		return fmt.Errorf("start health server: %w", err)
	}
	defer stopHealth()

	if !opts.leaderElect {
		rec.SetLeading(true)
		if err := syncer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("syncer: %w", err)
		}
		log.Info("controller stopped")
		return nil
	}

	if err := runWithLeaderElection(ctx, opts, k8sClient, syncer, rec, log); err != nil {
		return err
	}
	log.Info("controller stopped")
	return nil
}

// runWithLeaderElection blocks until the outer context is cancelled or leadership
// is lost. Losing the lease unexpectedly is returned as an error so the process
// exits non-zero and Kubernetes restarts it to re-compete for the lease.
func runWithLeaderElection(
	ctx context.Context,
	opts options,
	k8sClient kubernetes.Interface,
	syncer *controller.Syncer,
	rec *telemetry.Recorder,
	log *slog.Logger,
) error {
	ns := opts.leaderElectNS
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	if ns == "" {
		return errors.New("leader election namespace not set; use --leader-elect-namespace or set POD_NAMESPACE")
	}

	// Use pod name as identity so the current leader is visible in the Lease object.
	id := os.Getenv("POD_NAME")
	if id == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("determine leader election identity: %w", err)
		}
		id = hostname
	}

	// Broadcaster with no handlers: events are silently dropped, but the recorder
	// won't panic when the resource lock tries to emit leader-transition events.
	broadcaster := record.NewBroadcaster()
	recorder := broadcaster.NewRecorder(clientgoscheme.Scheme, corev1.EventSource{Component: lockName})

	rl, err := resourcelock.New(
		resourcelock.LeasesResourceLock,
		ns,
		lockName,
		k8sClient.CoreV1(),
		k8sClient.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: recorder,
		},
	)
	if err != nil {
		return fmt.Errorf("create leader election lock: %w", err)
	}

	log.Info("starting leader election", "namespace", ns, "identity", id)

	// Buffered so neither callback can block on send; the first error wins.
	errCh := make(chan error, 2)
	leCtx, stopLeading := context.WithCancel(ctx)
	defer stopLeading()

	leaderelection.RunOrDie(leCtx, leaderelection.LeaderElectionConfig{
		Lock:            rl,
		LeaseDuration:   leaseDuration,
		RenewDeadline:   renewDeadline,
		RetryPeriod:     retryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Info("acquired leader lease, starting syncer")
				rec.SetLeading(true)
				if err := syncer.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					errCh <- fmt.Errorf("syncer: %w", err)
					stopLeading()
				}
			},
			OnStoppedLeading: func() {
				rec.SetLeading(false)
				if ctx.Err() != nil {
					// Graceful shutdown — SIGTERM cancelled the outer context.
					log.Info("leader lease released on shutdown")
					return
				}
				errCh <- errors.New("lost leader lease unexpectedly")
				stopLeading()
			},
		},
	})

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
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

// startHealthServer binds addr and serves health and metrics endpoints. Binding
// happens synchronously so a port conflict is a startup failure rather than a
// silently degraded process. The returned func shuts the server down.
func startHealthServer(addr string, log *slog.Logger, rec *telemetry.Recorder, staleAfter time.Duration) (func(), error) {
	mux := http.NewServeMux()
	// Liveness: a leader that has stopped completing sync cycles is wedged and
	// should be restarted. Standby replicas always pass — they are not syncing
	// by design.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := rec.CheckLiveness(staleAfter); err != nil {
			log.Error("liveness check failed", "err", err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// Readiness: the process is up and serving. Standby replicas stay Ready so
	// that their metrics continue to be scraped.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/metrics", rec.Handler())

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server error", "err", err)
		}
	}()
	log.Info("health and metrics server listening", "addr", ln.Addr().String())

	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Warn("health server shutdown", "err", err)
		}
	}, nil
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
