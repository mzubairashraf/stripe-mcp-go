# stripe-mcp-go

[![CI](https://github.com/mzubairashraf/stripe-mcp-go/actions/workflows/ci.yml/badge.svg)](https://github.com/mzubairashraf/stripe-mcp-go/actions/workflows/ci.yml)

A [Model Context Protocol](https://modelcontextprotocol.io) server, written in Go, that lets an LLM agent work with a Stripe account through eight typed tools: look up customers, payments and invoices, create a customer, and issue a refund. It is a small reference implementation of how to put an existing payments API in front of an agent safely: validated inputs, retries that cannot double-charge, a read-only mode, and logs that never contain credentials or customer data.

Built with the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) (v1.8). One direct dependency. The Stripe client is about 550 lines of standard library.

This is an independent project and is not affiliated with Stripe.

## Quick start

Requires Go 1.25 or newer.

**Without a Stripe account** (uses the in-memory fake that the tests run against):

```bash
git clone https://github.com/mzubairashraf/stripe-mcp-go && cd stripe-mcp-go
go test ./...
go run ./cmd/fake-stripe &
STRIPE_API_BASE=http://127.0.0.1:12111 STRIPE_API_KEY=sk_test_fake_0000000000000000 go run ./cmd/stripe-mcp
```

The server is now waiting for MCP messages on stdin. Paste these three lines to initialise it and list the invoices that are still unpaid:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"manual","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_invoices","arguments":{"status":"open"}}}
```

**With Stripe test mode:** copy a test-mode secret key (`sk_test_...`) or, better, a restricted test key (`rk_test_...`) from the Stripe dashboard and run:

```bash
go install github.com/mzubairashraf/stripe-mcp-go/cmd/stripe-mcp@latest
STRIPE_API_KEY=sk_test_... stripe-mcp
```

## Connecting a client

Claude Code:

```bash
claude mcp add stripe --env STRIPE_API_KEY=sk_test_... -- stripe-mcp
```

Claude Desktop (`claude_desktop_config.json`), or any client that uses the same `mcpServers` format:

```json
{
  "mcpServers": {
    "stripe": {
      "command": "/absolute/path/to/stripe-mcp",
      "args": ["-read-only"],
      "env": { "STRIPE_API_KEY": "sk_test_..." }
    }
  }
}
```

Remove `-read-only` to expose the two write tools. For a remote deployment, `stripe-mcp -http 127.0.0.1:8080` serves the same tools over streamable HTTP at `/mcp`.

## Tools

| Tool | Type | What it does |
| --- | --- | --- |
| `get_balance` | read | Available and pending balance per currency, and whether the key is test or live mode. |
| `list_customers` | read | Customers, newest first; optional exact-match email filter; cursor pagination. |
| `get_customer` | read | One customer by ID. |
| `list_payment_intents` | read | Payment attempts, newest first; optional customer filter. |
| `get_payment_intent` | read | One payment, including the decline code and message if the last attempt failed. |
| `list_invoices` | read | Invoices filtered by customer and status (for example `open` for unpaid). |
| `create_customer` | write | Creates a customer. Idempotent when the caller repeats the `idempotency_key`. |
| `create_refund` | write | Refunds a succeeded payment in full or in part. Marked destructive. |

Every tool publishes a JSON Schema for its input and its output, generated from the Go types, and returns structured content alongside the text block. Amounts are integers in the smallest currency unit; timestamps are RFC 3339.

A failed call returns `isError: true` with a body the model can act on:

```json
{"error":{"kind":"not_found","message":"No such customer: 'cus_nope'","param":"id","stripe_code":"resource_missing","stripe_request_id":"req_...","http_status":404,"retryable":false}}
```

`kind` is one of `invalid_input`, `not_found`, `invalid_request`, `authentication`, `conflict`, `rate_limited`, `upstream_error`, `timeout`.

## Configuration

| Setting | Default | Purpose |
| --- | --- | --- |
| `STRIPE_API_KEY` (env) | required | Secret or restricted key. The only way to supply it; there is no flag or config file for secrets. |
| `-read-only` / `STRIPE_MCP_READ_ONLY=true` | off | Do not register the write tools. |
| `-allow-live` | off | Accept a live-mode key. The server is then forced read-only. |
| `-tool-timeout` | `30s` | Deadline for one tool call, including retries. |
| `-http ADDR` | stdio | Serve streamable HTTP instead of stdio. |
| `MCP_HTTP_TOKEN` (env) | none | Bearer token required from HTTP clients. Mandatory when `-http` binds a non-loopback address. |
| `STRIPE_API_VERSION` (env) | account default | Pin the `Stripe-Version` header. |
| `STRIPE_API_BASE` (env) | Stripe | Point at a fake. Plain HTTP is accepted only for localhost. |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |

## Design notes

**Layout.** `internal/stripe` is the HTTP client and knows nothing about MCP. `internal/tools` defines the tools and knows nothing about HTTP. `internal/config` validates the environment. `cmd/stripe-mcp` wires them together in about 150 lines. Wrapping a different API means replacing the first package and rewriting the tool definitions; the rest carries over.

**Error handling.** Each HTTP attempt has its own timeout and each tool call has an overall deadline. Transient failures (connection errors, 409, 429, 5xx, or whatever Stripe marks with `Stripe-Should-Retry`) are retried up to three times with exponential backoff and full jitter. `Retry-After` is honoured, and the client gives up early rather than start a wait that the caller's deadline cannot cover. Errors that reach the model are mapped to a small fixed vocabulary with a `retryable` flag, so the agent can tell "fix your input" from "try again later" from "stop". Stripe's request ID is passed through for support and log correlation.

**Safety of write tools.**

- Writes are test-mode only. A live key is refused at start-up unless `-allow-live` is given, and that flag forces read-only mode.
- In read-only mode the write tools are not registered at all, so a client cannot see or call them. This is stronger than registering them and rejecting calls.
- Every write carries an `Idempotency-Key`, generated per tool call or supplied by the caller, and the client refuses to send a write without one. A retry after a timeout therefore cannot create a second customer or a second refund. The key is returned in the result so the agent can repeat the call safely.
- Tools carry MCP annotations (`readOnlyHint`, `destructiveHint`) so a host can ask for confirmation before a refund. The refund description also tells the model to confirm with the user first.
- Object IDs are checked against strict patterns before they are placed in a URL path, which also prevents a crafted ID from redirecting a request to another endpoint.
- Restricted keys are supported and recommended: grant the key only the resources these tools touch.

**Secrets and logging.** The key is read from the environment and held in a `Secret` type whose `String`, `GoString`, `LogValue` and `MarshalText` methods are redacted; the raw value is reachable only through one `Reveal()` method, so every use is easy to find in review. Logs are structured JSON on stderr (stdout is the protocol channel). They record the tool name, outcome, duration and Stripe request ID, and deliberately omit arguments, because arguments contain customer emails and names. Stripe's 401 message, which can echo part of a key, is replaced before it reaches the model.

**Testing.** `go test ./...` runs three layers, all against `internal/stripetest`, an in-memory fake that implements Stripe's authentication, cursor pagination, idempotency replay, error envelope and injectable faults:

1. Client tests: the retry policy as a table (which statuses retry, how many attempts, how long the wait), idempotency keys held constant across retries, timeouts, cancellation.
2. Tool tests: a real MCP client connected to the server over the SDK's in-memory transport, so schema validation, handlers and error mapping are exercised together. Every validation case also asserts that no request reached the API.
3. End-to-end: the test builds the binary and drives it as a child process over stdio, the way an MCP host does.

Several tests assert that neither the API key nor customer data appears in errors or logs. CI runs `gofmt`, `go vet` and `go test -race`.

**What is deliberately left out.** No OAuth for the HTTP transport (a static bearer token is enough for a private deployment behind a gateway; a multi-tenant service would need MCP's OAuth flow). No client-side rate limiter (the server reacts to 429s rather than predicting them). Only eight tools: an agent chooses better from a short list of well-described tools than from a mirror of the whole API.

## Licence

MIT. See [LICENSE](LICENSE).
