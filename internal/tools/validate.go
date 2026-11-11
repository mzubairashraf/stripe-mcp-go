package tools

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

const (
	defaultLimit = 10
	maxLimit     = 100
)

var (
	customerIDRe      = regexp.MustCompile(`^cus_[A-Za-z0-9]{1,64}$`)
	paymentIntentIDRe = regexp.MustCompile(`^pi_[A-Za-z0-9]{1,64}$`)
	invoiceIDRe       = regexp.MustCompile(`^in_[A-Za-z0-9]{1,64}$`)
	// Idempotency keys go into an HTTP header: printable ASCII without spaces.
	idempotencyKeyRe = regexp.MustCompile(`^[\x21-\x7e]{1,255}$`)

	invoiceStatuses = []string{"draft", "open", "paid", "uncollectible", "void"}
	refundReasons   = []string{"duplicate", "fraudulent", "requested_by_customer"}
)

// requireID checks a required object ID. IDs are interpolated into URL paths,
// so the strict pattern is also what keeps a caller from steering the request
// to a different endpoint.
func requireID(param, value string, re *regexp.Regexp, example string) *Error {
	if value == "" {
		return invalidInput(param, param+" is required")
	}
	return optionalID(param, value, re, example)
}

func optionalID(param, value string, re *regexp.Regexp, example string) *Error {
	if value != "" && !re.MatchString(value) {
		return invalidInput(param, fmt.Sprintf("%s must look like %q", param, example))
	}
	return nil
}

// normaliseLimit applies the default and enforces Stripe's page-size bounds.
func normaliseLimit(limit int) (int, *Error) {
	if limit == 0 {
		return defaultLimit, nil
	}
	if limit < 1 || limit > maxLimit {
		return 0, invalidInput("limit", fmt.Sprintf("limit must be between 1 and %d", maxLimit))
	}
	return limit, nil
}

func validateEmail(param, value string) *Error {
	if value == "" {
		return nil
	}
	addr, err := mail.ParseAddress(value)
	if err != nil || addr.Address != value || len(value) > 512 {
		return invalidInput(param, param+" must be a plain email address such as jane@example.com")
	}
	return nil
}

func validateEnum(param, value string, allowed []string) *Error {
	if value == "" {
		return nil
	}
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return invalidInput(param, fmt.Sprintf("%s must be one of: %s", param, strings.Join(allowed, ", ")))
}

func validateMaxLen(param, value string, max int) *Error {
	if len(value) > max {
		return invalidInput(param, fmt.Sprintf("%s must be at most %d characters", param, max))
	}
	return nil
}

// validateMetadata enforces Stripe's documented metadata limits so the caller
// gets a precise message instead of a generic API rejection.
func validateMetadata(md map[string]string) *Error {
	if len(md) > 50 {
		return invalidInput("metadata", "metadata may hold at most 50 keys")
	}
	for k, v := range md {
		switch {
		case k == "" || len(k) > 40:
			return invalidInput("metadata", "metadata keys must be 1 to 40 characters")
		case strings.ContainsAny(k, "[]"):
			return invalidInput("metadata", "metadata keys must not contain square brackets")
		case len(v) > 500:
			return invalidInput("metadata", fmt.Sprintf("metadata value for %q exceeds 500 characters", k))
		}
	}
	return nil
}

func validateIdempotencyKey(value string) *Error {
	if value != "" && !idempotencyKeyRe.MatchString(value) {
		return invalidInput("idempotency_key", "idempotency_key must be 1 to 255 printable ASCII characters without spaces")
	}
	return nil
}

// firstError returns the first non-nil validation error as an error value,
// avoiding the typed-nil interface pitfall.
func firstError(errs ...*Error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
