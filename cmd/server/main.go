// Command monitorrr-server runs the control plane: agent API, admin API, and
// web UI. It is a single static binary; the only state is the SQLite file.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/ollie/monitorrr/internal/server"
	"github.com/ollie/monitorrr/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "monitorrr-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfg     server.Config
		debug   bool
		version bool
	)
	flag.StringVar(&cfg.Addr, "addr", env("MONITORRR_ADDR", ":8080"), "listen address")
	flag.StringVar(&cfg.DBPath, "db", env("MONITORRR_DB", "monitorrr.db"), "path to the SQLite database file")
	flag.StringVar(&cfg.DistDir, "dist", env("MONITORRR_DIST", "dist"), "directory containing built agent binaries")
	flag.StringVar(&cfg.PublicURL, "public-url", os.Getenv("MONITORRR_PUBLIC_URL"), "base URL agents should call; inferred from the request when empty")
	flag.StringVar(&cfg.TLSCert, "tls-cert", os.Getenv("MONITORRR_TLS_CERT"), "TLS certificate file (enables HTTPS)")
	flag.StringVar(&cfg.TLSKey, "tls-key", os.Getenv("MONITORRR_TLS_KEY"), "TLS key file")
	flag.BoolVar(&debug, "debug", false, "verbose logging, including every heartbeat")
	flag.BoolVar(&version, "version", false, "print version and exit")
	flag.Parse()

	if version {
		fmt.Println("monitorrr-server", server.FullVersion())
		return nil
	}

	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return fmt.Errorf("-tls-cert and -tls-key must be provided together")
	}

	abs, err := filepath.Abs(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}

	st, err := store.Open(abs)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	token, err := st.EnrollToken(ctx)
	if err != nil {
		return err
	}

	log.Info("monitorrr starting", "version", server.FullVersion(), "db", abs, "dist", cfg.DistDir)
	log.Info("enrollment token", "token", token, "hint", "shown on the Deployment page")
	if configured, err := st.HasUsers(ctx); err == nil && !configured {
		log.Info("no accounts yet — open the web UI to create the first administrator")
	}
	if cfg.TLSCert == "" {
		log.Warn("serving plain HTTP — agent tokens cross the network in clear text; terminate TLS at a proxy or pass -tls-cert/-tls-key")
	}

	srv, err := server.New(cfg, st, log)
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
