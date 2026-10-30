package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"time"

	"hy2tun/internal/config"
)

// serviceRunning reports whether the hy2tun init script considers the service
// running. It is a package variable so tests can stub the init system.
var serviceRunning = func() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/etc/init.d/hy2tun", "status")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

// configError is the stable JSON error shape consumed by the LuCI controller.
type configError struct {
	OK       bool   `json:"ok"`
	Code     string `json:"code"`
	Error    string `json:"error"`
	Advanced bool   `json:"advanced"`
}

// configResult is the success shape for `config set`.
type configResult struct {
	OK       bool   `json:"ok"`
	Revision string `json:"revision"`
}

// runConfig implements `hy2tun config get|set` and returns a process exit code.
func runConfig(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		configUsage(stderr)
		return 1
	}
	switch args[0] {
	case "get":
		return runConfigGet(args[1:], stdout, stderr)
	case "set":
		return runConfigSet(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hy2tun: unknown config command %q\n", args[0])
		configUsage(stderr)
		return 1
	}
}

func configUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: hy2tun config get [-c path]")
	fmt.Fprintln(w, "       hy2tun config set [-c path] [--require-stopped]")
}

func runConfigGet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("c", "client.yaml", "path to client config")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: hy2tun config get [-c path]")
		return 1
	}

	state, err := config.GetBasic(*path)
	if err != nil {
		return writeConfigError(stderr, err)
	}
	if err := json.NewEncoder(stdout).Encode(state); err != nil {
		return writeConfigError(stderr, &config.BasicError{
			Code:    config.BasicCodeInternal,
			Message: fmt.Sprintf("encode response: %v", err),
		})
	}
	return 0
}

func runConfigSet(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("c", "client.yaml", "path to client config")
	requireStopped := fs.Bool("require-stopped", false, "refuse to write while the service is running")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: hy2tun config set [-c path] [--require-stopped]")
		return 1
	}

	// Check before reading stdin so a running service short-circuits the
	// request; BeforeWrite repeats the check under the configuration lock.
	if *requireStopped && serviceRunning() {
		return writeConfigError(stderr, serviceRunningError())
	}

	var update config.BasicUpdate
	decoder := json.NewDecoder(stdin)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&update); err != nil {
		return writeConfigError(stderr, &config.BasicError{
			Code:    config.BasicCodeParse,
			Message: fmt.Sprintf("decode request: %v", err),
		})
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		message := "decode request: trailing data after JSON object"
		if err != nil {
			message = fmt.Sprintf("decode request: trailing data after JSON object: %v", err)
		}
		return writeConfigError(stderr, &config.BasicError{
			Code:    config.BasicCodeParse,
			Message: message,
		})
	}

	opts := config.SetBasicOptions{}
	if *requireStopped {
		opts.BeforeWrite = func() error {
			if serviceRunning() {
				return serviceRunningError()
			}
			return nil
		}
	}
	state, err := config.SetBasic(*path, &update, opts)
	if err != nil {
		return writeConfigError(stderr, err)
	}
	if err := json.NewEncoder(stdout).Encode(configResult{OK: true, Revision: state.Revision}); err != nil {
		return writeConfigError(stderr, &config.BasicError{
			Code:    config.BasicCodeInternal,
			Message: fmt.Sprintf("encode response: %v", err),
		})
	}
	return 0
}

func serviceRunningError() *config.BasicError {
	return &config.BasicError{
		Code:    config.BasicCodeServiceRunning,
		Message: "stop the hy2tun service before changing the configuration",
	}
}

// writeConfigError prints the stable JSON error to stderr and maps the basic
// error code to the documented exit code.
func writeConfigError(stderr io.Writer, err error) int {
	var basicErr *config.BasicError
	if !errors.As(err, &basicErr) {
		message := "unknown error"
		if err != nil {
			message = err.Error()
		}
		basicErr = &config.BasicError{Code: config.BasicCodeInternal, Message: message}
	}
	code := basicErr.Code
	if code == "" {
		code = config.BasicCodeInternal
	}
	_ = json.NewEncoder(stderr).Encode(configError{
		OK:       false,
		Code:     string(code),
		Error:    basicErr.Error(),
		Advanced: basicErr.Advanced,
	})
	return configExitCode(code)
}

func configExitCode(code config.BasicErrorCode) int {
	switch code {
	case config.BasicCodeValidation:
		return 2
	case config.BasicCodeConflict:
		return 3
	case config.BasicCodeUnknownFields, config.BasicCodeMulti:
		return 4
	case config.BasicCodeAdvanced:
		return 5
	case config.BasicCodeServiceRunning:
		return 6
	default:
		return 1
	}
}
