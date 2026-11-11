// Package stripe is a small, dependency-free client for the subset of the
// Stripe REST API that the MCP tools need.
//
// It is deliberately not a general SDK. It exists to show the transport
// concerns an integration has to get right: per-attempt timeouts, bounded
// retries with jittered backoff, Retry-After handling, idempotency keys on
// writes, bounded response bodies and typed errors.
package stripe

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the production Stripe API host. Test-mode and
	// live-mode requests use the same host; the key decides the mode.
	DefaultBaseURL = "https://api.stripe.com"

	maxResponseBytes = 1 << 20 // 1 MiB; list pages are capped at 100 items.
	userAgent        = "stripe-mcp-go"
)

// Options configures a Client. Zero values select sensible defaults.
type Options struct {
	BaseURL    string       // default DefaultBaseURL
	APIVersion string       // optional Stripe-Version header; empty uses the account default
	HTTPClient *http.Client // default: a client with no global timeout (per-attempt timeouts apply)
	Logger     *slog.Logger // default: discard

	MaxAttempts    int           // total attempts including the first; default 3
	AttemptTimeout time.Duration // per-attempt timeout; default 10s
	BaseBackoff    time.Duration // first retry delay before jitter; default 500ms
	MaxBackoff     time.Duration // cap on a single wait, including Retry-After; default 8s
}

// Client calls the Stripe API. It is safe for concurrent use.
type Client struct {
	key  string
	opts Options
	// sleep is replaceable in tests so retry tests do not wait on real time.
	sleep func(ctx context.Context, d time.Duration) error
}

// New returns a Client that authenticates with the given secret key.
func New(key string, opts Options) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	opts.BaseURL = strings.TrimRight(opts.BaseURL, "/")
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = 10 * time.Second
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = 500 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 8 * time.Second
	}
	return &Client{key: key, opts: opts, sleep: sleepCtx}
}

// Error is a failed Stripe API call. It never contains the API key.
type Error struct {
	HTTPStatus int           // 0 when no response was received
	Type       string        // Stripe error type, e.g. "invalid_request_error"
	Code       string        // Stripe error code, e.g. "resource_missing"
	Message    string        // human-readable message
	Param      string        // offending request parameter, if Stripe named one
	RequestID  string        // Stripe Request-Id header, for support and log correlation
	RetryAfter time.Duration // server-requested wait, if any
	Retryable  bool          // whether the same call may succeed if repeated later
	Attempts   int           // attempts made before giving up
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("stripe: ")
	if e.HTTPStatus != 0 {
		fmt.Fprintf(&b, "http %d: ", e.HTTPStatus)
	}
	b.WriteString(e.Message)
	if e.Code != "" {
		fmt.Fprintf(&b, " (code %s)", e.Code)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " [request %s]", e.RequestID)
	}
	return b.String()
}

// NewIdempotencyKey returns a random key suitable for the Idempotency-Key
// header. One key must cover all attempts of one logical write.
func NewIdempotencyKey() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; a time-based
		// key is still unique enough to keep retries of one call together.
		return "mcp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "mcp-" + hex.EncodeToString(b[:])
}

// get performs a GET with query parameters and decodes the response into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, "", out)
}

// post performs a form-encoded POST. idempotencyKey must be non-empty: it is
// what makes retrying a write safe.
func (c *Client) post(ctx context.Context, path string, form url.Values, idempotencyKey string, out any) error {
	if idempotencyKey == "" {
		return &Error{Type: "client_error", Message: "refusing to send a write without an idempotency key"}
	}
	return c.do(ctx, http.MethodPost, path, form, idempotencyKey, out)
}

func (c *Client) do(ctx context.Context, method, path string, params url.Values, idempotencyKey string, out any) error {
	var last *Error
	for attempt := 1; ; attempt++ {
		last = c.attempt(ctx, method, path, params, idempotencyKey, out)
		if last == nil {
			return nil
		}
		last.Attempts = attempt
		if !last.Retryable || attempt >= c.opts.MaxAttempts || ctx.Err() != nil {
			return last
		}

		wait := c.backoff(attempt)
		if last.RetryAfter > wait {
			wait = last.RetryAfter
		}
		if wait > c.opts.MaxBackoff {
			wait = c.opts.MaxBackoff
		}
		// Do not start a wait the caller's deadline cannot cover.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
			return last
		}
		c.opts.Logger.LogAttrs(ctx, slog.LevelWarn, "stripe request failed, retrying",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("attempt", attempt),
			slog.Int("http_status", last.HTTPStatus),
			slog.String("request_id", last.RequestID),
			slog.Duration("wait", wait),
		)
		if err := c.sleep(ctx, wait); err != nil {
			return last
		}
	}
}

// attempt makes one HTTP round trip. It returns nil on a 2xx that decoded.
func (c *Client) attempt(ctx context.Context, method, path string, params url.Values, idempotencyKey string, out any) *Error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.AttemptTimeout)
	defer cancel()

	target := c.opts.BaseURL + path
	var body io.Reader
	if method == http.MethodGet {
		if len(params) > 0 {
			target += "?" + params.Encode()
		}
	} else {
		body = strings.NewReader(params.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return &Error{Type: "client_error", Message: "building request: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if c.opts.APIVersion != "" {
		req.Header.Set("Stripe-Version", c.opts.APIVersion)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()

	requestID := resp.Header.Get("Request-Id")
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		e := transportError(err)
		e.RequestID = requestID
		return e
	}
	if len(raw) > maxResponseBytes {
		return &Error{HTTPStatus: resp.StatusCode, Type: "client_error", RequestID: requestID,
			Message: "response body exceeded the 1 MiB limit"}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return &Error{HTTPStatus: resp.StatusCode, Type: "client_error", RequestID: requestID,
				Message: "decoding response: " + err.Error()}
		}
		return nil
	}
	return apiError(resp, raw, requestID)
}

// apiError converts a non-2xx response into an Error and decides whether it
// is worth retrying.
func apiError(resp *http.Response, raw []byte, requestID string) *Error {
	e := &Error{HTTPStatus: resp.StatusCode, RequestID: requestID}

	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Message != "" {
		e.Type = envelope.Error.Type
		e.Code = envelope.Error.Code
		e.Message = envelope.Error.Message
		e.Param = envelope.Error.Param
	} else {
		// Proxies and load balancers answer with non-JSON bodies. The body
		// is not echoed: it is unstructured and of no use to the caller.
		e.Type = "api_error"
		e.Message = "unexpected response: " + http.StatusText(resp.StatusCode)
	}

	if s := resp.Header.Get("Retry-After"); s != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	// Stripe states its own opinion in Stripe-Should-Retry; honour it when
	// present, otherwise fall back to the status code.
	switch resp.Header.Get("Stripe-Should-Retry") {
	case "true":
		e.Retryable = true
	case "false":
		e.Retryable = false
	default:
		e.Retryable = resp.StatusCode == http.StatusConflict ||
			resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode >= 500
	}
	return e
}

// transportError describes a failure with no usable HTTP response. The
// underlying error is unwrapped from *url.Error so the message carries the
// cause without repeating the request URL.
func transportError(err error) *Error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	e := &Error{Type: "connection_error", Message: err.Error(), Retryable: true}
	if errors.Is(err, context.DeadlineExceeded) {
		e.Type = "timeout"
		e.Message = "request to Stripe timed out"
	}
	if errors.Is(err, context.Canceled) {
		e.Type = "cancelled"
		e.Message = "request cancelled"
		e.Retryable = false
	}
	return e
}

// backoff returns the wait before the next attempt: exponential with full
// jitter, so that concurrent callers do not retry in step.
func (c *Client) backoff(attempt int) time.Duration {
	ceiling := c.opts.BaseBackoff << (attempt - 1)
	if ceiling <= 0 || ceiling > c.opts.MaxBackoff {
		ceiling = c.opts.MaxBackoff
	}
	return time.Duration(mathrand.Int64N(int64(ceiling)) + 1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
