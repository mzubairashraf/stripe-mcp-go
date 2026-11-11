package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/mzubairashraf/stripe-mcp-go/internal/stripe"
)

// Error kinds. They are part of the tool contract: an LLM caller can branch
// on them to decide whether to fix its input, retry later or give up.
const (
	KindInvalidInput   = "invalid_input"   // rejected before any API call; fix the named param
	KindNotFound       = "not_found"       // the referenced object does not exist
	KindInvalidRequest = "invalid_request" // Stripe rejected the request; retrying unchanged will not help
	KindAuthentication = "authentication"  // the server's API key is missing permissions or invalid
	KindConflict       = "conflict"        // concurrent modification or idempotency-key reuse
	KindRateLimited    = "rate_limited"    // retries were exhausted while rate limited
	KindUpstream       = "upstream_error"  // Stripe or the network failed
	KindTimeout        = "timeout"         // the call did not complete within the tool timeout
)

// Error is the structured error returned to the MCP client. It is serialised
// as JSON in the tool result (with isError set) so the caller can read the
// kind, the offending parameter and whether a retry is worthwhile.
type Error struct {
	Kind            string `json:"kind"`
	Message         string `json:"message"`
	Param           string `json:"param,omitempty"`
	StripeCode      string `json:"stripe_code,omitempty"`
	StripeRequestID string `json:"stripe_request_id,omitempty"`
	HTTPStatus      int    `json:"http_status,omitempty"`
	Retryable       bool   `json:"retryable"`
}

func (e *Error) Error() string {
	b, err := json.Marshal(struct {
		Error *Error `json:"error"`
	}{e})
	if err != nil {
		return e.Kind + ": " + e.Message
	}
	return string(b)
}

func invalidInput(param, message string) *Error {
	return &Error{Kind: KindInvalidInput, Param: param, Message: message}
}

// toToolError maps any error from a handler onto the tool error contract.
func toToolError(err error) *Error {
	var te *Error
	if errors.As(err, &te) {
		return te
	}
	var se *stripe.Error
	if errors.As(err, &se) {
		return fromStripe(se)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: KindTimeout, Message: "the tool call timed out", Retryable: true}
	}
	// Unknown errors are reported without their text: it has not been
	// reviewed for what it might disclose.
	return &Error{Kind: KindUpstream, Message: "internal error"}
}

func fromStripe(se *stripe.Error) *Error {
	e := &Error{
		Message:         se.Message,
		Param:           se.Param,
		StripeCode:      se.Code,
		StripeRequestID: se.RequestID,
		HTTPStatus:      se.HTTPStatus,
		Retryable:       se.Retryable,
	}
	switch {
	case se.Type == "timeout":
		e.Kind = KindTimeout
	case se.HTTPStatus == http.StatusNotFound:
		e.Kind = KindNotFound
	case se.HTTPStatus == http.StatusUnauthorized, se.HTTPStatus == http.StatusForbidden:
		e.Kind = KindAuthentication
		// Stripe's 401 text can echo a fragment of the key; replace it.
		e.Message = "Stripe rejected the server's API key or its permissions"
	case se.HTTPStatus == http.StatusConflict:
		e.Kind = KindConflict
	case se.HTTPStatus == http.StatusTooManyRequests:
		e.Kind = KindRateLimited
	case se.HTTPStatus >= 400 && se.HTTPStatus < 500:
		e.Kind = KindInvalidRequest
	default:
		e.Kind = KindUpstream
	}
	return e
}
