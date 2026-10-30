package main

import (
	"context"
	"errors"
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
	if len(os.Args) > 1 && os.Args[1] == "config" {
		os.Exit(runConfig(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	checkConfig := flag.Bool("check-config", false, "validate configuration and exit")
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
	if *checkConfig {
		if err := config.CheckClient(cfg); err != nil {
			return err
		}
		fmt.Println("configuration OK")
		return nil
	}
	if cfg.WebEnabled() {
		password, generated, err := config.EnsureWebPassword(&cfg.Web)
		if err != nil {
			return err
		}
		if generated {
			if err := config.SaveClient(*configPath, cfg); err != nil && !errors.Is(err, config.ErrCommitted) {
				return fmt.Errorf("save generated web password: %w", err)
			}
			// The plaintext is shown exactly once and is never logged through
			// the structured logger.
			fmt.Fprintf(os.Stderr, "hy2tun: web console password for %q: %s (shown once)\n", cfg.Web.Username, password)
		}
	}
	log, closer, err := logging.New(cfg.Log.Level, cfg.Log.File)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	cl, err := client.NewSupervisor(cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("hy2tun starting", "config", *configPath, "servers", len(cfg.ServerConfigs()), "web", cl.WebAddr())
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
