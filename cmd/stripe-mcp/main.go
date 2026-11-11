// Command stripe-mcp is a Model Context Protocol server that exposes a small,
// safe set of Stripe operations as tools.
//
// It speaks MCP over stdio by default (stdout carries the protocol, logs go
// to stderr) and over streamable HTTP when started with -http.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mzubairashraf/stripe-mcp-go/internal/config"
	"github.com/mzubairashraf/stripe-mcp-go/internal/stripe"
	"github.com/mzubairashraf/stripe-mcp-go/internal/tools"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "stripe-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		return err
	}

	// Logs must never go to stdout: on the stdio transport stdout is the
	// protocol channel and a stray line would corrupt it.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	api := stripe.New(cfg.APIKey.Reveal(), stripe.Options{
		BaseURL:    cfg.BaseURL,
		APIVersion: cfg.APIVersion,
		Logger:     logger,
	})

	server := mcp.NewServer(
		&mcp.Implementation{Name: "stripe-mcp-go", Title: "Stripe (Go)", Version: version},
		&mcp.ServerOptions{
			Instructions: tools.Instructions,
			// Advertise only what is implemented: tools. (The SDK default
			// also advertises MCP logging, which this server does not use.)
			Capabilities: &mcp.ServerCapabilities{},
		},
	)
	registered := tools.Register(server, api, tools.Options{
		ReadOnly: cfg.ReadOnly,
		Timeout:  cfg.ToolTimeout,
		Logger:   logger,
	})

	logger.Info("starting",
		slog.String("version", version),
		slog.Bool("livemode", cfg.Livemode),
		slog.Bool("read_only", cfg.ReadOnly),
		slog.Any("tools", registered),
		slog.String("transport", transportName(cfg)),
	)

	if cfg.HTTPAddr != "" {
		return serveHTTP(ctx, cfg, server, logger)
	}
	err = server.Run(ctx, &mcp.StdioTransport{})
	if ctx.Err() != nil {
		return nil // shut down by signal
	}
	return err
}

func transportName(cfg *config.Config) string {
	if cfg.HTTPAddr != "" {
		return "streamable-http"
	}
	return "stdio"
}

func serveHTTP(ctx context.Context, cfg *config.Config, server *mcp.Server, logger *slog.Logger) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Logger: logger, SessionTimeout: 30 * time.Minute},
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", requireBearer(cfg.HTTPToken, handler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("listening", slog.String("addr", cfg.HTTPAddr), slog.String("path", "/mcp"),
		slog.Bool("auth", cfg.HTTPToken != ""))

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// requireBearer enforces a static bearer token when one is configured.
// Without a token the server only binds to loopback (enforced in config).
func requireBearer(token config.Secret, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token.Reveal())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
