// Package config loads chronicled's configuration from the environment and
// fails fast — a service that boots half-configured records nothing, or worse,
// records under the wrong identity. Every error message says which variable is
// wrong and what a right value looks like.
//
// There is no configuration library and no file format beyond JSON for the
// token table: the environment is the interface, which is the twelve-factor
// shape and the one every orchestrator already speaks.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Environment variable names. Collected here so the docs, the errors and the
// code cannot drift apart.
const (
	EnvDSN             = "CHRONICLED_DSN"
	EnvAddr            = "CHRONICLED_ADDR"
	EnvTokens          = "CHRONICLED_TOKENS"
	EnvTokensFile      = "CHRONICLED_TOKENS_FILE"
	EnvChaining        = "CHRONICLED_CHAINING"
	EnvMigrate         = "CHRONICLED_MIGRATE"
	EnvLogLevel        = "CHRONICLED_LOG_LEVEL"
	EnvReadTimeout     = "CHRONICLED_READ_TIMEOUT"
	EnvWriteTimeout    = "CHRONICLED_WRITE_TIMEOUT"
	EnvIdleTimeout     = "CHRONICLED_IDLE_TIMEOUT"
	EnvShutdownTimeout = "CHRONICLED_SHUTDOWN_TIMEOUT"
)

// Actor is the identity a token writes as. It mirrors chronicle.Actor; the
// service stamps it on every write made with the token, and no request body
// can override it.
type Actor struct {
	// ID is required — chronicle refuses writes with an empty actor ID, and
	// chronicled refuses to boot with one, which is earlier and better.
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

// Token is one entry in the token table: a static bearer token, the actor it
// authenticates as, and its role.
//
// This is API-key authentication for a single-trust-zone deployment: anyone
// holding the token is the actor, full stop. Put mTLS or an OIDC-aware proxy
// in front for anything bigger; chronicled deliberately does not grow an
// identity provider.
type Token struct {
	Token string `json:"token"`
	// Role is "writer" (write and read) or "admin" (also legal holds,
	// retention sweeps, crypto-shredding and chain verification).
	Role  string `json:"role"`
	Actor Actor  `json:"actor"`
}

// Config is everything chronicled needs to run.
type Config struct {
	DSN      string
	Addr     string
	Tokens   []Token
	Chaining bool
	Migrate  bool
	LogLevel slog.Level

	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

const exampleTokens = `[{"token":"s3cret","role":"writer","actor":{"id":"svc-payroll","type":"service","name":"Payroll"}}]`

// Load reads configuration through getenv (os.Getenv in production, a map in
// tests). It validates everything it can without touching the network, and
// the first problem it finds is the error — with the variable name and an
// example of a value that would have worked.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:            ":8080",
		LogLevel:        slog.LevelInfo,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 15 * time.Second,
	}

	cfg.DSN = getenv(EnvDSN)
	if cfg.DSN == "" {
		return Config{}, fmt.Errorf("%s is required: a PostgreSQL DSN such as postgres://user:pass@localhost:5432/chronicle?sslmode=disable", EnvDSN)
	}

	if v := getenv(EnvAddr); v != "" {
		cfg.Addr = v
	}

	tokens, err := loadTokens(getenv)
	if err != nil {
		return Config{}, err
	}
	cfg.Tokens = tokens

	switch v := getenv(EnvChaining); v {
	case "", "off":
		cfg.Chaining = false
	case "on":
		cfg.Chaining = true
	default:
		return Config{}, fmt.Errorf("%s must be \"on\" or \"off\", got %q", EnvChaining, v)
	}

	switch v := getenv(EnvMigrate); v {
	case "", "false":
		cfg.Migrate = false
	case "true":
		cfg.Migrate = true
	default:
		return Config{}, fmt.Errorf("%s must be \"true\" or \"false\", got %q", EnvMigrate, v)
	}

	switch v := getenv(EnvLogLevel); v {
	case "", "info":
		cfg.LogLevel = slog.LevelInfo
	case "debug":
		cfg.LogLevel = slog.LevelDebug
	case "warn":
		cfg.LogLevel = slog.LevelWarn
	case "error":
		cfg.LogLevel = slog.LevelError
	default:
		return Config{}, fmt.Errorf("%s must be one of debug, info, warn, error; got %q", EnvLogLevel, v)
	}

	for _, d := range []struct {
		env string
		dst *time.Duration
	}{
		{EnvReadTimeout, &cfg.ReadTimeout},
		{EnvWriteTimeout, &cfg.WriteTimeout},
		{EnvIdleTimeout, &cfg.IdleTimeout},
		{EnvShutdownTimeout, &cfg.ShutdownTimeout},
	} {
		v := getenv(d.env)
		if v == "" {
			continue
		}
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s must be a Go duration such as \"30s\", got %q", d.env, v)
		}
		if parsed <= 0 {
			return Config{}, fmt.Errorf("%s must be positive, got %q", d.env, v)
		}
		*d.dst = parsed
	}

	return cfg, nil
}

// loadTokens reads the token table from CHRONICLED_TOKENS or
// CHRONICLED_TOKENS_FILE — exactly one of them, because a deployment that
// sets both has two sources of truth and no way to tell which one won.
func loadTokens(getenv func(string) string) ([]Token, error) {
	inline := getenv(EnvTokens)
	file := getenv(EnvTokensFile)

	var raw []byte
	switch {
	case inline != "" && file != "":
		return nil, fmt.Errorf("set exactly one of %s and %s, not both", EnvTokens, EnvTokensFile)
	case inline == "" && file == "":
		return nil, fmt.Errorf("one of %s or %s is required: a JSON array such as %s", EnvTokens, EnvTokensFile, exampleTokens)
	case inline != "":
		raw = []byte(inline)
	default:
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s=%s: %w", EnvTokensFile, file, err)
		}
		raw = b
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var tokens []Token
	if err := dec.Decode(&tokens); err != nil {
		return nil, fmt.Errorf("token table is not valid JSON (%v); expected an array such as %s", err, exampleTokens)
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("token table is empty; at least one entry is required, such as %s", exampleTokens)
	}

	seen := make(map[string]bool, len(tokens))
	for i, t := range tokens {
		where := fmt.Sprintf("token table entry %d", i)
		if t.Token == "" {
			return nil, fmt.Errorf("%s: \"token\" is required and must not be empty", where)
		}
		if seen[t.Token] {
			return nil, fmt.Errorf("%s: duplicate token value; each token must map to exactly one actor", where)
		}
		seen[t.Token] = true
		if t.Role != "writer" && t.Role != "admin" {
			return nil, fmt.Errorf("%s: \"role\" must be \"writer\" or \"admin\", got %q", where, t.Role)
		}
		if t.Actor.ID == "" {
			return nil, fmt.Errorf("%s: \"actor.id\" is required — chronicle refuses writes without an actor, and an audit log must know who this token writes as", where)
		}
	}
	return tokens, nil
}
