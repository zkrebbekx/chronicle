// Package boot wires chronicled together and runs it: database, store,
// keyring, log, HTTP server, graceful shutdown. It is the composition root;
// everything above it is testable without a database and everything below it
// is the chronicle libraries.
package boot

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver; pgstore imports none itself

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/chronicled/internal/api"
	"github.com/zkrebbekx/chronicle/chronicled/internal/auth"
	"github.com/zkrebbekx/chronicle/chronicled/internal/config"
	"github.com/zkrebbekx/chronicle/pgstore"
)

// Run starts chronicled and serves until ctx is cancelled — which main wires
// to SIGTERM and SIGINT — then drains in-flight requests for up to the
// configured shutdown timeout before closing the database.
//
// onListen, when non-nil, is called with the bound address once the listener
// is up; tests use it to find the ephemeral port.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, onListen func(net.Addr)) error {
	creds := make([]auth.Credential, 0, len(cfg.Tokens))
	for _, t := range cfg.Tokens {
		creds = append(creds, auth.Credential{
			Token: t.Token,
			Principal: auth.Principal{
				Actor: chronicle.Actor{ID: t.Actor.ID, Type: t.Actor.Type, Name: t.Actor.Name},
				Role:  auth.Role(t.Role),
			},
		})
	}
	authn, err := auth.New(creds)
	if err != nil {
		return err
	}

	db, err := sql.Open("pgx", cfg.DSN)
	if err != nil {
		return fmt.Errorf("open database (check %s): %w", config.EnvDSN, err)
	}
	defer func() { _ = db.Close() }()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("database unreachable — is Postgres up and %s correct? %w", config.EnvDSN, err)
	}

	store, err := pgstore.New(db)
	if err != nil {
		return fmt.Errorf("configure store: %w", err)
	}
	keyring, err := pgstore.NewKeyring(db)
	if err != nil {
		return fmt.Errorf("configure keyring: %w", err)
	}

	// Migration is opt-in (CHRONICLED_MIGRATE=true) and off by default:
	// production schema changes should be an explicit, reviewed act, not a
	// side effect of whichever replica booted first with a newer binary.
	// Apply pgstore's SchemaSQL/KeysSchemaSQL through your migration tool,
	// or flip the flag for demos and development. Migrate is idempotent and
	// safe under concurrent boots, so the flag is about change control, not
	// safety.
	if cfg.Migrate {
		if err := store.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate records schema: %w", err)
		}
		if err := keyring.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate keys schema: %w", err)
		}
		logger.Info("schema migrated")
	}

	// One chronicle.Log per process, over a store that assigns transaction
	// time. Horizontal replicas are safe for exactly that reason: pgstore
	// stamps every write from the database's clock, so no replica's clock is
	// ever authoritative and any number of Logs across processes produce one
	// correctly ordered transaction history. Note the ceiling that comes
	// with the simplicity: a Log serializes its writes, so one replica lands
	// one write at a time — see the phase 4 correction in docs/DESIGN.md.
	opts := []chronicle.Option{chronicle.WithKeyring(keyring)}
	if cfg.Chaining {
		opts = append(opts, chronicle.WithChaining())
	}
	log := chronicle.NewLog(store, opts...)

	handler := api.NewHandler(api.Deps{
		Log:     log,
		Store:   store,
		Keyring: keyring,
		Auth:    authn,
		Logger:  logger,
		Ready:   db.PingContext,
	})

	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s (check %s): %w", cfg.Addr, config.EnvAddr, err)
	}
	if onListen != nil {
		onListen(ln.Addr())
	}
	logger.Info("chronicled listening",
		"addr", ln.Addr().String(),
		"chaining", cfg.Chaining,
		"tokens", len(cfg.Tokens),
	)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "drain_timeout", cfg.ShutdownTimeout.String())
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelDrain()
	if err := srv.Shutdown(drainCtx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("drain: %w", err)
	}
	// The deferred db.Close runs after this return — connections close only
	// once every in-flight request has finished with them.
	logger.Info("stopped")
	return nil
}
