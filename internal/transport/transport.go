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
	Version       int
	Nonce         string
	Mode          config.TransportMode
	ServerVersion string
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
