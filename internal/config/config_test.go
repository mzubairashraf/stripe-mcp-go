package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	const testKey, liveKey = "sk_test_abc123", "sk_live_abc123"
	tests := []struct {
		name         string
		args         []string
		env          map[string]string
		wantErr      string // substring; empty means success
		wantReadOnly bool
		wantLive     bool
	}{
		{name: "test key", env: map[string]string{"STRIPE_API_KEY": testKey}},
		{name: "restricted test key", env: map[string]string{"STRIPE_API_KEY": "rk_test_abc"}},
		{name: "read-only flag", args: []string{"-read-only"}, env: map[string]string{"STRIPE_API_KEY": testKey}, wantReadOnly: true},
		{name: "read-only env", env: map[string]string{"STRIPE_API_KEY": testKey, "STRIPE_MCP_READ_ONLY": "true"}, wantReadOnly: true},
		{name: "missing key", wantErr: "STRIPE_API_KEY is not set"},
		{name: "publishable key", env: map[string]string{"STRIPE_API_KEY": "pk_test_abc"}, wantErr: "publishable key"},
		{name: "unrecognised key", env: map[string]string{"STRIPE_API_KEY": "hunter2"}, wantErr: "not a recognised Stripe key"},
		{name: "live key refused by default", env: map[string]string{"STRIPE_API_KEY": liveKey}, wantErr: "live-mode key"},
		{name: "live key allowed is forced read-only", args: []string{"-allow-live"}, env: map[string]string{"STRIPE_API_KEY": liveKey}, wantReadOnly: true, wantLive: true},
		{name: "http base only on loopback", env: map[string]string{"STRIPE_API_KEY": testKey, "STRIPE_API_BASE": "http://example.com"}, wantErr: "must use https"},
		{name: "http base on loopback", env: map[string]string{"STRIPE_API_KEY": testKey, "STRIPE_API_BASE": "http://127.0.0.1:9999"}},
		{name: "public http needs token", args: []string{"-http", "0.0.0.0:8080"}, env: map[string]string{"STRIPE_API_KEY": testKey}, wantErr: "requires MCP_HTTP_TOKEN"},
		{name: "public http with token", args: []string{"-http", ":8080"}, env: map[string]string{"STRIPE_API_KEY": testKey, "MCP_HTTP_TOKEN": "t0ken"}},
		{name: "loopback http without token", args: []string{"-http", "127.0.0.1:8080"}, env: map[string]string{"STRIPE_API_KEY": testKey}},
		{name: "bad log level", args: []string{"-log-level", "loud"}, env: map[string]string{"STRIPE_API_KEY": testKey}, wantErr: "invalid -log-level"},
		{name: "stray argument", args: []string{"serve"}, env: map[string]string{"STRIPE_API_KEY": testKey}, wantErr: "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.args, func(k string) string { return tt.env[k] }, io.Discard)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if key := tt.env["STRIPE_API_KEY"]; key != "" && strings.Contains(err.Error(), key) {
					t.Errorf("error discloses the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ReadOnly != tt.wantReadOnly || cfg.Livemode != tt.wantLive {
				t.Errorf("readOnly=%v live=%v, want %v/%v", cfg.ReadOnly, cfg.Livemode, tt.wantReadOnly, tt.wantLive)
			}
		})
	}
}

func TestSecretIsRedactedEverywhere(t *testing.T) {
	const raw = "sk_test_do_not_print"
	cfg := &Config{APIKey: Secret(raw), HTTPToken: Secret(raw)}

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config", "key", cfg.APIKey, "cfg", cfg)
	asJSON, _ := json.Marshal(cfg)

	for name, out := range map[string]string{
		"%v":   fmt.Sprintf("%v", cfg.APIKey),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg.APIKey),
		"%s":   fmt.Sprintf("%s", cfg.APIKey),
		"slog": logs.String(),
		"json": string(asJSON),
	} {
		if strings.Contains(out, raw) {
			t.Errorf("%s output discloses the secret: %s", name, out)
		}
	}
	if cfg.APIKey.Reveal() != raw {
		t.Error("Reveal must return the original value")
	}
}
