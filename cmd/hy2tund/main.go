package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"hy2tun/internal/config"
	"hy2tun/internal/logging"
	"hy2tun/internal/server"
	"hy2tun/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hy2tund: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("c", "server.yaml", "path to server config")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return nil
	}

	cfg, err := config.LoadServer(*configPath)
	if err != nil {
		return err
	}
	log, closer, err := logging.New(cfg.Log.Level, cfg.Log.File)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("hy2tund starting", "config", *configPath, "web", srv.WebAddr())
	if err := srv.Start(ctx); err != nil {
		_ = srv.Close()
		return err
	}
	if err := srv.Close(); err != nil {
		return err
	}
	log.Info("hy2tund stopped")
	return nil
}
