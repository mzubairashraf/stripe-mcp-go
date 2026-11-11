// Package config loads and validates runtime configuration. Secrets come
// from the environment only; there is no flag or file that accepts a key.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
)

// Secret holds a credential. Its formatting methods are redacted, so it can
// be passed to a logger or printed with %v without disclosing the value.
type Secret string

// Reveal returns the underlying value. Calling it is the only way to read
// a Secret, which keeps every use easy to find in review.
func (s Secret) Reveal() string { return string(s) }

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

func (s Secret) GoString() string             { return s.String() }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(s.String()) }
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Config is the validated runtime configuration.
type Config struct {
	APIKey     Secret
	Livemode   bool   // true when APIKey is a live-mode key
	BaseURL    string // empty means the real Stripe API
	APIVersion string // optional Stripe-Version pin

	ReadOnly    bool
	ToolTimeout time.Duration
	LogLevel    slog.Level

	HTTPAddr  string // empty means stdio
	HTTPToken Secret // bearer token required on the HTTP transport
}

// Load parses flags and environment. getenv is injected so tests do not
// depend on the process environment.
func Load(args []string, getenv func(string) string, stderr io.Writer) (*Config, error) {
	fs := flag.NewFlagSet("stripe-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		readOnly  = fs.Bool("read-only", envBool(getenv("STRIPE_MCP_READ_ONLY")), "do not register write tools (env STRIPE_MCP_READ_ONLY)")
		allowLive = fs.Bool("allow-live", false, "accept a live-mode key; the server is then forced read-only")
		httpAddr  = fs.String("http", "", "serve streamable HTTP on this address (e.g. 127.0.0.1:8080) instead of stdio")
		timeout   = fs.Duration("tool-timeout", 30*time.Second, "end-to-end deadline for one tool call, including retries")
		logLevel  = fs.String("log-level", "info", "debug, info, warn or error")
	)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: stripe-mcp [flags]")
		fmt.Fprintln(stderr, "\nEnvironment:")
		fmt.Fprintln(stderr, "  STRIPE_API_KEY       Stripe secret or restricted key (required; test mode unless -allow-live)")
		fmt.Fprintln(stderr, "  STRIPE_API_VERSION   optional Stripe-Version to pin, e.g. a dated API version")
		fmt.Fprintln(stderr, "  STRIPE_API_BASE      optional API base URL, for testing against a fake")
		fmt.Fprintln(stderr, "  STRIPE_MCP_READ_ONLY set to true to disable write tools")
		fmt.Fprintln(stderr, "  MCP_HTTP_TOKEN       bearer token required from HTTP clients (with -http)")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg := &Config{
		APIKey:      Secret(strings.TrimSpace(getenv("STRIPE_API_KEY"))),
		BaseURL:     strings.TrimSpace(getenv("STRIPE_API_BASE")),
		APIVersion:  strings.TrimSpace(getenv("STRIPE_API_VERSION")),
		ReadOnly:    *readOnly,
		ToolTimeout: *timeout,
		HTTPAddr:    *httpAddr,
		HTTPToken:   Secret(strings.TrimSpace(getenv("MCP_HTTP_TOKEN"))),
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return nil, fmt.Errorf("invalid -log-level %q", *logLevel)
	}
	if cfg.ToolTimeout <= 0 {
		return nil, errors.New("-tool-timeout must be positive")
	}

	live, err := keyMode(cfg.APIKey.Reveal())
	if err != nil {
		return nil, err
	}
	cfg.Livemode = live
	if live {
		if !*allowLive {
			return nil, errors.New("STRIPE_API_KEY is a live-mode key; use a test-mode key, or pass -allow-live to run read-only against live data")
		}
		// Writes against live money are out of scope for this server.
		cfg.ReadOnly = true
	}

	if cfg.BaseURL != "" {
		if err := checkBaseURL(cfg.BaseURL); err != nil {
			return nil, err
		}
	}
	if cfg.HTTPAddr != "" && cfg.HTTPToken == "" && !isLoopbackAddr(cfg.HTTPAddr) {
		return nil, errors.New("-http on a non-loopback address requires MCP_HTTP_TOKEN: the server holds a Stripe key and must not be reachable unauthenticated")
	}
	return cfg, nil
}

// keyMode reports whether key is a live-mode key, rejecting anything that
// is not recognisably a Stripe secret or restricted key. The error never
// includes the key.
func keyMode(key string) (live bool, err error) {
	switch {
	case key == "":
		return false, errors.New("STRIPE_API_KEY is not set")
	case strings.HasPrefix(key, "sk_test_"), strings.HasPrefix(key, "rk_test_"):
		return false, nil
	case strings.HasPrefix(key, "sk_live_"), strings.HasPrefix(key, "rk_live_"):
		return true, nil
	case strings.HasPrefix(key, "pk_"):
		return false, errors.New("STRIPE_API_KEY is a publishable key; a secret (sk_test_...) or restricted (rk_test_...) key is required")
	default:
		return false, errors.New("STRIPE_API_KEY is not a recognised Stripe key; expected sk_test_... or rk_test_...")
	}
}

// checkBaseURL allows plain HTTP only for loopback hosts, so an override
// cannot send the API key over the network in clear text.
func checkBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("STRIPE_API_BASE is not a valid URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.New("STRIPE_API_BASE must use https unless it points at localhost")
	default:
		return errors.New("STRIPE_API_BASE must be an http(s) URL")
	}
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
