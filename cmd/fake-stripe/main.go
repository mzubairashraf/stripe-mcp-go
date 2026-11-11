// Command fake-stripe serves the in-memory Stripe fake used by the tests, so
// the MCP server can be tried end to end without a Stripe account:
//
//	go run ./cmd/fake-stripe &
//	STRIPE_API_BASE=http://127.0.0.1:12111 STRIPE_API_KEY=sk_test_fake_0000000000000000 go run ./cmd/stripe-mcp
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/mzubairashraf/stripe-mcp-go/internal/stripetest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:12111", "listen address")
	flag.Parse()

	srv := &http.Server{Addr: *addr, Handler: stripetest.Handler(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("fake Stripe listening on http://%s (API key: %s)", *addr, stripetest.Key)
	log.Fatal(srv.ListenAndServe())
}
