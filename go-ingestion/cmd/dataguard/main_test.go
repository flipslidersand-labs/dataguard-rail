package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestCheckAPIKeyRequirement(t *testing.T) {
	cases := []struct {
		name             string
		apiKey           string
		allowInsecureEnv string
		wantErr          bool
	}{
		{"api key set", "secret", "", false},
		{"api key set, env ignored", "secret", "1", false},
		{"no api key, no opt-in", "", "", true},
		{"no api key, wrong opt-in value", "", "true", true},
		{"no api key, explicit opt-in", "", "1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkAPIKeyRequirement(c.apiKey, c.allowInsecureEnv)
			if (err != nil) != c.wantErr {
				t.Errorf("checkAPIKeyRequirement(%q, %q) error = %v, wantErr %v", c.apiKey, c.allowInsecureEnv, err, c.wantErr)
			}
		})
	}
}

func TestServeAddrDefaultIsLoopback(t *testing.T) {
	f := newServeCmd().Flags().Lookup("addr")
	if f == nil {
		t.Fatal("--addr flag not found")
	}
	if f.DefValue != "127.0.0.1:8080" {
		t.Errorf("--addr default = %q, want %q", f.DefValue, "127.0.0.1:8080")
	}
	if err := checkListenPolicy(f.DefValue, "", "", ""); err != nil {
		t.Errorf("default addr must start without TLS: %v", err)
	}
}

func TestCheckListenPolicy(t *testing.T) {
	cases := []struct {
		name                   string
		addr, cert, key, allow string
		wantErr                bool
	}{
		{"loopback v4, no tls", "127.0.0.1:8080", "", "", "", false},
		{"loopback v6, no tls", "[::1]:8080", "", "", "", false},
		{"localhost, no tls", "localhost:8080", "", "", "", false},
		{"all interfaces (:port), no tls, no opt-in", ":8080", "", "", "", true},
		{"0.0.0.0, no tls, no opt-in", "0.0.0.0:8080", "", "", "", true},
		{"external ip, no tls, wrong opt-in value", "192.0.2.10:8080", "", "", "true", true},
		{"0.0.0.0, no tls, explicit opt-in", "0.0.0.0:8080", "", "", "1", false},
		{"0.0.0.0, tls", "0.0.0.0:8443", "c.pem", "k.pem", "", false},
		{"cert without key", "127.0.0.1:8080", "c.pem", "", "", true},
		{"key without cert", "127.0.0.1:8080", "", "k.pem", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkListenPolicy(c.addr, c.cert, c.key, c.allow)
			if (err != nil) != c.wantErr {
				t.Errorf("checkListenPolicy(%q,%q,%q,%q) error = %v, wantErr %v", c.addr, c.cert, c.key, c.allow, err, c.wantErr)
			}
		})
	}
}

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveSecretFromFileTrimsAndDoesNotWarn(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	got, err := resolveSecret(zap.New(core), "api-key", "", writeSecretFile(t, "  s3cret\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cret" {
		t.Errorf("got %q, want %q", got, "s3cret")
	}
	if logs.Len() != 0 {
		t.Errorf("file input must not warn: %+v", logs.All())
	}
}

func TestResolveSecretFromEnvFallback(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	got, err := resolveSecret(zap.New(core), "api-key", "", "", "from-env")
	if err != nil || got != "from-env" {
		t.Fatalf("got %q, %v; want from-env", got, err)
	}
	if logs.Len() != 0 {
		t.Errorf("env input must not warn: %+v", logs.All())
	}
}

func TestResolveSecretArgWarnsWithoutLoggingValue(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	got, err := resolveSecret(zap.New(core), "api-key", "from-arg", "", "")
	if err != nil || got != "from-arg" {
		t.Fatalf("got %q, %v; want from-arg", got, err)
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 warning, got %d", len(entries))
	}
	if !strings.Contains(entries[0].Message, "プロセス一覧") {
		t.Errorf("warning should mention process listing: %q", entries[0].Message)
	}
	if strings.Contains(fmt.Sprintf("%+v", entries[0]), "from-arg") {
		t.Error("secret value must not be logged")
	}
}

func TestResolveSecretErrors(t *testing.T) {
	log := zap.NewNop()
	if _, err := resolveSecret(log, "api-key", "a", writeSecretFile(t, "b"), ""); err == nil {
		t.Error("arg + file together must be rejected")
	}
	if _, err := resolveSecret(log, "api-key", "", writeSecretFile(t, " \n"), ""); err == nil {
		t.Error("empty file must be rejected")
	}
	if _, err := resolveSecret(log, "api-key", "", filepath.Join(t.TempDir(), "missing"), ""); err == nil {
		t.Error("missing file must be rejected")
	}
}
