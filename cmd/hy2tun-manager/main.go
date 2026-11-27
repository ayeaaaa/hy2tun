package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"hy2tun/internal/manager"
	"hy2tun/internal/version"
)

const (
	defaultConfigPath      = "/etc/hy2tun/manager.yaml"
	minAdminPasswordRunes  = 8
	maxAdminPasswordBytes  = 72
	shutdownGracePeriod    = 10 * time.Second
	managerReadTimeout     = 15 * time.Second
	managerWriteTimeout    = 60 * time.Second
	managerIdleTimeout     = 60 * time.Second
	managerReadHeaderLimit = 10 * time.Second
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "hy2tun-manager: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable command entry point. ready, when non-nil, receives the
// actual listener address after startup so tests can use 127.0.0.1:0.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, ready func(net.Addr)) error {
	// Accept the subcommand form documented in the design as well as the
	// -reset-admin flag required by the command interface.
	if len(args) > 0 && args[0] == "reset-admin" {
		args = append([]string{"-reset-admin"}, args[1:]...)
	}

	flags := flag.NewFlagSet("hy2tun-manager", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath, "path to the manager configuration file")
	resetAdmin := flags.Bool("reset-admin", false, "prompt for a new administrator password, save it, and exit")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *showVersion {
		fmt.Fprintf(stdout, "hy2tun-manager %s (commit %s, built %s)\n", version.Version, version.Commit, version.Date)
		return nil
	}
	if *resetAdmin {
		return resetAdminPassword(*configPath, stdin, stderr)
	}
	return serveManager(*configPath, stderr, ready)
}

// serveManager is the normal composition root: config, store, audit, inventory,
// Web shell, HTTP listener, background refresh, and signal-driven shutdown.
func serveManager(configPath string, stderr io.Writer, ready func(net.Addr)) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := manager.LoadConfig(configPath)
	if err != nil {
		return err
	}
	// Hold the state lock for the whole service lifetime. reset-admin uses the
	// same lock and must fail while this process is running: the live manager
	// has an in-memory hash snapshot and could otherwise overwrite the reset.
	stateLock, err := manager.AcquireStateLock(cfg.StateFile)
	if err != nil {
		return err
	}
	defer stateLock.Close()
	store, err := manager.OpenStore(cfg.StateFile, cfg.KeyFile)
	if err != nil {
		return err
	}
	audit := manager.NewAuditLog(cfg.AuditFile)
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	inventory := manager.NewInventory(store, audit, nil, log)
	web, err := manager.NewWebServer(store, audit, cfg, log)
	if err != nil {
		return err
	}
	web.SetInventory(inventory)

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.Handler(),
		ReadTimeout:       managerReadTimeout,
		ReadHeaderTimeout: managerReadHeaderLimit,
		WriteTimeout:      managerWriteTimeout,
		IdleTimeout:       managerIdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	defer listener.Close()
	if ready != nil {
		ready(listener.Addr())
	}
	log.Info("hy2tun-manager starting",
		"listen", listener.Addr().String(),
		"base_path", cfg.BasePath,
		"servers", len(store.State().Servers))

	refreshCtx, cancelRefresh := context.WithCancel(context.Background())
	var refreshWG sync.WaitGroup
	refreshWG.Add(1)
	go func() {
		defer refreshWG.Done()
		inventory.RefreshAll(refreshCtx)
		ticker := time.NewTicker(cfg.RefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-ticker.C:
				inventory.RefreshAll(refreshCtx)
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		log.Info("hy2tun-manager shutting down")
	case err := <-serveErr:
		cancelRefresh()
		refreshWG.Wait()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve manager HTTP: %w", err)
		}
		return nil
	}

	cancelRefresh()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGracePeriod)
	defer cancelShutdown()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	refreshWG.Wait()
	serveResult := <-serveErr
	if shutdownErr != nil {
		return fmt.Errorf("shutdown manager HTTP: %w", shutdownErr)
	}
	if serveResult != nil && !errors.Is(serveResult, http.ErrServerClosed) {
		return fmt.Errorf("serve manager HTTP: %w", serveResult)
	}
	log.Info("hy2tun-manager stopped")
	return nil
}

// resetAdminPassword changes only the administrator bcrypt hash in the state
// file. It refuses to run while a manager holds the state lock, because that
// process keeps a construction-time hash snapshot and could overwrite the new
// hash on its next state mutation. The operator must stop the service, run the
// reset, and then start the service again.
func resetAdminPassword(configPath string, stdin io.Reader, stderr io.Writer) error {
	cfg, err := manager.LoadConfig(configPath)
	if err != nil {
		return err
	}
	stateLock, err := manager.AcquireStateLock(cfg.StateFile)
	if err != nil {
		return err
	}
	defer stateLock.Close()

	store, err := manager.OpenStore(cfg.StateFile, cfg.KeyFile)
	if err != nil {
		return err
	}
	password, err := promptNewPassword(stdin, stderr)
	if err != nil {
		return err
	}
	audit := manager.NewAuditLog(cfg.AuditFile)
	if err := audit.Append(manager.AuditEntry{
		Actor:  "cli",
		Action: "admin.reset",
		Target: "admin",
		Result: "pending",
	}); err != nil {
		return fmt.Errorf("audit log is unavailable; administrator password was not changed: %w", err)
	}
	if err := store.SetAdminPassword(password); err != nil {
		_ = audit.Append(manager.AuditEntry{
			Actor:   "cli",
			Action:  "admin.reset",
			Target:  "admin",
			Result:  "error",
			Message: err.Error(),
		})
		return err
	}
	if err := audit.Append(manager.AuditEntry{
		Actor:  "cli",
		Action: "admin.reset",
		Target: "admin",
		Result: "ok",
	}); err != nil {
		// The hash is already committed. Report the audit durability failure
		// rather than pretending the reset failed; the operator can verify the
		// password and fix the audit log.
		return fmt.Errorf("administrator password updated but the audit log could not be written: %w", err)
	}
	fmt.Fprintln(stderr, "hy2tun-manager: administrator password updated; restart the running manager so it reloads the new hash and ends existing sessions")
	return nil
}

func promptNewPassword(stdin io.Reader, stderr io.Writer) (string, error) {
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		return readTerminalPasswordPair(file, stderr)
	}
	return readStreamPasswordPair(stdin, stderr)
}

func readTerminalPasswordPair(file *os.File, stderr io.Writer) (string, error) {
	first, err := readTerminalPassword(file, stderr, "New manager administrator password: ")
	if err != nil {
		return "", err
	}
	second, err := readTerminalPassword(file, stderr, "Confirm new password: ")
	if err != nil {
		return "", err
	}
	return validateNewPassword(first, second)
}

func readTerminalPassword(file *os.File, stderr io.Writer, prompt string) (string, error) {
	fmt.Fprint(stderr, prompt)
	value, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(value), nil
}

func readStreamPasswordPair(stdin io.Reader, stderr io.Writer) (string, error) {
	reader := bufio.NewReader(stdin)
	first, err := readStreamPassword(reader, stderr, "New manager administrator password: ")
	if err != nil {
		return "", err
	}
	second, err := readStreamPassword(reader, stderr, "Confirm new password: ")
	if err != nil {
		return "", err
	}
	return validateNewPassword(first, second)
}

func readStreamPassword(reader *bufio.Reader, stderr io.Writer, prompt string) (string, error) {
	fmt.Fprint(stderr, prompt)
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func validateNewPassword(password, confirmation string) (string, error) {
	if strings.TrimSpace(password) == "" {
		return "", fmt.Errorf("new administrator password must not be empty")
	}
	if utf8.RuneCountInString(password) < minAdminPasswordRunes {
		return "", fmt.Errorf("new administrator password must be at least %d characters", minAdminPasswordRunes)
	}
	if len(password) > maxAdminPasswordBytes {
		return "", fmt.Errorf("new administrator password must not exceed %d bytes", maxAdminPasswordBytes)
	}
	if password != confirmation {
		return "", fmt.Errorf("new administrator passwords do not match")
	}
	return password, nil
}
