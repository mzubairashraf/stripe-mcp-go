package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mzubairashraf/stripe-mcp-go/internal/stripe"
	"github.com/mzubairashraf/stripe-mcp-go/internal/stripetest"
	"github.com/mzubairashraf/stripe-mcp-go/internal/tools"
)

// harness wires a real MCP client to the server over an in-memory transport,
// with the Stripe client pointed at the fake. Tests therefore exercise the
// full path: schema validation, handler, HTTP client, error mapping.
type harness struct {
	t       *testing.T
	fake    *stripetest.Fake
	session *mcp.ClientSession
	logs    *bytes.Buffer
}

func newHarness(t *testing.T, opts tools.Options) *harness {
	t.Helper()
	fake := stripetest.New()
	t.Cleanup(fake.Close)

	logs := &bytes.Buffer{}
	opts.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	api := stripe.New(stripetest.Key, stripe.Options{
		BaseURL: fake.URL, Logger: opts.Logger,
		BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
	})

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tools.Register(server, api, opts)

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = session.Close()
		_ = serverSession.Wait()
	})
	return &harness{t: t, fake: fake, session: session, logs: logs}
}

// call invokes a tool and returns its structured output, or the structured
// error when the tool reported one.
func (h *harness) call(name string, args map[string]any) (out map[string]any, toolErr *tools.Error) {
	h.t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: protocol error: %v", name, err)
	}
	if len(res.Content) == 0 {
		h.t.Fatalf("%s: empty content", name)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError {
		var env struct {
			Error *tools.Error `json:"error"`
		}
		if err := json.Unmarshal([]byte(text), &env); err != nil || env.Error == nil {
			h.t.Fatalf("%s: error is not structured JSON: %s", name, text)
		}
		return nil, env.Error
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatalf("%s: structured content: %v", name, err)
	}
	// The text block must carry the same JSON for clients without
	// structured-content support.
	var fromText map[string]any
	if err := json.Unmarshal([]byte(text), &fromText); err != nil {
		h.t.Fatalf("%s: text content is not JSON: %s", name, text)
	}
	return out, nil
}

func (h *harness) mustCall(name string, args map[string]any) map[string]any {
	h.t.Helper()
	out, toolErr := h.call(name, args)
	if toolErr != nil {
		h.t.Fatalf("%s: unexpected tool error: %+v", name, toolErr)
	}
	return out
}

func TestToolListing(t *testing.T) {
	readTools := []string{tools.GetBalance, tools.ListCustomers, tools.GetCustomer,
		tools.ListPaymentIntents, tools.GetPaymentIntent, tools.ListInvoices}
	writeTools := []string{tools.CreateCustomer, tools.CreateRefund}

	tests := []struct {
		name     string
		readOnly bool
		want     []string
	}{
		{name: "default exposes reads and writes", want: slices.Concat(readTools, writeTools)},
		{name: "read-only hides writes", readOnly: true, want: readTools},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tools.Options{ReadOnly: tt.readOnly})
			res, err := h.session.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, tool := range res.Tools {
				got = append(got, tool.Name)
				if tool.Description == "" || tool.InputSchema == nil || tool.OutputSchema == nil {
					t.Errorf("%s: missing description or schema", tool.Name)
				}
				isWrite := slices.Contains(writeTools, tool.Name)
				if tool.Annotations == nil || tool.Annotations.ReadOnlyHint == isWrite {
					t.Errorf("%s: readOnlyHint annotation is wrong", tool.Name)
				}
			}
			slices.Sort(got)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("tools = %v, want %v", got, want)
			}
		})
	}
}

func TestReadOnlyServerRejectsWriteCalls(t *testing.T) {
	h := newHarness(t, tools.Options{ReadOnly: true})
	_, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: tools.CreateRefund, Arguments: map[string]any{"payment_intent_id": stripetest.PaymentPaid},
	})
	if err == nil {
		t.Fatal("expected a protocol error for an unregistered tool")
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("requests reaching Stripe = %d, want 0", n)
	}
}

func TestReadTools(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		args  map[string]any
		check func(t *testing.T, out map[string]any)
	}{
		{name: "balance", tool: tools.GetBalance, check: func(t *testing.T, out map[string]any) {
			avail := out["available"].([]any)[0].(map[string]any)
			if avail["amount"] != 125000.0 || avail["currency"] != "usd" || out["livemode"] != false {
				t.Errorf("out = %v", out)
			}
		}},
		{name: "customers default page", tool: tools.ListCustomers, check: func(t *testing.T, out map[string]any) {
			if n := len(out["customers"].([]any)); n != 3 || out["has_more"] != false {
				t.Errorf("customers = %d, has_more = %v", n, out["has_more"])
			}
			if _, ok := out["next_cursor"]; ok {
				t.Error("next_cursor must be absent on the last page")
			}
		}},
		{name: "customers first page of two", tool: tools.ListCustomers, args: map[string]any{"limit": 2},
			check: func(t *testing.T, out map[string]any) {
				if len(out["customers"].([]any)) != 2 || out["has_more"] != true || out["next_cursor"] != stripetest.CustomerGrace {
					t.Errorf("out = %v", out)
				}
			}},
		{name: "customers second page", tool: tools.ListCustomers,
			args: map[string]any{"limit": 2, "starting_after": stripetest.CustomerGrace},
			check: func(t *testing.T, out map[string]any) {
				cs := out["customers"].([]any)
				if len(cs) != 1 || cs[0].(map[string]any)["id"] != stripetest.CustomerAda || out["has_more"] != false {
					t.Errorf("out = %v", out)
				}
			}},
		{name: "customers by email", tool: tools.ListCustomers, args: map[string]any{"email": "grace@example.com"},
			check: func(t *testing.T, out map[string]any) {
				cs := out["customers"].([]any)
				if len(cs) != 1 || cs[0].(map[string]any)["id"] != stripetest.CustomerGrace {
					t.Errorf("out = %v", out)
				}
			}},
		{name: "customers no match is an empty array", tool: tools.ListCustomers, args: map[string]any{"email": "nobody@example.com"},
			check: func(t *testing.T, out map[string]any) {
				if cs, ok := out["customers"].([]any); !ok || len(cs) != 0 {
					t.Errorf("customers = %#v, want empty array", out["customers"])
				}
			}},
		{name: "customer by id", tool: tools.GetCustomer, args: map[string]any{"customer_id": stripetest.CustomerGrace},
			check: func(t *testing.T, out map[string]any) {
				if out["email"] != "grace@example.com" || out["delinquent"] != true ||
					out["created"] != "2025-11-04T00:00:00Z" || out["metadata"].(map[string]any)["crm_id"] != "42" {
					t.Errorf("out = %v", out)
				}
			}},
		{name: "payment intents for customer", tool: tools.ListPaymentIntents, args: map[string]any{"customer_id": stripetest.CustomerAda},
			check: func(t *testing.T, out map[string]any) {
				pis := out["payment_intents"].([]any)
				if len(pis) != 1 || pis[0].(map[string]any)["status"] != "succeeded" {
					t.Errorf("out = %v", out)
				}
			}},
		{name: "declined payment exposes the decline", tool: tools.GetPaymentIntent, args: map[string]any{"payment_intent_id": stripetest.PaymentDeclined},
			check: func(t *testing.T, out map[string]any) {
				lastErr, _ := out["last_error"].(map[string]any)
				if out["amount"] != 9900.0 || out["amount_received"] != 0.0 || lastErr["code"] != "card_declined" {
					t.Errorf("out = %v", out)
				}
			}},
		{name: "open invoices", tool: tools.ListInvoices, args: map[string]any{"status": "open"},
			check: func(t *testing.T, out map[string]any) {
				invs := out["invoices"].([]any)
				if len(invs) != 1 {
					t.Fatalf("invoices = %d, want 1", len(invs))
				}
				inv := invs[0].(map[string]any)
				if inv["id"] != stripetest.InvoiceOpen || inv["amount_remaining"] != 9900.0 || inv["due_date"] != "2025-11-18T00:00:00Z" {
					t.Errorf("invoice = %v", inv)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tools.Options{})
			tt.check(t, h.mustCall(tt.tool, tt.args))
		})
	}
}

// Invalid input must be rejected with the offending parameter named, and
// must never reach the API.
func TestInputValidation(t *testing.T) {
	long := strings.Repeat("x", 501)
	tests := []struct {
		name      string
		tool      string
		args      map[string]any
		wantParam string
	}{
		{"limit too large", tools.ListCustomers, map[string]any{"limit": 101}, "limit"},
		{"limit negative", tools.ListInvoices, map[string]any{"limit": -1}, "limit"},
		{"email malformed", tools.ListCustomers, map[string]any{"email": "not-an-email"}, "email"},
		{"email with display name", tools.ListCustomers, map[string]any{"email": "Ada <ada@example.com>"}, "email"},
		{"cursor of wrong type", tools.ListCustomers, map[string]any{"starting_after": "pi_123"}, "starting_after"},
		{"customer id empty", tools.GetCustomer, map[string]any{"customer_id": ""}, "customer_id"},
		{"customer id wrong prefix", tools.GetCustomer, map[string]any{"customer_id": "pi_123"}, "customer_id"},
		{"customer id path traversal", tools.GetCustomer, map[string]any{"customer_id": "cus_1/../../v1/refunds"}, "customer_id"},
		{"payment intent id with query", tools.GetPaymentIntent, map[string]any{"payment_intent_id": "pi_1?expand[]=x"}, "payment_intent_id"},
		{"invoice status unknown", tools.ListInvoices, map[string]any{"status": "overdue"}, "status"},
		{"create customer with nothing", tools.CreateCustomer, map[string]any{"description": "x"}, "email"},
		{"create customer bad email", tools.CreateCustomer, map[string]any{"email": "a@"}, "email"},
		{"create customer metadata value too long", tools.CreateCustomer, map[string]any{"name": "A", "metadata": map[string]any{"k": long}}, "metadata"},
		{"create customer metadata key with brackets", tools.CreateCustomer, map[string]any{"name": "A", "metadata": map[string]any{"a[b]": "v"}}, "metadata"},
		{"idempotency key with newline", tools.CreateCustomer, map[string]any{"name": "A", "idempotency_key": "a\nX-Evil: 1"}, "idempotency_key"},
		{"refund negative amount", tools.CreateRefund, map[string]any{"payment_intent_id": stripetest.PaymentPaid, "amount": -5}, "amount"},
		{"refund unknown reason", tools.CreateRefund, map[string]any{"payment_intent_id": stripetest.PaymentPaid, "reason": "because"}, "reason"},
		{"refund of a customer id", tools.CreateRefund, map[string]any{"payment_intent_id": stripetest.CustomerAda}, "payment_intent_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tools.Options{})
			_, toolErr := h.call(tt.tool, tt.args)
			if toolErr == nil {
				t.Fatal("expected a tool error")
			}
			if toolErr.Kind != tools.KindInvalidInput || toolErr.Param != tt.wantParam || toolErr.Retryable {
				t.Errorf("error = %+v, want invalid_input on %q", toolErr, tt.wantParam)
			}
			if n := len(h.fake.Requests()); n != 0 {
				t.Errorf("requests reaching Stripe = %d, want 0", n)
			}
		})
	}
}

// Arguments of the wrong JSON type are rejected by the SDK against the
// inferred schema, before the handler runs.
func TestSchemaRejectsWrongTypes(t *testing.T) {
	h := newHarness(t, tools.Options{})
	for name, args := range map[string]map[string]any{
		"string for integer": {"limit": "ten"},
		"unknown property":   {"emial": "ada@example.com"},
	} {
		res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: tools.ListCustomers, Arguments: args})
		if err != nil {
			t.Fatalf("%s: protocol error: %v", name, err)
		}
		if !res.IsError {
			t.Errorf("%s: expected a tool error", name)
		}
	}
	if n := len(h.fake.Requests()); n != 0 {
		t.Errorf("requests reaching Stripe = %d, want 0", n)
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	rateLimited := stripetest.Fault{Status: http.StatusTooManyRequests,
		Body: `{"error":{"type":"rate_limit_error","code":"rate_limit","message":"Too many requests"}}`}
	tests := []struct {
		name         string
		faults       []stripetest.Fault
		tool         string
		args         map[string]any
		wantKind     string
		wantCode     string
		wantRetry    bool
		wantRequests int
	}{
		{name: "missing customer", tool: tools.GetCustomer, args: map[string]any{"customer_id": "cus_missing"},
			wantKind: tools.KindNotFound, wantCode: "resource_missing", wantRequests: 1},
		{name: "refund of a failed payment", tool: tools.CreateRefund, args: map[string]any{"payment_intent_id": stripetest.PaymentDeclined},
			wantKind: tools.KindInvalidRequest, wantCode: "payment_intent_unexpected_state", wantRequests: 1},
		{name: "refund larger than the payment", tool: tools.CreateRefund, args: map[string]any{"payment_intent_id": stripetest.PaymentPaid, "amount": 999999},
			wantKind: tools.KindInvalidRequest, wantCode: "amount_too_large", wantRequests: 1},
		{name: "rate limit outlasts retries", faults: []stripetest.Fault{rateLimited, rateLimited, rateLimited}, tool: tools.GetBalance,
			wantKind: tools.KindRateLimited, wantCode: "rate_limit", wantRetry: true, wantRequests: 3},
		{name: "persistent outage", faults: []stripetest.Fault{{Status: 503}, {Status: 503}, {Status: 503}}, tool: tools.GetBalance,
			wantKind: tools.KindUpstream, wantRetry: true, wantRequests: 3},
		{name: "rejected key", faults: []stripetest.Fault{{Status: 401, Body: `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided: sk_test_****fake"}}`}},
			tool: tools.GetBalance, wantKind: tools.KindAuthentication, wantRequests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tools.Options{})
			h.fake.FailNext(tt.faults...)
			_, toolErr := h.call(tt.tool, tt.args)
			if toolErr == nil {
				t.Fatal("expected a tool error")
			}
			if toolErr.Kind != tt.wantKind || toolErr.StripeCode != tt.wantCode || toolErr.Retryable != tt.wantRetry {
				t.Errorf("error = %+v, want kind=%s code=%q retryable=%v", toolErr, tt.wantKind, tt.wantCode, tt.wantRetry)
			}
			if toolErr.StripeRequestID == "" {
				t.Error("stripe_request_id must be set for upstream errors")
			}
			if strings.Contains(toolErr.Message, "sk_") {
				t.Errorf("message echoes key material: %q", toolErr.Message)
			}
			if n := len(h.fake.Requests()); n != tt.wantRequests {
				t.Errorf("requests = %d, want %d", n, tt.wantRequests)
			}
		})
	}
}

func TestTransientFailureIsRetriedTransparently(t *testing.T) {
	h := newHarness(t, tools.Options{})
	h.fake.FailNext(stripetest.Fault{Status: 500})
	out := h.mustCall(tools.GetCustomer, map[string]any{"customer_id": stripetest.CustomerAda})
	if out["id"] != stripetest.CustomerAda || len(h.fake.Requests()) != 2 {
		t.Errorf("out = %v, requests = %d", out, len(h.fake.Requests()))
	}
}

func TestToolTimeout(t *testing.T) {
	h := newHarness(t, tools.Options{Timeout: time.Nanosecond})
	_, toolErr := h.call(tools.GetBalance, nil)
	if toolErr == nil || toolErr.Kind != tools.KindTimeout {
		t.Fatalf("error = %+v, want timeout", toolErr)
	}
}

func TestCreateCustomerIsIdempotent(t *testing.T) {
	h := newHarness(t, tools.Options{})
	args := map[string]any{"email": "new@example.com", "name": "New Customer",
		"metadata": map[string]any{"source": "test"}, "idempotency_key": "onboard-42"}

	first := h.mustCall(tools.CreateCustomer, args)
	second := h.mustCall(tools.CreateCustomer, args)

	id := first["customer"].(map[string]any)["id"]
	if id == "" || id != second["customer"].(map[string]any)["id"] {
		t.Errorf("ids differ: %v vs %v", id, second["customer"].(map[string]any)["id"])
	}
	if first["idempotency_key"] != "onboard-42" {
		t.Errorf("idempotency_key = %v", first["idempotency_key"])
	}
	all := h.mustCall(tools.ListCustomers, nil)
	if n := len(all["customers"].([]any)); n != 4 {
		t.Errorf("customers after two identical creates = %d, want 4", n)
	}
}

func TestCreateCustomerGeneratesAKeyWhenNoneGiven(t *testing.T) {
	h := newHarness(t, tools.Options{})
	h.fake.FailNext(stripetest.Fault{Status: 500})
	out := h.mustCall(tools.CreateCustomer, map[string]any{"name": "No Key"})

	reqs := h.fake.Requests()
	key, _ := out["idempotency_key"].(string)
	if len(reqs) != 2 || key == "" || reqs[0].IdempotencyKey != key || reqs[1].IdempotencyKey != key {
		t.Errorf("requests = %+v, returned key = %q", reqs, key)
	}
}

func TestRefundFlow(t *testing.T) {
	h := newHarness(t, tools.Options{})

	partial := h.mustCall(tools.CreateRefund, map[string]any{
		"payment_intent_id": stripetest.PaymentPaid, "amount": 1000, "reason": "requested_by_customer"})
	if partial["amount"] != 1000.0 || partial["status"] != "succeeded" || partial["reason"] != "requested_by_customer" {
		t.Errorf("partial = %v", partial)
	}

	rest := h.mustCall(tools.CreateRefund, map[string]any{"payment_intent_id": stripetest.PaymentPaid})
	if rest["amount"] != 3250.0 {
		t.Errorf("full refund of remainder = %v, want 3250", rest["amount"])
	}

	_, toolErr := h.call(tools.CreateRefund, map[string]any{"payment_intent_id": stripetest.PaymentPaid})
	if toolErr == nil || toolErr.StripeCode != "charge_already_refunded" || toolErr.Retryable {
		t.Errorf("third refund error = %+v", toolErr)
	}
}

func TestLogsCarryNoSecretsOrArguments(t *testing.T) {
	h := newHarness(t, tools.Options{})
	h.mustCall(tools.CreateCustomer, map[string]any{"email": "private@example.com", "name": "Private Person"})
	h.fake.FailNext(stripetest.Fault{Status: 500})
	h.mustCall(tools.ListCustomers, map[string]any{"email": "private@example.com"})
	h.call(tools.GetCustomer, map[string]any{"customer_id": "cus_missing"})

	logs := h.logs.String()
	for _, secret := range []string{stripetest.Key, "private@example.com", "Private Person"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs)
		}
	}
	for _, want := range []string{`"tool":"create_customer"`, `"outcome":"ok"`, `"error_kind":"not_found"`, `"stripe_request_id":"req_fake`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %s:\n%s", want, logs)
		}
	}
}
