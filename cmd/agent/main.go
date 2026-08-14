// Command monitorrr-agent is the endpoint collector. One codebase builds for
// Windows, macOS, and Linux; the result is a static binary with no runtime
// dependency on the target machine.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ollie/monitorrr/internal/agent"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "monitorrr-agent: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfg     agent.Config
		debug   bool
		version bool
	)
	flag.StringVar(&cfg.ServerURL, "server", os.Getenv("MONITORRR_SERVER"), "monitorrr server base URL, e.g. https://monitorrr.lab:8080")
	flag.StringVar(&cfg.EnrollToken, "enroll-token", os.Getenv("MONITORRR_ENROLL_TOKEN"), "enrollment token; only needed until this device has an identity")
	flag.StringVar(&cfg.StatePath, "state", os.Getenv("MONITORRR_STATE"), "identity file path (default is per-OS system location)")
	flag.BoolVar(&cfg.Insecure, "insecure", false, "skip TLS certificate verification (self-signed lab certificates)")
	flag.BoolVar(&cfg.Once, "once", false, "check in once and exit")
	flag.BoolVar(&debug, "debug", false, "verbose logging")
	flag.BoolVar(&version, "version", false, "print version and exit")
	flag.Parse()

	if version {
		fmt.Println("monitorrr-agent", agent.FullVersion())
		return nil
	}

	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	a, err := agent.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Info("monitorrr agent starting", "version", agent.FullVersion(), "server", cfg.ServerURL)
	return a.Run(ctx)
}
