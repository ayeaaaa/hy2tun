package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"io"
	"net"
	"sync/atomic"
	"time"

	"hy2tun/internal/credentials"
	"hy2tun/internal/transport"
	"hy2tun/internal/version"
)

type sessionMeta struct {
	Kind              transport.Kind
	Principal         *transport.Principal
	Peer              *transport.TCPPeer
	CloseTransport    func() error
	HandshakeDeadline time.Time
	registered        func()
}

// The endpoint supplies TLS and lifetime context, not cached mode/credentials.
func (s *Server) tcpServerOptions(tlsConfig *tls.Config) transport.TCPServerOptions {
	cfg := s.currentConfig()
	return transport.TCPServerOptions{
		TLS: tlsConfig, Authenticate: s.authenticateTCP,
		Mode: cfg.Mode.Effective(), Version: version.Version,
		// Transport directions are client-oriented; config directions are server-oriented.
		Limits: transport.RateLimits{Up: uint64(cfg.Bandwidth.Down), Down: uint64(cfg.Bandwidth.Up)},
	}
}

func (s *Server) authenticateTCP(ctx context.Context, addr net.Addr, greeting transport.Greeting) (transport.Principal, error) {
	if err := ctx.Err(); err != nil {
		return transport.Principal{}, err
	}
	cfg := s.currentConfig()
	set := s.currentCredentials()
	if cfg == nil || !cfg.Mode.AllowsTCP() || greeting.ClientID == "" {
		return transport.Principal{}, transport.ErrAuthentication
	}
	generation, ok := set.AcceptControlToken(greeting.ControlToken)
	if !ok {
		return transport.Principal{}, transport.ErrAuthentication
	}
	if cfg.Auth.Type == "password" {
		// Bind both credentials to one retained generation, including partial rotations.
		match := func(password, token string) bool {
			return password != "" && token != "" && subtle.ConstantTimeCompare([]byte(password), []byte(greeting.Auth)) == 1 && subtle.ConstantTimeCompare([]byte(token), []byte(greeting.ControlToken)) == 1
		}
		switch {
		case match(set.Auth, set.ControlToken):
			generation = set.Generation
		case set.PreviousGeneration > 0 && match(set.PreviousAuth, set.PreviousControlToken):
			generation = set.PreviousGeneration
		default:
			return transport.Principal{}, transport.ErrAuthentication
		}
	} else {
		authenticator, err := newAuthenticator(&cfg.Auth, s.credentials)
		if err != nil {
			return transport.Principal{}, transport.ErrAuthentication
		}
		if ok, _ := authenticator.Authenticate(addr, greeting.Auth, 0); !ok {
			return transport.Principal{}, transport.ErrAuthentication
		}
	}
	if err := ctx.Err(); err != nil {
		return transport.Principal{}, err
	}
	return transport.Principal{ClientID: greeting.ClientID, Generation: generation, TokenDigest: sha256.Sum256([]byte(greeting.ControlToken))}, nil
}

// A root cannot promote its immutable authentication generation via Hello or
// a credential ack. In particular, rotating only the password may retain the token.
func tcpPrincipalValid(p transport.Principal, set *credentials.Set) bool {
	if set == nil {
		return false
	}
	if p.Generation == set.Generation && set.ControlToken != "" {
		return p.TokenDigest == sha256.Sum256([]byte(set.ControlToken))
	}
	return set.PreviousGeneration > 0 && p.Generation == set.PreviousGeneration && set.PreviousControlToken != "" && p.TokenDigest == sha256.Sum256([]byte(set.PreviousControlToken))
}

type tcpRegistrationDeadlineKey struct{}

// serveTCPConn owns an already-accepted raw connection. Production binding and
// endpoint workers belong to the startup layer; this entry is also usable by a
// private listener. TLS, greeting, and HelloAck share the same ten-second budget.
func (s *Server) serveTCPConn(ctx context.Context, conn net.Conn, tlsConfig *tls.Config) error {
	deadline := time.Now().Add(controlHandshakeTimeout)
	hctx, cancel := context.WithDeadline(ctx, deadline)
	peer, err := transport.AcceptTCP(hctx, conn, s.tcpServerOptions(tlsConfig))
	cancel()
	if err != nil {
		return err
	}
	return s.serveTCPPeer(context.WithValue(ctx, tcpRegistrationDeadlineKey{}, deadline), peer)
}

func (s *Server) serveTCPPeer(ctx context.Context, peer *transport.TCPPeer) error {
	if peer == nil {
		return transport.ErrProtocol
	}
	// Pending business operations (notably proxy dials) belong to this root,
	// rather than the longer-lived endpoint worker context.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer peer.Close()
	stop := context.AfterFunc(ctx, func() { _ = peer.Close() })
	defer stop()
	deadline, ok := ctx.Value(tcpRegistrationDeadlineKey{}).(time.Time)
	if !ok {
		deadline = time.Now().Add(controlHandshakeTimeout)
	}
	var registered atomic.Bool
	timer := time.AfterFunc(max(time.Until(deadline), 0), func() {
		if !registered.Load() {
			_ = peer.Close()
		}
	})
	defer timer.Stop()
	controlSeen := false
	for {
		header, conn, err := peer.Accept(ctx)
		if err != nil {
			return err
		}
		if header.Kind == transport.StreamControl {
			if controlSeen || !s.currentConfig().Mode.AllowsTCP() || !tcpPrincipalValid(peer.Principal(), s.currentCredentials()) {
				_ = transport.WriteStreamAck(conn, transport.ErrAuthentication)
				_ = conn.Close()
				continue
			}
			controlSeen = true
			if err := transport.WriteStreamAck(conn, nil); err != nil {
				_ = conn.Close()
				return err
			}
			principal := peer.Principal()
			go s.control.serveWithMeta(conn, sessionMeta{Kind: transport.TCP, Principal: &principal, Peer: peer, CloseTransport: peer.Close, HandshakeDeadline: deadline, registered: func() { registered.Store(true) }})
			continue
		}
		owner := s.tcpOwner(peer)
		if owner == nil {
			_ = transport.WriteStreamAck(conn, transport.ErrAuthentication)
			_ = conn.Close()
			continue
		}
		go s.serveTCPStream(ctx, peer, owner, header, conn)
	}
}

func (s *Server) tcpOwner(peer *transport.TCPPeer) *clientSession {
	if !s.currentConfig().Mode.AllowsTCP() || !tcpPrincipalValid(peer.Principal(), s.currentCredentials()) {
		return nil
	}
	m := s.mappings
	m.mu.Lock()
	defer m.mu.Unlock()
	owner := m.sessions[peer.Principal().ClientID]
	if !m.liveTCPOwnerLocked(owner) || owner.tcpPeer != peer {
		return nil
	}
	return owner
}

func (m *mappingManager) liveTCPOwnerLocked(owner *clientSession) bool {
	return owner != nil && owner.transport == transport.TCP && owner.tcpPeer != nil && owner.online && owner.isReady() && m.sessions[owner.id] == owner
}

func (s *Server) serveTCPStream(ctx context.Context, peer *transport.TCPPeer, owner *clientSession, header transport.StreamHeader, conn net.Conn) {
	defer conn.Close()
	if s.tcpOwner(peer) != owner {
		_ = transport.WriteStreamAck(conn, transport.ErrAuthentication)
		return
	}
	switch header.Kind {
	case transport.StreamReverseTCP:
		public, err := s.data.takeFor(header.Token, owner)
		if err != nil {
			_ = transport.WriteStreamAck(conn, transport.ErrAuthentication)
			return
		}
		defer public.Close()
		if err := transport.WriteStreamAck(conn, nil); err != nil {
			return
		}
		relayTCP(conn, public)
	case transport.StreamUDP:
		rt, err := s.mappings.udpRuntimeFor(header.Token, owner)
		if err != nil {
			_ = transport.WriteStreamAck(conn, transport.ErrAuthentication)
			return
		}
		if err := transport.WriteStreamAck(conn, nil); err != nil {
			return
		}
		// Do not obscure the managed connection: the adapter inherits root/stream
		// cancellation and server rate ownership from this exact object.
		datagrams := transport.NewDatagramConn(conn)
		defer datagrams.Close()
		s.mappings.mu.Lock()
		valid := s.mappings.liveTCPOwnerLocked(owner) && rt.owner == owner && !rt.closed.Load()
		if valid {
			rt.trackDatagram(datagrams)
		}
		s.mappings.mu.Unlock()
		if !valid {
			return
		}
		defer rt.untrackDatagram(datagrams)
		// Serializing/draining the previous public reader belongs only to udpMu,
		// not the shared manager. Removal can close the socket and cancel this
		// child meanwhile; attachPublicUDPConn checks closed state before publish.
		public := s.attachPublicUDPConn(rt, datagrams)
		if public == nil {
			return
		}
		s.bridgeTCPUDP(rt, public, datagrams)
	case transport.StreamTCPProxy:
		if isVirtualHost(header.Target) {
			_ = transport.WriteStreamAck(conn, transport.ErrProtocol)
			return
		}
		public, err := outboundDialer.DialContext(ctx, "tcp", header.Target)
		if err != nil {
			_ = transport.WriteStreamAck(conn, transport.ErrProtocol)
			return
		}
		defer public.Close()
		if s.tcpOwner(peer) != owner {
			_ = transport.WriteStreamAck(conn, transport.ErrAuthentication)
			return
		}
		if err := transport.WriteStreamAck(conn, nil); err != nil {
			return
		}
		relayTCP(conn, public)
	default:
		_ = transport.WriteStreamAck(conn, transport.ErrProtocol)
	}
}

func relayTCP(a, b net.Conn) {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(a, b); _ = a.Close(); _ = b.Close(); close(done) }()
	_, _ = io.Copy(b, a)
	_ = a.Close()
	_ = b.Close()
	<-done
}
