package main

import (
	"flag"
	"fmt"
	"os"

	"hy2tun/internal/manager"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hy2tun-manager: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/hy2tun/manager.yaml", "path to the manager configuration file")
	flag.Parse()

	cfg, err := manager.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	store, err := manager.OpenStore(cfg.StateFile, cfg.KeyFile)
	if err != nil {
		return err
	}
	audit := manager.NewAuditLog(cfg.AuditFile)
	_ = audit

	fmt.Printf("hy2tun-manager: configuration loaded (listen=%s base_path=%s servers=%d)\n",
		cfg.Listen, cfg.BasePath, len(store.State().Servers))
	return nil
}
