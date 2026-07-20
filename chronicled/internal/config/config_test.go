package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validTokens = `[{"token":"t1","role":"writer","actor":{"id":"a1","type":"service","name":"One"}},` +
	`{"token":"t2","role":"admin","actor":{"id":"a2"}}]`

// env builds a getenv func over a map, with a valid baseline that individual
// cases override.
func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		EnvDSN:    "postgres://chronicle:chronicle@localhost:5432/chronicle?sslmode=disable",
		EnvTokens: validTokens,
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(key string) string { return base[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.Chaining || cfg.Migrate {
		t.Errorf("Chaining/Migrate default = %v/%v, want off/off", cfg.Chaining, cfg.Migrate)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.ReadTimeout != 10*time.Second || cfg.WriteTimeout != 30*time.Second ||
		cfg.IdleTimeout != 120*time.Second || cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("timeout defaults wrong: %+v", cfg)
	}
	if len(cfg.Tokens) != 2 || cfg.Tokens[0].Actor.ID != "a1" || cfg.Tokens[1].Role != "admin" {
		t.Errorf("tokens = %+v", cfg.Tokens)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		EnvAddr:            "127.0.0.1:9999",
		EnvChaining:        "on",
		EnvMigrate:         "true",
		EnvLogLevel:        "debug",
		EnvReadTimeout:     "1s",
		EnvWriteTimeout:    "2s",
		EnvIdleTimeout:     "3s",
		EnvShutdownTimeout: "4s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9999" || !cfg.Chaining || !cfg.Migrate || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("overrides not applied: %+v", cfg)
	}
	if cfg.ReadTimeout != time.Second || cfg.WriteTimeout != 2*time.Second ||
		cfg.IdleTimeout != 3*time.Second || cfg.ShutdownTimeout != 4*time.Second {
		t.Errorf("timeouts not applied: %+v", cfg)
	}
}

func TestLoadTokensFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte(validTokens), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(env(map[string]string{EnvTokens: "", EnvTokensFile: path}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Tokens) != 2 {
		t.Fatalf("tokens from file = %+v", cfg.Tokens)
	}
}

// TestLoadFailsFast: every misconfiguration is an error naming the variable,
// and where sensible, an example of a working value.
func TestLoadFailsFast(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		contains string
	}{
		{"missing DSN", map[string]string{EnvDSN: ""}, EnvDSN},
		{"no tokens", map[string]string{EnvTokens: ""}, EnvTokens},
		{"both token sources", map[string]string{EnvTokensFile: "/tmp/x"}, "exactly one"},
		{"unreadable file", map[string]string{EnvTokens: "", EnvTokensFile: "/does/not/exist"}, "read"},
		{"bad JSON", map[string]string{EnvTokens: "{not json"}, "JSON"},
		{"unknown token field", map[string]string{EnvTokens: `[{"token":"t","role":"writer","actor":{"id":"a"},"scope":"x"}]`}, "JSON"},
		{"empty array", map[string]string{EnvTokens: "[]"}, "empty"},
		{"empty token", map[string]string{EnvTokens: `[{"token":"","role":"writer","actor":{"id":"a"}}]`}, "token"},
		{"duplicate token", map[string]string{EnvTokens: `[{"token":"t","role":"writer","actor":{"id":"a"}},{"token":"t","role":"admin","actor":{"id":"b"}}]`}, "duplicate"},
		{"bad role", map[string]string{EnvTokens: `[{"token":"t","role":"root","actor":{"id":"a"}}]`}, "role"},
		{"missing actor id", map[string]string{EnvTokens: `[{"token":"t","role":"writer","actor":{"name":"x"}}]`}, "actor.id"},
		{"bad chaining", map[string]string{EnvChaining: "yes"}, EnvChaining},
		{"bad migrate", map[string]string{EnvMigrate: "1"}, EnvMigrate},
		{"bad log level", map[string]string{EnvLogLevel: "loud"}, EnvLogLevel},
		{"bad duration", map[string]string{EnvReadTimeout: "fast"}, EnvReadTimeout},
		{"negative duration", map[string]string{EnvShutdownTimeout: "-1s"}, EnvShutdownTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.env))
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error %q does not mention %q", err, tc.contains)
			}
		})
	}
}
