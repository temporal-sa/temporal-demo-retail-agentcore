// Package store is the demo's mock commerce backend: a stateless REST API for
// menus, stock, rewards, quotes, payments, orders and Stardust. It stands in
// for a café chain's real systems, which the agent would call the same way.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// Config is everything the store reads from its environment.
type Config struct {
	Addr  string
	Token string
	Table string
	// DynamoEndpoint points at DynamoDB Local under Compose. Empty on the
	// platform, where the SDK finds DynamoDB and the pod's role on its own.
	DynamoEndpoint string
	// CreateTable makes the table on start. Compose only.
	CreateTable bool
}

func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:           getenv("LISTEN_ADDR"),
		Token:          getenv("STORE_API_TOKEN"),
		Table:          getenv("STORE_LEDGER_TABLE"),
		DynamoEndpoint: getenv("DYNAMODB_ENDPOINT"),
		CreateTable:    getenv("STORE_CREATE_TABLE") == "true",
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	var errs []error
	if len(cfg.Token) < 24 {
		errs = append(errs, errors.New("STORE_API_TOKEN must be set, at least 24 characters"))
	}
	if cfg.Table == "" {
		errs = append(errs, errors.New("STORE_LEDGER_TABLE is not set"))
	}
	return cfg, errors.Join(errs...)
}

// Run serves the store until ctx is cancelled, then drains in-flight requests.
func Run(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	logger := slog.New(slog.NewTextHandler(stdout, nil))
	cfg, err := LoadConfig(getenv)
	if err != nil {
		return err
	}
	skin, err := LoadSkin()
	if err != nil {
		return err
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	db := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if cfg.DynamoEndpoint != "" {
			o.BaseEndpoint = aws.String(cfg.DynamoEndpoint)
		}
		// Each request is bounded by the handler's context; the SDK's own
		// retries cover a throttled or dropped call.
		o.RetryMaxAttempts = 3
	})
	ledger := NewDynamoLedger(db, cfg.Table)
	if cfg.CreateTable {
		if err := ledger.CreateTable(ctx); err != nil {
			return fmt.Errorf("create table %s: %w", cfg.Table, err)
		}
	}

	handler := NewServer(skin, ledger, cfg.Token, time.Now, logger)
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           http.TimeoutHandler(handler, 10*time.Second, `{"type":"about:blank","title":"Store timed out","status":503}`),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("store listening", "addr", cfg.Addr, "brand", skin.Brand.Name, "table", cfg.Table)
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
