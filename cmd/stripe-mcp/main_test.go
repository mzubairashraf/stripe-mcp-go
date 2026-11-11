package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mzubairashraf/stripe-mcp-go/internal/stripetest"
)

var binary string

// TestMain builds the real binary once. The tests below then talk to it the
// way an MCP host does: as a child process, over stdin and stdout.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "stripe-mcp-e2e")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "stripe-mcp")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic("build failed: " + err.Error() + "\n" + string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func start(t *testing.T, fake *stripetest.Fake, args ...string) (*mcp.ClientSession, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = []string{"STRIPE_API_KEY=" + stripetest.Key, "STRIPE_API_BASE=" + fake.URL}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr: %s", err, stderr)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, stderr
}

func TestStdioEndToEnd(t *testing.T) {
	fake := stripetest.New()
	defer fake.Close()
	session, stderr := start(t, fake)
	ctx := context.Background()

	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 8 {
		t.Errorf("tools = %d, want 8", len(list.Tools))
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "get_customer", Arguments: map[string]any{"customer_id": stripetest.CustomerAda},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError || !strings.Contains(text, `"email":"ada@example.com"`) {
		t.Errorf("isError=%v content=%s", res.IsError, text)
	}

	_ = session.Close() // stdin closes; the server exits and stderr is complete
	logs := stderr.String()
	if strings.Contains(logs, stripetest.Key) {
		t.Errorf("stderr discloses the API key:\n%s", logs)
	}
	if !strings.Contains(logs, `"tool":"get_customer"`) {
		t.Errorf("stderr is missing the tool-call log line:\n%s", logs)
	}
}

func TestStdioReadOnly(t *testing.T) {
	fake := stripetest.New()
	defer fake.Close()
	session, _ := start(t, fake, "-read-only")

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if strings.HasPrefix(tool.Name, "create_") {
			t.Errorf("write tool %s is exposed in read-only mode", tool.Name)
		}
	}
	if len(list.Tools) != 6 {
		t.Errorf("tools = %d, want 6", len(list.Tools))
	}
}

func TestRefusesToStartWithoutAUsableKey(t *testing.T) {
	for name, key := range map[string]string{"missing": "", "live": "sk_live_abcdef123456"} {
		cmd := exec.Command(binary)
		cmd.Env = []string{"STRIPE_API_KEY=" + key}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("%s key: expected a non-zero exit", name)
		}
		if key != "" && strings.Contains(string(out), key) {
			t.Errorf("%s key: output discloses the key: %s", name, out)
		}
	}
}
