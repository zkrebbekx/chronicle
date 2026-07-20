// Command chronicled is chronicle as a standalone REST service: the
// deployment shape for polyglot shops that cannot import the Go library.
// Configuration is environment-only, authentication is a static bearer-token
// table mapping each token to the actor it writes as, and the bitemporal
// semantics over the wire are exactly the library's. See the repository
// README's "Running the service" section and /v1/openapi.yaml.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zkrebbekx/chronicle/chronicled/internal/boot"
	"github.com/zkrebbekx/chronicle/chronicled/internal/config"
)

func main() {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chronicled:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err := serve(cfg, logger); err != nil {
		logger.Error("chronicled failed", "err", err.Error())
		os.Exit(1)
	}
}

// serve runs the service until SIGTERM or SIGINT, then drains. It exists so
// that main's os.Exit calls sit outside any function with defers.
func serve(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return boot.Run(ctx, cfg, logger, nil)
}
