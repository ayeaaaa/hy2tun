package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"
)

// CheckServer validates server files and runtime paths without binding sockets
// or writing the configuration. It is used by `hy2tund --check-config`.
func CheckServer(cfg *ServerConfig) error {
	if cfg == nil {
		return fmt.Errorf("server config is nil")
	}
	if cfg.TLS.Cert == "" || cfg.TLS.Key == "" {
		return fmt.Errorf("tls.cert and tls.key are required")
	}
	if _, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key); err != nil {
		return fmt.Errorf("load TLS key pair: %w", err)
	}
	if err := checkWebTLS(cfg.Web); err != nil {
		return err
	}
	if err := checkWebPassword(cfg.Web.PasswordHash); err != nil {
		return err
	}
	if cfg.StateFile != "" {
		if err := checkParentWritable(cfg.StateFile); err != nil {
			return fmt.Errorf("state_file: %w", err)
		}
	}
	if cfg.Log.File != "" {
		if err := checkParentWritable(cfg.Log.File); err != nil {
			return fmt.Errorf("log.file: %w", err)
		}
	}
	return nil
}

// CheckClient validates client files and runtime paths without binding sockets
// or writing the configuration. It is used by `hy2tun --check-config`.
func CheckClient(cfg *ClientConfig) error {
	if cfg == nil {
		return fmt.Errorf("client config is nil")
	}
	for _, server := range cfg.ServerConfigs() {
		if server.TLS.CA == "" {
			continue
		}
		data, err := os.ReadFile(server.TLS.CA)
		if err != nil {
			return fmt.Errorf("read TLS CA %s: %w", server.TLS.CA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return fmt.Errorf("parse TLS CA %s: no certificates found", server.TLS.CA)
		}
	}
	if err := checkWebTLS(cfg.Web); err != nil {
		return err
	}
	if err := checkWebPassword(cfg.Web.PasswordHash); err != nil {
		return err
	}
	if cfg.Log.File != "" {
		if err := checkParentWritable(cfg.Log.File); err != nil {
			return fmt.Errorf("log.file: %w", err)
		}
	}
	return nil
}

func checkWebTLS(web WebConfig) error {
	if (web.TLS.Cert == "") != (web.TLS.Key == "") {
		return fmt.Errorf("web.tls.cert and web.tls.key must be set together")
	}
	if web.TLS.Cert == "" {
		return nil
	}
	if _, err := tls.LoadX509KeyPair(web.TLS.Cert, web.TLS.Key); err != nil {
		return fmt.Errorf("load web TLS key pair: %w", err)
	}
	return nil
}

func checkWebPassword(hash string) error {
	if hash == "" {
		return nil
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return fmt.Errorf("web.password_hash is not a valid bcrypt hash: %w", err)
	}
	return nil
}

func checkParentWritable(path string) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	f, err := os.CreateTemp(dir, ".hy2tun-check-*")
	if err != nil {
		return fmt.Errorf("directory %s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}
