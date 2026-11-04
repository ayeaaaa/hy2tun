// Package transport defines the tunnel session contract independently of the
// underlying HY2 or TLS/TCP implementation.
package transport

import (
	"context"
	"crypto/tls"
	"net"

	hyclient "github.com/apernet/hysteria/core/v2/client"

	"hy2tun/internal/config"
)

type Kind string

const (
	HY2 Kind = "hy2"
	TCP Kind = "tcp"
)

// TCPRequest selects either a reverse stream token or a forward proxy target.
// Exactly one field must be nonempty.
type TCPRequest struct {
	ReverseToken string
	Target       string
}

type DatagramConn interface {
	Receive() ([]byte, string, error)
	Send([]byte, string) error
	Close() error
}

type ProbeInfo struct {
	Version       int                  `json:"version"`
	Nonce         string               `json:"nonce"`
	Mode          config.TransportMode `json:"mode"`
	ServerVersion string               `json:"server_version"`
}

// Session owns its transport. Cancellation of an in-flight HY2 open closes the
// session because the upstream synchronous API has no per-operation context.
type Session interface {
	Kind() Kind
	OpenControl(context.Context) (net.Conn, error)
	OpenTCP(context.Context, TCPRequest) (net.Conn, error)
	OpenUDP(context.Context, string) (DatagramConn, error)
	Probe(context.Context) (ProbeInfo, error)
	Close() error
}

// CompletionSession optionally exposes the actual completion of asynchronous
// data opens separately from their prompt cancellation return. Each call returns
// a non-nil channel, closed only after native work and any abandoned/error-child
// cleanup finish. On successful handoff it does NOT wait for the delivered
// child's lifetime. Validation/pre-cancelled calls return an already-closed
// channel. Context cancellation or Session.Close alone is not completion.
//
// Asynchronous HY2 wrappers must preserve this contract when a caller accounts
// for pending work across replacement roots; a HY2 implementation without it is
// treated as synchronous by that external budget. TCP retains its own native
// reservation/cleanup budget and need not implement this interface. Existing
// Session methods and cancellation/root-ownership rules remain unchanged.
type CompletionSession interface {
	Session
	OpenTCPWithCompletion(context.Context, TCPRequest) (net.Conn, <-chan struct{}, error)
	OpenUDPWithCompletion(context.Context, string) (DatagramConn, <-chan struct{}, error)
}

// ProbeCompletionSession reports completion of the native probe exchange and
// all late/abandoned child cleanup, independently of prompt cancellation/Close.
// Every receipt is non-nil. Asynchronous decorators must forward the receipt.
type ProbeCompletionSession interface {
	Session
	ProbeWithCompletion(context.Context) (ProbeInfo, <-chan struct{}, error)
}

// ControlCompletionSession provides the same native-work receipt for setup's
// control open. Successful completion does not wait for the live control stream.
type ControlCompletionSession interface {
	Session
	OpenControlWithCompletion(context.Context) (net.Conn, <-chan struct{}, error)
}

type DialOptions struct {
	// HY2 contains the existing, complete Hysteria configuration. DialHY2
	// copies it before wrapping its packet factory; it does not mutate it.
	HY2          *hyclient.Config
	TCPAddress   string
	TLS          *tls.Config
	Auth         string
	ControlToken string
	ClientID     string
	Up, Down     uint64 // Bytes per second; zero means unlimited.
}

type HY2Factory func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error)
