package boot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/zkrebbekx/chronicle/chronicled/internal/config"
)

// TestRunEndToEnd boots the whole service against a live Postgres — config,
// migration, HTTP, auth, one write, graceful shutdown — gated on
// CHRONICLE_TEST_DSN. This is the one test that exercises the composition
// root itself.
func TestRunEndToEnd(t *testing.T) {
	dsn := os.Getenv("CHRONICLE_TEST_DSN")
	if dsn == "" {
		t.Skip("CHRONICLE_TEST_DSN not set; skipping boot integration test")
	}

	// Isolate in a dedicated schema via search_path, dropped afterwards.
	schema := fmt.Sprintf("chronicled_boot_%d", time.Now().UnixNano())
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close() }()
	if _, err := admin.Exec(`CREATE SCHEMA "` + schema + `"`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() {
		if _, err := admin.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	}()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	q := u.Query()
	q.Set("options", "-csearch_path="+schema)
	u.RawQuery = q.Encode()

	cfg := config.Config{
		DSN:  u.String(),
		Addr: "127.0.0.1:0",
		Tokens: []config.Token{
			{Token: "boot-writer", Role: "writer", Actor: config.Actor{ID: "svc-boot", Type: "service", Name: "Boot Test"}},
		},
		Chaining:        true,
		Migrate:         true,
		LogLevel:        slog.LevelInfo,
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    10 * time.Second,
		IdleTimeout:     30 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)),
			func(a net.Addr) { addrCh <- a })
	}()

	var base string
	select {
	case a := <-addrCh:
		base = "http://" + a.String()
	case err := <-done:
		t.Fatalf("Run returned before listening: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("service did not start listening")
	}

	get := func(path, token string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", base+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, body
	}

	// Liveness and readiness, unauthenticated.
	if resp, body := get("/healthz", ""); resp.StatusCode != 200 {
		t.Fatalf("healthz = %d: %s", resp.StatusCode, body)
	}
	if resp, body := get("/readyz", ""); resp.StatusCode != 200 {
		t.Fatalf("readyz = %d: %s", resp.StatusCode, body)
	}

	// One authenticated write and read through the booted stack, proving
	// migration ran and the token table works.
	putBody := strings.NewReader(`{"data":{"v":1},"validFrom":"2026-01-01T00:00:00Z"}`)
	req, _ := http.NewRequest("POST", base+"/v1/thing/x/records", putBody)
	req.Header.Set("Authorization", "Bearer boot-writer")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	created, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("write = %d: %s", resp.StatusCode, created)
	}
	var result struct {
		Record struct {
			Actor struct{ ID string } `json:"actor"`
		} `json:"record"`
	}
	if err := json.Unmarshal(created, &result); err != nil {
		t.Fatalf("unmarshal %s: %v", created, err)
	}
	if result.Record.Actor.ID != "svc-boot" {
		t.Fatalf("actor = %q, want the token's actor", result.Record.Actor.ID)
	}
	if resp, body := get("/v1/thing/x", "boot-writer"); resp.StatusCode != 200 {
		t.Fatalf("read back = %d: %s", resp.StatusCode, body)
	}

	// Graceful shutdown: cancelling the context drains and returns nil.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestRunFailsFastOnBadDatabase: an unreachable database is a boot error
// with an actionable message, not a server that limps up.
func TestRunFailsFastOnBadDatabase(t *testing.T) {
	cfg := config.Config{
		DSN:  "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable",
		Addr: "127.0.0.1:0",
		Tokens: []config.Token{
			{Token: "t", Role: "writer", Actor: config.Actor{ID: "a"}},
		},
		ReadTimeout:     time.Second,
		WriteTimeout:    time.Second,
		IdleTimeout:     time.Second,
		ShutdownTimeout: time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil {
		t.Fatal("Run succeeded against an unreachable database")
	}
	if !strings.Contains(err.Error(), config.EnvDSN) {
		t.Fatalf("error %q does not point at %s", err, config.EnvDSN)
	}
}

// TestRunRejectsBadCredentials: the auth table is validated before anything
// touches the network.
func TestRunRejectsBadCredentials(t *testing.T) {
	cfg := config.Config{
		DSN:    "postgres://x@localhost/x",
		Tokens: []config.Token{{Token: "t", Role: "sudo", Actor: config.Actor{ID: "a"}}},
	}
	if err := Run(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err == nil {
		t.Fatal("Run accepted an unknown role")
	}
}
