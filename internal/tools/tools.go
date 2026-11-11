// Package tools defines the MCP tools and maps them onto the Stripe client.
//
// Each tool has a typed input and output struct. The SDK derives the JSON
// Schemas from those types and validates arguments against them before a
// handler runs; the handlers then apply the semantic checks a schema cannot
// express (ID formats, bounds, enum values) before any request leaves the
// process.
package tools

import (
	"context"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mzubairashraf/stripe-mcp-go/internal/stripe"
)

// Options configures tool registration.
type Options struct {
	// ReadOnly leaves the write tools unregistered. A client connected to a
	// read-only server cannot see or call them.
	ReadOnly bool
	// Timeout bounds one tool call end to end, including retries.
	Timeout time.Duration
	Logger  *slog.Logger
}

// Tool names.
const (
	GetBalance         = "get_balance"
	ListCustomers      = "list_customers"
	GetCustomer        = "get_customer"
	ListPaymentIntents = "list_payment_intents"
	GetPaymentIntent   = "get_payment_intent"
	ListInvoices       = "list_invoices"
	CreateCustomer     = "create_customer"
	CreateRefund       = "create_refund"
)

// Instructions is sent to clients at initialisation as server-level guidance.
const Instructions = "Tools for a Stripe account. Monetary amounts are integers in the currency's " +
	"smallest unit (4250 with currency \"usd\" is $42.50; zero-decimal currencies such as JPY are " +
	"whole units). List tools return newest first; to page, pass the previous result's next_cursor " +
	"as starting_after. Failed calls return a JSON error with kind, message, param and retryable."

type registry struct {
	api  *stripe.Client
	opts Options
}

// Register adds the tools to the server. It returns the names registered, in
// order, so the caller can log exactly what is exposed.
func Register(s *mcp.Server, api *stripe.Client, opts Options) []string {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	r := &registry{api: api, opts: opts}
	var names []string

	add(r, s, &names, readTool(GetBalance, "Get account balance",
		"Return the Stripe account balance: funds available for payout and funds still pending, "+
			"per currency. Takes no arguments. Also reports whether the account key is live or test mode."),
		r.getBalance)

	add(r, s, &names, readTool(ListCustomers, "List customers",
		"List customers, newest first. Use email to find a customer by exact, case-sensitive email "+
			"address; there is no partial or name search. Returns up to limit customers plus next_cursor "+
			"when more exist."),
		r.listCustomers)

	add(r, s, &names, readTool(GetCustomer, "Get customer",
		"Fetch one customer by ID (cus_...). Use list_customers first if you only know the email."),
		r.getCustomer)

	add(r, s, &names, readTool(ListPaymentIntents, "List payment intents",
		"List payment intents (payment attempts), newest first, optionally for one customer. "+
			"status \"succeeded\" means the money was collected; \"requires_payment_method\" after an "+
			"attempt usually means the payment failed."),
		r.listPaymentIntents)

	add(r, s, &names, readTool(GetPaymentIntent, "Get payment intent",
		"Fetch one payment intent by ID (pi_...), including the amount received and, if the last "+
			"attempt failed, the decline code and message."),
		r.getPaymentIntent)

	add(r, s, &names, readTool(ListInvoices, "List invoices",
		"List invoices, newest first, optionally filtered by customer and by status "+
			"(draft, open, paid, uncollectible, void). Use status \"open\" to find unpaid invoices."),
		r.listInvoices)

	if !opts.ReadOnly {
		add(r, s, &names, writeTool(CreateCustomer, "Create customer", false,
			"Create a customer. Provide at least an email or a name. This writes to the Stripe "+
				"account. Check list_customers by email first to avoid duplicates. To make a retry "+
				"safe, pass the same idempotency_key again: Stripe then returns the original customer "+
				"instead of creating a second one."),
			r.createCustomer)

		add(r, s, &names, writeTool(CreateRefund, "Create refund", true,
			"Refund a succeeded payment intent, in full (omit amount) or in part (amount in the "+
				"smallest currency unit). This moves money back to the customer and cannot be undone. "+
				"Confirm the payment intent and amount with the user before calling. Pass the same "+
				"idempotency_key on a retry so the refund is not issued twice."),
			r.createRefund)
	}
	return names
}

func readTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
	}
}

func writeTool(name, title string, destructive bool, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}
}

// add registers one tool, wrapping its handler with the cross-cutting
// behaviour every tool shares: a deadline, the error contract and a log line.
//
// Arguments are not logged. They can contain personal data (emails, names),
// and the Stripe request ID is enough to find the call in the dashboard.
func add[In, Out any](r *registry, s *mcp.Server, names *[]string, tool *mcp.Tool, fn func(context.Context, In) (Out, error)) {
	*names = append(*names, tool.Name)
	mcp.AddTool(s, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		ctx, cancel := context.WithTimeout(ctx, r.opts.Timeout)
		defer cancel()

		start := time.Now()
		out, err := fn(ctx, in)
		attrs := []slog.Attr{
			slog.String("tool", tool.Name),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if err != nil {
			te := toToolError(err)
			attrs = append(attrs,
				slog.String("outcome", "error"),
				slog.String("error_kind", te.Kind),
				slog.Int("http_status", te.HTTPStatus),
				slog.String("stripe_request_id", te.StripeRequestID),
			)
			r.opts.Logger.LogAttrs(ctx, slog.LevelWarn, "tool call", attrs...)
			var zero Out
			return nil, zero, te
		}
		attrs = append(attrs, slog.String("outcome", "ok"))
		r.opts.Logger.LogAttrs(ctx, slog.LevelInfo, "tool call", attrs...)
		return nil, out, nil
	})
}

// ---- get_balance ----

type GetBalanceInput struct{}

type MoneyOutput struct {
	Amount   int64  `json:"amount" jsonschema:"amount in the smallest currency unit, e.g. cents"`
	Currency string `json:"currency" jsonschema:"three-letter ISO currency code, lowercase"`
}

type BalanceOutput struct {
	Available []MoneyOutput `json:"available" jsonschema:"funds available for payout, one entry per currency"`
	Pending   []MoneyOutput `json:"pending" jsonschema:"funds not yet available, one entry per currency"`
	Livemode  bool          `json:"livemode" jsonschema:"false when the server is using a test-mode key"`
}

func (r *registry) getBalance(ctx context.Context, _ GetBalanceInput) (BalanceOutput, error) {
	b, err := r.api.GetBalance(ctx)
	if err != nil {
		return BalanceOutput{}, err
	}
	return BalanceOutput{Available: moneyList(b.Available), Pending: moneyList(b.Pending), Livemode: b.Livemode}, nil
}

// ---- customers ----

type CustomerOutput struct {
	ID          string            `json:"id" jsonschema:"customer ID, cus_..."`
	Email       string            `json:"email,omitempty"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Created     string            `json:"created" jsonschema:"creation time, RFC 3339 UTC"`
	Delinquent  bool              `json:"delinquent" jsonschema:"true if the customer has an overdue invoice"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type ListCustomersInput struct {
	Email         string `json:"email,omitempty" jsonschema:"exact, case-sensitive email address to filter by"`
	Limit         int    `json:"limit,omitempty" jsonschema:"page size, 1 to 100; default 10"`
	StartingAfter string `json:"starting_after,omitempty" jsonschema:"next_cursor from the previous page (a customer ID)"`
}

type ListCustomersOutput struct {
	Customers  []CustomerOutput `json:"customers"`
	HasMore    bool             `json:"has_more" jsonschema:"true if another page exists"`
	NextCursor string           `json:"next_cursor,omitempty" jsonschema:"pass as starting_after to fetch the next page"`
}

func (r *registry) listCustomers(ctx context.Context, in ListCustomersInput) (ListCustomersOutput, error) {
	limit, lerr := normaliseLimit(in.Limit)
	if err := firstError(lerr,
		validateEmail("email", in.Email),
		optionalID("starting_after", in.StartingAfter, customerIDRe, "cus_123"),
	); err != nil {
		return ListCustomersOutput{}, err
	}
	page, err := r.api.ListCustomers(ctx, in.Email, stripe.Page{Limit: limit, StartingAfter: in.StartingAfter})
	if err != nil {
		return ListCustomersOutput{}, err
	}
	out := ListCustomersOutput{Customers: make([]CustomerOutput, 0, len(page.Data)), HasMore: page.HasMore}
	for _, c := range page.Data {
		out.Customers = append(out.Customers, customerOutput(&c))
	}
	if page.HasMore && len(page.Data) > 0 {
		out.NextCursor = page.Data[len(page.Data)-1].ID
	}
	return out, nil
}

type GetCustomerInput struct {
	CustomerID string `json:"customer_id" jsonschema:"customer ID, e.g. cus_Nabc123"`
}

func (r *registry) getCustomer(ctx context.Context, in GetCustomerInput) (CustomerOutput, error) {
	if err := firstError(requireID("customer_id", in.CustomerID, customerIDRe, "cus_123")); err != nil {
		return CustomerOutput{}, err
	}
	c, err := r.api.GetCustomer(ctx, in.CustomerID)
	if err != nil {
		return CustomerOutput{}, err
	}
	return customerOutput(c), nil
}

type CreateCustomerInput struct {
	Email          string            `json:"email,omitempty" jsonschema:"customer email address"`
	Name           string            `json:"name,omitempty" jsonschema:"customer full name or business name"`
	Description    string            `json:"description,omitempty" jsonschema:"internal note, not shown to the customer"`
	Metadata       map[string]string `json:"metadata,omitempty" jsonschema:"up to 50 string key-value pairs, e.g. your own customer reference"`
	IdempotencyKey string            `json:"idempotency_key,omitempty" jsonschema:"optional; reuse the same value when retrying this exact call"`
}

type CreateCustomerOutput struct {
	Customer       CustomerOutput `json:"customer"`
	IdempotencyKey string         `json:"idempotency_key" jsonschema:"key used for this write; pass it again to retry safely"`
}

func (r *registry) createCustomer(ctx context.Context, in CreateCustomerInput) (CreateCustomerOutput, error) {
	var missing *Error
	if in.Email == "" && in.Name == "" {
		missing = invalidInput("email", "provide at least one of email or name")
	}
	if err := firstError(missing,
		validateEmail("email", in.Email),
		validateMaxLen("name", in.Name, 256),
		validateMaxLen("description", in.Description, 350),
		validateMetadata(in.Metadata),
		validateIdempotencyKey(in.IdempotencyKey),
	); err != nil {
		return CreateCustomerOutput{}, err
	}
	key := idempotencyKey(in.IdempotencyKey)
	c, err := r.api.CreateCustomer(ctx, stripe.CustomerParams{
		Email: in.Email, Name: in.Name, Description: in.Description, Metadata: in.Metadata,
	}, key)
	if err != nil {
		return CreateCustomerOutput{}, err
	}
	return CreateCustomerOutput{Customer: customerOutput(c), IdempotencyKey: key}, nil
}

// ---- payment intents ----

type PaymentErrorOutput struct {
	Code    string `json:"code,omitempty" jsonschema:"Stripe error or decline code, e.g. card_declined"`
	Message string `json:"message,omitempty"`
}

type PaymentIntentOutput struct {
	ID             string              `json:"id" jsonschema:"payment intent ID, pi_..."`
	Amount         int64               `json:"amount" jsonschema:"intended amount in the smallest currency unit"`
	AmountReceived int64               `json:"amount_received" jsonschema:"amount actually collected, smallest currency unit"`
	Currency       string              `json:"currency"`
	Status         string              `json:"status" jsonschema:"e.g. succeeded, processing, requires_payment_method, canceled"`
	CustomerID     string              `json:"customer_id,omitempty"`
	Description    string              `json:"description,omitempty"`
	LatestChargeID string              `json:"latest_charge_id,omitempty"`
	Created        string              `json:"created" jsonschema:"creation time, RFC 3339 UTC"`
	LastError      *PaymentErrorOutput `json:"last_error,omitempty" jsonschema:"present when the most recent payment attempt failed"`
}

type ListPaymentIntentsInput struct {
	CustomerID    string `json:"customer_id,omitempty" jsonschema:"only payment intents for this customer, cus_..."`
	Limit         int    `json:"limit,omitempty" jsonschema:"page size, 1 to 100; default 10"`
	StartingAfter string `json:"starting_after,omitempty" jsonschema:"next_cursor from the previous page (a payment intent ID)"`
}

type ListPaymentIntentsOutput struct {
	PaymentIntents []PaymentIntentOutput `json:"payment_intents"`
	HasMore        bool                  `json:"has_more" jsonschema:"true if another page exists"`
	NextCursor     string                `json:"next_cursor,omitempty" jsonschema:"pass as starting_after to fetch the next page"`
}

func (r *registry) listPaymentIntents(ctx context.Context, in ListPaymentIntentsInput) (ListPaymentIntentsOutput, error) {
	limit, lerr := normaliseLimit(in.Limit)
	if err := firstError(lerr,
		optionalID("customer_id", in.CustomerID, customerIDRe, "cus_123"),
		optionalID("starting_after", in.StartingAfter, paymentIntentIDRe, "pi_123"),
	); err != nil {
		return ListPaymentIntentsOutput{}, err
	}
	page, err := r.api.ListPaymentIntents(ctx, in.CustomerID, stripe.Page{Limit: limit, StartingAfter: in.StartingAfter})
	if err != nil {
		return ListPaymentIntentsOutput{}, err
	}
	out := ListPaymentIntentsOutput{PaymentIntents: make([]PaymentIntentOutput, 0, len(page.Data)), HasMore: page.HasMore}
	for _, pi := range page.Data {
		out.PaymentIntents = append(out.PaymentIntents, paymentIntentOutput(&pi))
	}
	if page.HasMore && len(page.Data) > 0 {
		out.NextCursor = page.Data[len(page.Data)-1].ID
	}
	return out, nil
}

type GetPaymentIntentInput struct {
	PaymentIntentID string `json:"payment_intent_id" jsonschema:"payment intent ID, e.g. pi_3Nabc123"`
}

func (r *registry) getPaymentIntent(ctx context.Context, in GetPaymentIntentInput) (PaymentIntentOutput, error) {
	if err := firstError(requireID("payment_intent_id", in.PaymentIntentID, paymentIntentIDRe, "pi_123")); err != nil {
		return PaymentIntentOutput{}, err
	}
	pi, err := r.api.GetPaymentIntent(ctx, in.PaymentIntentID)
	if err != nil {
		return PaymentIntentOutput{}, err
	}
	return paymentIntentOutput(pi), nil
}

// ---- invoices ----

type InvoiceOutput struct {
	ID              string `json:"id" jsonschema:"invoice ID, in_..."`
	Number          string `json:"number,omitempty" jsonschema:"customer-facing invoice number"`
	CustomerID      string `json:"customer_id,omitempty"`
	Status          string `json:"status" jsonschema:"draft, open, paid, uncollectible or void"`
	Currency        string `json:"currency"`
	AmountDue       int64  `json:"amount_due" jsonschema:"smallest currency unit"`
	AmountPaid      int64  `json:"amount_paid" jsonschema:"smallest currency unit"`
	AmountRemaining int64  `json:"amount_remaining" jsonschema:"smallest currency unit"`
	DueDate         string `json:"due_date,omitempty" jsonschema:"RFC 3339 UTC; absent when the invoice has no due date"`
	Created         string `json:"created" jsonschema:"creation time, RFC 3339 UTC"`
	HostedURL       string `json:"hosted_url,omitempty" jsonschema:"link the customer can use to view and pay the invoice"`
}

type ListInvoicesInput struct {
	CustomerID    string `json:"customer_id,omitempty" jsonschema:"only invoices for this customer, cus_..."`
	Status        string `json:"status,omitempty" jsonschema:"one of: draft, open, paid, uncollectible, void"`
	Limit         int    `json:"limit,omitempty" jsonschema:"page size, 1 to 100; default 10"`
	StartingAfter string `json:"starting_after,omitempty" jsonschema:"next_cursor from the previous page (an invoice ID)"`
}

type ListInvoicesOutput struct {
	Invoices   []InvoiceOutput `json:"invoices"`
	HasMore    bool            `json:"has_more" jsonschema:"true if another page exists"`
	NextCursor string          `json:"next_cursor,omitempty" jsonschema:"pass as starting_after to fetch the next page"`
}

func (r *registry) listInvoices(ctx context.Context, in ListInvoicesInput) (ListInvoicesOutput, error) {
	limit, lerr := normaliseLimit(in.Limit)
	if err := firstError(lerr,
		optionalID("customer_id", in.CustomerID, customerIDRe, "cus_123"),
		validateEnum("status", in.Status, invoiceStatuses),
		optionalID("starting_after", in.StartingAfter, invoiceIDRe, "in_123"),
	); err != nil {
		return ListInvoicesOutput{}, err
	}
	page, err := r.api.ListInvoices(ctx, in.CustomerID, in.Status, stripe.Page{Limit: limit, StartingAfter: in.StartingAfter})
	if err != nil {
		return ListInvoicesOutput{}, err
	}
	out := ListInvoicesOutput{Invoices: make([]InvoiceOutput, 0, len(page.Data)), HasMore: page.HasMore}
	for _, inv := range page.Data {
		out.Invoices = append(out.Invoices, InvoiceOutput{
			ID: inv.ID, Number: inv.Number, CustomerID: inv.Customer, Status: inv.Status,
			Currency: inv.Currency, AmountDue: inv.AmountDue, AmountPaid: inv.AmountPaid,
			AmountRemaining: inv.AmountRemaining, DueDate: timestamp(inv.DueDate),
			Created: timestamp(inv.Created), HostedURL: inv.HostedInvoiceURL,
		})
	}
	if page.HasMore && len(page.Data) > 0 {
		out.NextCursor = page.Data[len(page.Data)-1].ID
	}
	return out, nil
}

// ---- refunds ----

type CreateRefundInput struct {
	PaymentIntentID string `json:"payment_intent_id" jsonschema:"the succeeded payment intent to refund, pi_..."`
	Amount          int64  `json:"amount,omitempty" jsonschema:"partial refund amount in the smallest currency unit; omit to refund the full remaining amount"`
	Reason          string `json:"reason,omitempty" jsonschema:"one of: duplicate, fraudulent, requested_by_customer"`
	IdempotencyKey  string `json:"idempotency_key,omitempty" jsonschema:"optional; reuse the same value when retrying this exact call"`
}

type CreateRefundOutput struct {
	ID              string `json:"id" jsonschema:"refund ID, re_..."`
	Amount          int64  `json:"amount" jsonschema:"refunded amount in the smallest currency unit"`
	Currency        string `json:"currency"`
	Status          string `json:"status" jsonschema:"succeeded, pending, failed, canceled or requires_action"`
	Reason          string `json:"reason,omitempty"`
	PaymentIntentID string `json:"payment_intent_id,omitempty"`
	Created         string `json:"created" jsonschema:"creation time, RFC 3339 UTC"`
	IdempotencyKey  string `json:"idempotency_key" jsonschema:"key used for this write; pass it again to retry safely"`
}

func (r *registry) createRefund(ctx context.Context, in CreateRefundInput) (CreateRefundOutput, error) {
	var amount *Error
	if in.Amount < 0 {
		amount = invalidInput("amount", "amount must be a positive integer in the smallest currency unit, or omitted for a full refund")
	}
	if err := firstError(
		requireID("payment_intent_id", in.PaymentIntentID, paymentIntentIDRe, "pi_123"),
		amount,
		validateEnum("reason", in.Reason, refundReasons),
		validateIdempotencyKey(in.IdempotencyKey),
	); err != nil {
		return CreateRefundOutput{}, err
	}
	key := idempotencyKey(in.IdempotencyKey)
	ref, err := r.api.CreateRefund(ctx, stripe.RefundParams{
		PaymentIntent: in.PaymentIntentID, Amount: in.Amount, Reason: in.Reason,
	}, key)
	if err != nil {
		return CreateRefundOutput{}, err
	}
	return CreateRefundOutput{
		ID: ref.ID, Amount: ref.Amount, Currency: ref.Currency, Status: ref.Status, Reason: ref.Reason,
		PaymentIntentID: ref.PaymentIntent, Created: timestamp(ref.Created), IdempotencyKey: key,
	}, nil
}

// ---- mapping helpers ----

func customerOutput(c *stripe.Customer) CustomerOutput {
	return CustomerOutput{
		ID: c.ID, Email: c.Email, Name: c.Name, Description: c.Description,
		Created: timestamp(c.Created), Delinquent: c.Delinquent, Metadata: c.Metadata,
	}
}

func paymentIntentOutput(pi *stripe.PaymentIntent) PaymentIntentOutput {
	out := PaymentIntentOutput{
		ID: pi.ID, Amount: pi.Amount, AmountReceived: pi.AmountReceived, Currency: pi.Currency,
		Status: pi.Status, CustomerID: pi.Customer, Description: pi.Description,
		LatestChargeID: pi.LatestCharge, Created: timestamp(pi.Created),
	}
	if pi.LastPaymentError != nil {
		out.LastError = &PaymentErrorOutput{Code: pi.LastPaymentError.Code, Message: pi.LastPaymentError.Message}
	}
	return out
}

// moneyList always returns a non-nil slice so the output serialises as a
// JSON array, which is what the output schema promises.
func moneyList(in []stripe.Money) []MoneyOutput {
	out := make([]MoneyOutput, 0, len(in))
	for _, m := range in {
		out = append(out, MoneyOutput{Amount: m.Amount, Currency: m.Currency})
	}
	return out
}

// timestamp renders Stripe's Unix seconds as RFC 3339, which models read
// more reliably than epoch integers. Zero means "not set".
func timestamp(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// idempotencyKey returns the caller's key, or a fresh one. Either way a
// single key covers every retry the client makes for this tool call.
func idempotencyKey(supplied string) string {
	if supplied != "" {
		return supplied
	}
	return stripe.NewIdempotencyKey()
}
