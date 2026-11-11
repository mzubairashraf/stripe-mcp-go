package stripe

import (
	"context"
	"net/url"
	"strconv"
)

// The structs below decode only the fields the tools expose. Stripe adds
// fields over time; unknown fields are ignored, so a narrow view is stable
// across API versions.

// Money is an amount in the currency's smallest unit (cents for USD).
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// Balance is the account balance.
type Balance struct {
	Available []Money `json:"available"`
	Pending   []Money `json:"pending"`
	Livemode  bool    `json:"livemode"`
}

// Customer is a Stripe customer.
type Customer struct {
	ID          string            `json:"id"`
	Email       string            `json:"email"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Created     int64             `json:"created"`
	Delinquent  bool              `json:"delinquent"`
	Livemode    bool              `json:"livemode"`
	Metadata    map[string]string `json:"metadata"`
}

// PaymentIntent is a Stripe payment intent.
type PaymentIntent struct {
	ID               string `json:"id"`
	Amount           int64  `json:"amount"`
	AmountReceived   int64  `json:"amount_received"`
	Currency         string `json:"currency"`
	Status           string `json:"status"`
	Customer         string `json:"customer"`
	Description      string `json:"description"`
	LatestCharge     string `json:"latest_charge"`
	Created          int64  `json:"created"`
	Livemode         bool   `json:"livemode"`
	LastPaymentError *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"last_payment_error"`
}

// Invoice is a Stripe invoice.
type Invoice struct {
	ID               string `json:"id"`
	Number           string `json:"number"`
	Customer         string `json:"customer"`
	Status           string `json:"status"`
	Currency         string `json:"currency"`
	AmountDue        int64  `json:"amount_due"`
	AmountPaid       int64  `json:"amount_paid"`
	AmountRemaining  int64  `json:"amount_remaining"`
	DueDate          int64  `json:"due_date"`
	Created          int64  `json:"created"`
	HostedInvoiceURL string `json:"hosted_invoice_url"`
}

// Refund is a Stripe refund.
type Refund struct {
	ID            string `json:"id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	PaymentIntent string `json:"payment_intent"`
	Charge        string `json:"charge"`
	Created       int64  `json:"created"`
}

// List is one page of a Stripe list endpoint.
type List[T any] struct {
	Data    []T  `json:"data"`
	HasMore bool `json:"has_more"`
}

// Page selects a page of a list endpoint using Stripe's cursor pagination.
type Page struct {
	Limit         int    // 1..100; 0 uses Stripe's default of 10
	StartingAfter string // ID of the last item of the previous page
}

func (p Page) apply(q url.Values) {
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.StartingAfter != "" {
		q.Set("starting_after", p.StartingAfter)
	}
}

// GetBalance returns the account balance.
func (c *Client) GetBalance(ctx context.Context) (*Balance, error) {
	var out Balance
	if err := c.get(ctx, "/v1/balance", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCustomers lists customers, newest first, optionally filtered by an
// exact, case-sensitive email match.
func (c *Client) ListCustomers(ctx context.Context, email string, page Page) (*List[Customer], error) {
	q := url.Values{}
	if email != "" {
		q.Set("email", email)
	}
	page.apply(q)
	var out List[Customer]
	if err := c.get(ctx, "/v1/customers", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetCustomer fetches one customer by ID.
func (c *Client) GetCustomer(ctx context.Context, id string) (*Customer, error) {
	var out Customer
	if err := c.get(ctx, "/v1/customers/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CustomerParams are the fields accepted when creating a customer.
type CustomerParams struct {
	Email       string
	Name        string
	Description string
	Metadata    map[string]string
}

// CreateCustomer creates a customer. Repeating the call with the same
// idempotency key returns the original customer instead of a duplicate.
func (c *Client) CreateCustomer(ctx context.Context, p CustomerParams, idempotencyKey string) (*Customer, error) {
	form := url.Values{}
	setIfNotEmpty(form, "email", p.Email)
	setIfNotEmpty(form, "name", p.Name)
	setIfNotEmpty(form, "description", p.Description)
	for k, v := range p.Metadata {
		form.Set("metadata["+k+"]", v)
	}
	var out Customer
	if err := c.post(ctx, "/v1/customers", form, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPaymentIntents lists payment intents, newest first, optionally for one
// customer.
func (c *Client) ListPaymentIntents(ctx context.Context, customerID string, page Page) (*List[PaymentIntent], error) {
	q := url.Values{}
	setIfNotEmpty(q, "customer", customerID)
	page.apply(q)
	var out List[PaymentIntent]
	if err := c.get(ctx, "/v1/payment_intents", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPaymentIntent fetches one payment intent by ID.
func (c *Client) GetPaymentIntent(ctx context.Context, id string) (*PaymentIntent, error) {
	var out PaymentIntent
	if err := c.get(ctx, "/v1/payment_intents/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListInvoices lists invoices, newest first, optionally filtered by customer
// and status.
func (c *Client) ListInvoices(ctx context.Context, customerID, status string, page Page) (*List[Invoice], error) {
	q := url.Values{}
	setIfNotEmpty(q, "customer", customerID)
	setIfNotEmpty(q, "status", status)
	page.apply(q)
	var out List[Invoice]
	if err := c.get(ctx, "/v1/invoices", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefundParams are the fields accepted when creating a refund.
type RefundParams struct {
	PaymentIntent string
	Amount        int64  // 0 refunds the full remaining amount
	Reason        string // optional: duplicate, fraudulent or requested_by_customer
}

// CreateRefund refunds a payment intent in full or in part.
func (c *Client) CreateRefund(ctx context.Context, p RefundParams, idempotencyKey string) (*Refund, error) {
	form := url.Values{}
	form.Set("payment_intent", p.PaymentIntent)
	if p.Amount > 0 {
		form.Set("amount", strconv.FormatInt(p.Amount, 10))
	}
	setIfNotEmpty(form, "reason", p.Reason)
	var out Refund
	if err := c.post(ctx, "/v1/refunds", form, idempotencyKey, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func setIfNotEmpty(v url.Values, key, value string) {
	if value != "" {
		v.Set(key, value)
	}
}
