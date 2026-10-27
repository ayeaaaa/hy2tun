package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/logging"
	"hy2tun/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hy2tun: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	showVersion := flag.Bool("version", false, "print version and exit")
	configPath := flag.String("c", "client.yaml", "path to client config")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return nil
	}

	cfg, err := config.LoadClient(*configPath)
	if err != nil {
		return err
	}
	hadClientID := cfg.Client.ID != ""
	log, closer, err := logging.New(cfg.Log.Level, cfg.Log.File)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	cl, err := client.New(cfg, log)
	if err != nil {
		return err
	}
	if cfg.Client.ID != "" && !hadClientID {
		if err := config.SaveClient(*configPath, cfg); err != nil {
			_ = cl.Close()
			return fmt.Errorf("save generated client ID: %w", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("hy2tun starting", "config", *configPath, "server", cfg.Server)
	if err := cl.Start(ctx); err != nil {
		_ = cl.Close()
		return err
	}
	if err := cl.Close(); err != nil {
		return err
	}
	log.Info("hy2tun stopped")
	return nil
}
