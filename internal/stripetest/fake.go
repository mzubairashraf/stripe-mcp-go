// Package stripetest provides an in-memory fake of the Stripe endpoints the
// server uses. It reproduces the behaviour the client depends on: bearer
// authentication, form-encoded writes, cursor pagination, idempotency keys,
// Stripe's error envelope and injectable transient failures.
package stripetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// Key is the API key the fake accepts.
const Key = "sk_test_fake_0000000000000000"

// Seeded object IDs.
const (
	CustomerAda     = "cus_Ada001"
	CustomerGrace   = "cus_Grace002"
	CustomerLinus   = "cus_Linus003"
	PaymentPaid     = "pi_Paid001"
	PaymentDeclined = "pi_Declined002"
	InvoiceOpen     = "in_Open001"
	InvoicePaid     = "in_Paid002"
)

// Request is a request the fake received, kept for assertions.
type Request struct {
	Method         string
	Path           string
	Query          map[string]string
	Form           map[string]string
	IdempotencyKey string
	Authorization  string
}

// Fault is a canned failure served instead of the real handler.
type Fault struct {
	Status int
	Header map[string]string
	Body   string // defaults to a Stripe-style api_error envelope
}

type object = map[string]any

// Fake is the fake Stripe server.
type Fake struct {
	*httptest.Server

	mu          sync.Mutex
	customers   []object // newest first, as Stripe lists them
	intents     []object
	invoices    []object
	refunded    map[string]int64  // payment intent ID -> amount refunded
	idempotent  map[string]object // idempotency key -> first response
	faults      []Fault
	requests    []Request
	nextID      int
	nextRequest int
}

// New starts a fake on a random local port, seeded with a small, fixed data set.
func New() *Fake {
	f := newFake()
	f.Server = httptest.NewServer(f.Handler())
	return f
}

// Handler returns a standalone handler backed by its own seeded state, for
// serving the fake on a fixed address (see cmd/fake-stripe).
func Handler() http.Handler { return newFake().Handler() }

// Handler returns the fake's HTTP handler.
func (f *Fake) Handler() http.Handler { return http.HandlerFunc(f.serve) }

func newFake() *Fake {
	f := &Fake{refunded: map[string]int64{}, idempotent: map[string]object{}}
	f.customers = []object{
		{"id": CustomerLinus, "object": "customer", "email": "linus@example.com", "name": "Linus Example", "created": 1762300800, "delinquent": false, "livemode": false, "metadata": object{}},
		{"id": CustomerGrace, "object": "customer", "email": "grace@example.com", "name": "Grace Example", "created": 1762214400, "delinquent": true, "livemode": false, "metadata": object{"crm_id": "42"}},
		{"id": CustomerAda, "object": "customer", "email": "ada@example.com", "name": "Ada Example", "created": 1762128000, "delinquent": false, "livemode": false, "metadata": object{}},
	}
	f.intents = []object{
		{"id": PaymentDeclined, "object": "payment_intent", "amount": 9900, "amount_received": 0, "currency": "usd", "status": "requires_payment_method", "customer": CustomerGrace, "created": 1762304400,
			"last_payment_error": object{"code": "card_declined", "message": "Your card was declined."}},
		{"id": PaymentPaid, "object": "payment_intent", "amount": 4250, "amount_received": 4250, "currency": "usd", "status": "succeeded", "customer": CustomerAda, "description": "Order 1001", "latest_charge": "ch_Paid001", "created": 1762218000},
	}
	f.invoices = []object{
		{"id": InvoicePaid, "object": "invoice", "number": "INV-0002", "customer": CustomerAda, "status": "paid", "currency": "usd", "amount_due": 4250, "amount_paid": 4250, "amount_remaining": 0, "created": 1762218000},
		{"id": InvoiceOpen, "object": "invoice", "number": "INV-0001", "customer": CustomerGrace, "status": "open", "currency": "usd", "amount_due": 9900, "amount_paid": 0, "amount_remaining": 9900, "due_date": 1763424000, "created": 1762131600, "hosted_invoice_url": "https://invoice.stripe.com/i/test_0001"},
	}
	return f
}

// FailNext queues faults; each is served once, in order, before normal
// handling resumes.
func (f *Fake) FailNext(faults ...Fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, faults...)
}

// Requests returns a copy of every request received so far.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	_ = r.ParseForm()
	rec := Request{
		Method: r.Method, Path: r.URL.Path, Query: flatten(r.URL.Query()), Form: flatten(r.PostForm),
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Authorization: r.Header.Get("Authorization"),
	}
	f.requests = append(f.requests, rec)

	f.nextRequest++
	w.Header().Set("Request-Id", fmt.Sprintf("req_fake%04d", f.nextRequest))
	w.Header().Set("Content-Type", "application/json")

	if len(f.faults) > 0 {
		fault := f.faults[0]
		f.faults = f.faults[1:]
		for k, v := range fault.Header {
			w.Header().Set(k, v)
		}
		body := fault.Body
		if body == "" {
			body = `{"error":{"type":"api_error","message":"injected fault"}}`
		}
		w.WriteHeader(fault.Status)
		_, _ = w.Write([]byte(body))
		return
	}

	if rec.Authorization != "Bearer "+Key {
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "", "", "Invalid API Key provided: sk_test_****")
		return
	}

	// A replayed idempotency key returns the first response unchanged.
	if r.Method == http.MethodPost && rec.IdempotencyKey != "" {
		if prev, ok := f.idempotent[rec.IdempotencyKey]; ok {
			w.Header().Set("Idempotent-Replayed", "true")
			writeJSON(w, http.StatusOK, prev)
			return
		}
	}

	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	switch {
	case r.Method == http.MethodGet && path == "balance":
		writeJSON(w, http.StatusOK, object{
			"object": "balance", "livemode": false,
			"available": []object{{"amount": 125000, "currency": "usd"}},
			"pending":   []object{{"amount": 4250, "currency": "usd"}},
		})

	case r.Method == http.MethodGet && path == "customers":
		f.list(w, r, f.customers, func(o object) bool {
			email := r.URL.Query().Get("email")
			return email == "" || o["email"] == email
		})

	case r.Method == http.MethodPost && path == "customers":
		f.nextID++
		c := object{
			"id": fmt.Sprintf("cus_New%03d", f.nextID), "object": "customer",
			"email": r.PostForm.Get("email"), "name": r.PostForm.Get("name"),
			"description": r.PostForm.Get("description"), "created": 1762387200,
			"delinquent": false, "livemode": false, "metadata": formMap(r, "metadata"),
		}
		f.customers = append([]object{c}, f.customers...)
		f.idempotent[rec.IdempotencyKey] = c
		writeJSON(w, http.StatusOK, c)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "customers/"):
		f.get(w, f.customers, strings.TrimPrefix(path, "customers/"), "customer")

	case r.Method == http.MethodGet && path == "payment_intents":
		f.list(w, r, f.intents, func(o object) bool {
			customer := r.URL.Query().Get("customer")
			return customer == "" || o["customer"] == customer
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "payment_intents/"):
		f.get(w, f.intents, strings.TrimPrefix(path, "payment_intents/"), "payment_intent")

	case r.Method == http.MethodGet && path == "invoices":
		f.list(w, r, f.invoices, func(o object) bool {
			q := r.URL.Query()
			return (q.Get("customer") == "" || o["customer"] == q.Get("customer")) &&
				(q.Get("status") == "" || o["status"] == q.Get("status"))
		})

	case r.Method == http.MethodPost && path == "refunds":
		f.refund(w, r, rec.IdempotencyKey)

	default:
		writeError(w, http.StatusNotFound, "invalid_request_error", "", "", "Unrecognized request URL ("+r.Method+": "+r.URL.Path+").")
	}
}

func (f *Fake) get(w http.ResponseWriter, items []object, id, kind string) {
	for _, o := range items {
		if o["id"] == id {
			writeJSON(w, http.StatusOK, o)
			return
		}
	}
	writeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing", "id", "No such "+kind+": '"+id+"'")
}

// list implements Stripe's cursor pagination over a filtered slice.
func (f *Fake) list(w http.ResponseWriter, r *http.Request, items []object, keep func(object) bool) {
	var filtered []object
	for _, o := range items {
		if keep(o) {
			filtered = append(filtered, o)
		}
	}
	if after := r.URL.Query().Get("starting_after"); after != "" {
		idx := -1
		for i, o := range filtered {
			if o["id"] == after {
				idx = i
				break
			}
		}
		if idx < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "resource_missing", "starting_after", "No such object: '"+after+"'")
			return
		}
		filtered = filtered[idx+1:]
	}
	limit := 10
	if s := r.URL.Query().Get("limit"); s != "" {
		limit, _ = strconv.Atoi(s)
	}
	hasMore := len(filtered) > limit
	if hasMore {
		filtered = filtered[:limit]
	}
	if filtered == nil {
		filtered = []object{}
	}
	writeJSON(w, http.StatusOK, object{"object": "list", "data": filtered, "has_more": hasMore})
}

func (f *Fake) refund(w http.ResponseWriter, r *http.Request, idempotencyKey string) {
	id := r.PostForm.Get("payment_intent")
	var intent object
	for _, o := range f.intents {
		if o["id"] == id {
			intent = o
		}
	}
	if intent == nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", "resource_missing", "payment_intent", "No such payment_intent: '"+id+"'")
		return
	}
	if intent["status"] != "succeeded" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "payment_intent_unexpected_state", "payment_intent", "This PaymentIntent does not have a successful charge to refund.")
		return
	}
	received := int64(intent["amount_received"].(int))
	remaining := received - f.refunded[id]
	if remaining <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "charge_already_refunded", "", "The charge has already been refunded.")
		return
	}
	amount := remaining
	if s := r.PostForm.Get("amount"); s != "" {
		amount, _ = strconv.ParseInt(s, 10, 64)
	}
	if amount > remaining {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "amount_too_large", "amount", "Refund amount is greater than the unrefunded amount on the charge.")
		return
	}
	f.refunded[id] += amount
	f.nextID++
	ref := object{
		"id": fmt.Sprintf("re_New%03d", f.nextID), "object": "refund", "amount": amount,
		"currency": intent["currency"], "status": "succeeded", "reason": r.PostForm.Get("reason"),
		"payment_intent": id, "charge": intent["latest_charge"], "created": 1762387200,
	}
	f.idempotent[idempotencyKey] = ref
	writeJSON(w, http.StatusOK, ref)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, code, param, message string) {
	e := object{"type": typ, "message": message}
	if code != "" {
		e["code"] = code
	}
	if param != "" {
		e["param"] = param
	}
	writeJSON(w, status, object{"error": e})
}

func flatten(v map[string][]string) map[string]string {
	out := make(map[string]string, len(v))
	for k, vals := range v {
		if len(vals) > 0 {
			out[k] = vals[0]
		}
	}
	return out
}

// formMap collects bracketed form fields such as metadata[key]=value.
func formMap(r *http.Request, name string) object {
	out := object{}
	for k, vals := range r.PostForm {
		if strings.HasPrefix(k, name+"[") && strings.HasSuffix(k, "]") && len(vals) > 0 {
			out[k[len(name)+1:len(k)-1]] = vals[0]
		}
	}
	return out
}
