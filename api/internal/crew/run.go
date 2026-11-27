// Package crew is the demo's API for the phone app: it starts crew order
// Workflows, sends the member's payment and messages as Signals, and reads
// state back for the UI without ever sending a Query.
package crew

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/envconfig"
	"go.temporal.io/sdk/log"
)

// Config is everything the API reads from its environment.
type Config struct {
	Addr      string
	TaskQueue string
	// UIURL and UINamespace build links into the Temporal UI. The platform sets
	// both on every Kubernetes component.
	UIURL       string
	UINamespace string
	// WorkerLocation words the "starting a Worker" banner: "agentcore" on the
	// platform, "local" under Compose, where a stand-in does the invoking.
	WorkerLocation string
	// StoreURL is the store's base URL inside the cluster (http://store:8080
	// under Compose). The API reads the member's profile and the order's
	// ledger rows from it; it never writes.
	StoreURL   string
	StoreToken string
	MemberID   string
	CafeID     string
}

// LoadConfig reads Config, failing on what the demo can't run without.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:           getenv("LISTEN_ADDR"),
		TaskQueue:      getenv("TEMPORAL_TASK_QUEUE"),
		UIURL:          getenv("TEMPORAL_UI_URL"),
		UINamespace:    getenv("TEMPORAL_UI_NAMESPACE"),
		WorkerLocation: getenv("WORKER_LOCATION"),
		StoreURL:       strings.TrimRight(getenv("STORE_INTERNAL_URL"), "/"),
		StoreToken:     getenv("STORE_API_TOKEN"),
		MemberID:       getenv("DEMO_MEMBER_ID"),
		CafeID:         getenv("DEMO_CAFE_ID"),
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.WorkerLocation == "" {
		cfg.WorkerLocation = "agentcore"
	}
	if cfg.MemberID == "" {
		cfg.MemberID = "zora-vex"
	}
	if cfg.CafeID == "" {
		cfg.CafeID = "titan-ring"
	}
	var errs []error
	if cfg.TaskQueue == "" {
		errs = append(errs, errors.New("TEMPORAL_TASK_QUEUE is not set"))
	}
	if cfg.WorkerLocation != "agentcore" && cfg.WorkerLocation != "local" {
		errs = append(errs, fmt.Errorf("WORKER_LOCATION must be agentcore or local, got %q", cfg.WorkerLocation))
	}
	if cfg.StoreURL == "" || cfg.StoreToken == "" {
		errs = append(errs, errors.New("STORE_INTERNAL_URL and STORE_API_TOKEN must be set"))
	}
	return cfg, errors.Join(errs...)
}

// Run starts the server and blocks until ctx is cancelled, then drains it.
func Run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	logger := slog.New(slog.NewTextHandler(stdout, nil))
	cfg, err := LoadConfig(getenv)
	if err != nil {
		return err
	}

	// TEMPORAL_ADDRESS, TEMPORAL_NAMESPACE, TEMPORAL_API_KEY and TEMPORAL_TLS.
	// The config file is off so a developer's CLI profile can't point the API
	// at a real Cloud namespace.
	opts, err := envconfig.LoadClientOptions(envconfig.LoadClientOptionsRequest{DisableFile: true, EnvLookup: envLookup(getenv)})
	if err != nil {
		return fmt.Errorf("temporal client options: %w", err)
	}
	if opts.HostPort == "" {
		opts.HostPort = "localhost:7233"
	}
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	opts.Logger = log.NewStructuredLogger(logger)
	// Lazy, so the UI comes up and says "reconnecting" before Temporal is reachable.
	tc, err := client.NewLazyClient(opts)
	if err != nil {
		return fmt.Errorf("temporal client: %w", err)
	}
	defer tc.Close()

	store := NewStoreClient(cfg.StoreURL, cfg.StoreToken)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           NewServer(cfg, opts.Namespace, tc, store, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.Addr, "task_queue", cfg.TaskQueue, "namespace", opts.Namespace)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

type envLookup func(string) string

func (e envLookup) Environ() []string { return nil }
func (e envLookup) LookupEnv(key string) (string, bool) {
	v := e(key)
	return v, v != ""
}
