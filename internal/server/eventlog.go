package server

import (
	"log/slog"
	"net"

	hyserver "github.com/apernet/hysteria/core/v2/server"
)

var _ hyserver.EventLogger = (*eventLogger)(nil)

// eventLogger writes HY2 lifecycle and request events to the server log. It
// never receives authentication secrets; request targets are redacted when they
// contain a hy2tun virtual-address token.
type eventLogger struct {
	log *slog.Logger
}

func newEventLogger(log *slog.Logger) *eventLogger {
	if log == nil {
		log = slog.Default()
	}
	return &eventLogger{log: log}
}

// Connect logs a successful HY2 authentication.
func (e *eventLogger) Connect(addr net.Addr, id string, tx uint64) {
	e.log.Info("HY2 client connected",
		"remote", addrString(addr),
		"client", id,
		"tx_bytes", tx,
	)
}

// Disconnect logs a client disconnect, including the error when there is one.
func (e *eventLogger) Disconnect(addr net.Addr, id string, err error) {
	args := []any{"remote", addrString(addr), "client", id}
	args = append(args, errorAttrs(err)...)
	e.log.Info("HY2 client disconnected", args...)
}

// TCPRequest logs the target of a proxied TCP request.
func (e *eventLogger) TCPRequest(addr net.Addr, id, reqAddr string) {
	e.log.Debug("HY2 TCP request",
		"remote", addrString(addr),
		"client", id,
		"target", redactReqAddr(reqAddr),
	)
}

// TCPError logs a proxied TCP stream ending and its error, if any.
func (e *eventLogger) TCPError(addr net.Addr, id, reqAddr string, err error) {
	args := []any{
		"remote", addrString(addr),
		"client", id,
		"target", redactReqAddr(reqAddr),
	}
	args = append(args, errorAttrs(err)...)
	e.log.Debug("HY2 TCP error", args...)
}

// UDPRequest logs the target of a proxied UDP session.
func (e *eventLogger) UDPRequest(addr net.Addr, id string, sessionID uint32, reqAddr string) {
	e.log.Debug("HY2 UDP request",
		"remote", addrString(addr),
		"client", id,
		"session", sessionID,
		"target", redactReqAddr(reqAddr),
	)
}

// UDPError logs a proxied UDP session ending and its error, if any.
func (e *eventLogger) UDPError(addr net.Addr, id string, sessionID uint32, err error) {
	args := []any{
		"remote", addrString(addr),
		"client", id,
		"session", sessionID,
	}
	args = append(args, errorAttrs(err)...)
	e.log.Debug("HY2 UDP error", args...)
}

func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

func errorAttrs(err error) []any {
	if err == nil {
		return nil
	}
	return []any{"error", err.Error()}
}

// redactReqAddr hides tokens embedded in reserved hy2tun virtual addresses so
// they can never reach the server log.
func redactReqAddr(reqAddr string) string {
	kind, token, ok := parseVirtualAddr(reqAddr)
	if ok {
		if token == "" {
			return reqAddr
		}
		return redactedVirtualAddr(kind, reqAddr)
	}
	if isVirtualHost(reqAddr) {
		return redactedVirtualAddr("", reqAddr)
	}
	return reqAddr
}

func redactedVirtualAddr(kind, reqAddr string) string {
	_, port, err := net.SplitHostPort(reqAddr)
	if err != nil {
		port = "0"
	}
	host := virtualHostPrefix + "[redacted]"
	if kind != "" {
		host = virtualHostPrefix + kind + "_[redacted]"
	}
	return net.JoinHostPort(host, port)
}
