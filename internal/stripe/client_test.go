package stripe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mzubairashraf/stripe-mcp-go/internal/stripetest"
)

// newTestClient returns a client pointed at the fake whose waits are
// recorded instead of slept.
func newTestClient(t *testing.T, fake *stripetest.Fake, opts Options) (*Client, *[]time.Duration) {
	t.Helper()
	t.Cleanup(fake.Close)
	opts.BaseURL = fake.URL
	c := New(stripetest.Key, opts)
	var waits []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	return c, &waits
}

func TestRetryPolicy(t *testing.T) {
	rateLimited := stripetest.Fault{
		Status: http.StatusTooManyRequests,
		Header: map[string]string{"Retry-After": "2"},
		Body:   `{"error":{"type":"rate_limit_error","code":"rate_limit","message":"Too many requests"}}`,
	}
	tests := []struct {
		name         string
		faults       []stripetest.Fault
		wantRequests int
		wantErr      bool
		wantStatus   int
		wantRetry    bool          // Error.Retryable on failure
		wantMinWait  time.Duration // lower bound on the first wait
	}{
		{name: "success first time", wantRequests: 1},
		{name: "500 then success", faults: []stripetest.Fault{{Status: 500}}, wantRequests: 2},
		{name: "503 twice then success", faults: []stripetest.Fault{{Status: 503}, {Status: 503}}, wantRequests: 3},
		{name: "429 honours Retry-After", faults: []stripetest.Fault{rateLimited}, wantRequests: 2, wantMinWait: 2 * time.Second},
		{name: "429 until attempts exhausted", faults: []stripetest.Fault{rateLimited, rateLimited, rateLimited},
			wantRequests: 3, wantErr: true, wantStatus: 429, wantRetry: true},
		{name: "400 is not retried", faults: []stripetest.Fault{{Status: 400}}, wantRequests: 1, wantErr: true, wantStatus: 400},
		{name: "409 is retried", faults: []stripetest.Fault{{Status: 409}}, wantRequests: 2},
		{name: "Stripe-Should-Retry false overrides 500",
			faults:       []stripetest.Fault{{Status: 500, Header: map[string]string{"Stripe-Should-Retry": "false"}}},
			wantRequests: 1, wantErr: true, wantStatus: 500},
		{name: "Stripe-Should-Retry true overrides 400",
			faults:       []stripetest.Fault{{Status: 400, Header: map[string]string{"Stripe-Should-Retry": "true"}}},
			wantRequests: 2},
		{name: "non-JSON gateway error is retried",
			faults:       []stripetest.Fault{{Status: 502, Body: "<html>Bad Gateway</html>"}},
			wantRequests: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := stripetest.New()
			c, waits := newTestClient(t, fake, Options{})
			fake.FailNext(tt.faults...)

			_, err := c.GetBalance(context.Background())

			if got := len(fake.Requests()); got != tt.wantRequests {
				t.Errorf("requests = %d, want %d", got, tt.wantRequests)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantMinWait > 0 && (len(*waits) == 0 || (*waits)[0] < tt.wantMinWait) {
				t.Errorf("waits = %v, want first >= %v", *waits, tt.wantMinWait)
			}
			if err == nil {
				return
			}
			var se *Error
			if !errors.As(err, &se) {
				t.Fatalf("error type = %T, want *Error", err)
			}
			if se.HTTPStatus != tt.wantStatus || se.Retryable != tt.wantRetry || se.Attempts != tt.wantRequests {
				t.Errorf("got status=%d retryable=%v attempts=%d, want status=%d retryable=%v attempts=%d",
					se.HTTPStatus, se.Retryable, se.Attempts, tt.wantStatus, tt.wantRetry, tt.wantRequests)
			}
		})
	}
}

func TestBackoffIsBoundedAndGrows(t *testing.T) {
	c := New("sk_test_x", Options{BaseBackoff: 100 * time.Millisecond, MaxBackoff: time.Second})
	for attempt, ceiling := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 3: 400 * time.Millisecond, 10: time.Second, 80: time.Second} {
		for range 200 {
			if d := c.backoff(attempt); d <= 0 || d > ceiling {
				t.Fatalf("backoff(%d) = %v, want in (0, %v]", attempt, d, ceiling)
			}
		}
	}
}

func TestWriteKeepsOneIdempotencyKeyAcrossRetries(t *testing.T) {
	fake := stripetest.New()
	c, _ := newTestClient(t, fake, Options{})
	fake.FailNext(stripetest.Fault{Status: 500}, stripetest.Fault{Status: 503})

	cust, err := c.CreateCustomer(context.Background(), CustomerParams{
		Email: "new@example.com", Metadata: map[string]string{"ref": "A1"},
	}, "key-123")
	if err != nil {
		t.Fatal(err)
	}
	if cust.Email != "new@example.com" || cust.Metadata["ref"] != "A1" {
		t.Errorf("customer = %+v", cust)
	}
	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	for i, r := range reqs {
		if r.IdempotencyKey != "key-123" {
			t.Errorf("request %d idempotency key = %q", i, r.IdempotencyKey)
		}
	}
	if got := reqs[2].Form["metadata[ref]"]; got != "A1" {
		t.Errorf("metadata form field = %q", got)
	}
}

func TestWriteWithoutIdempotencyKeyIsRefused(t *testing.T) {
	fake := stripetest.New()
	c, _ := newTestClient(t, fake, Options{})
	if _, err := c.CreateCustomer(context.Background(), CustomerParams{Name: "x"}, ""); err == nil {
		t.Fatal("expected an error")
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

func TestErrorDecoding(t *testing.T) {
	fake := stripetest.New()
	c, _ := newTestClient(t, fake, Options{})

	_, err := c.GetCustomer(context.Background(), "cus_missing")
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *Error", err)
	}
	want := Error{HTTPStatus: 404, Type: "invalid_request_error", Code: "resource_missing", Param: "id",
		Message: "No such customer: 'cus_missing'", RequestID: "req_fake0001", Attempts: 1}
	if *se != want {
		t.Errorf("got  %+v\nwant %+v", *se, want)
	}
}

func TestRequestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New("sk_test_abc", Options{BaseURL: srv.URL + "/", APIVersion: "2020-08-27"})
	if _, err := c.GetBalance(context.Background()); err != nil {
		t.Fatal(err)
	}
	for header, want := range map[string]string{
		"Authorization":  "Bearer sk_test_abc",
		"Stripe-Version": "2020-08-27",
		"User-Agent":     "stripe-mcp-go",
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, want %q", header, got.Get(header), want)
		}
	}
}

func TestAttemptTimeoutIsRetriedThenReported(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := New("sk_test_abc", Options{BaseURL: srv.URL, AttemptTimeout: 30 * time.Millisecond, MaxAttempts: 2, BaseBackoff: time.Millisecond})
	_, err := c.GetBalance(context.Background())

	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if se.Type != "timeout" || se.Attempts != 2 || calls.Load() != 2 {
		t.Errorf("type=%q attempts=%d calls=%d, want timeout/2/2", se.Type, se.Attempts, calls.Load())
	}
}

func TestCancelledContextIsNotRetried(t *testing.T) {
	fake := stripetest.New()
	c, _ := newTestClient(t, fake, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.GetBalance(ctx)
	var se *Error
	if !errors.As(err, &se) || se.Type != "cancelled" || se.Retryable {
		t.Fatalf("err = %+v, want non-retryable cancelled", err)
	}
	if n := len(fake.Requests()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

func TestDoesNotWaitPastCallerDeadline(t *testing.T) {
	fake := stripetest.New()
	c, waits := newTestClient(t, fake, Options{})
	fake.FailNext(stripetest.Fault{Status: 429, Header: map[string]string{"Retry-After": "5"}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := c.GetBalance(ctx)

	var se *Error
	if !errors.As(err, &se) || se.HTTPStatus != 429 {
		t.Fatalf("err = %v, want the 429", err)
	}
	if len(*waits) != 0 || len(fake.Requests()) != 1 {
		t.Errorf("waits=%v requests=%d, want no wait and one request", *waits, len(fake.Requests()))
	}
}

func TestErrorsNeverContainTheKey(t *testing.T) {
	const key = "sk_test_supersecret"
	fake := stripetest.New()
	defer fake.Close()

	// Wrong key against the fake (401), and an unreachable host (transport error).
	for name, c := range map[string]*Client{
		"unauthorized": New(key, Options{BaseURL: fake.URL}),
		"unreachable":  New(key, Options{BaseURL: "http://127.0.0.1:1", MaxAttempts: 1}),
	} {
		_, err := c.GetBalance(context.Background())
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("%s: error discloses the key: %v", name, err)
		}
	}
}

func TestNewIdempotencyKeyIsUnique(t *testing.T) {
	a, b := NewIdempotencyKey(), NewIdempotencyKey()
	if a == b || len(a) < 20 {
		t.Errorf("keys %q, %q", a, b)
	}
}
