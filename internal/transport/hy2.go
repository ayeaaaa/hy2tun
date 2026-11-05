package transport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"sync"

	hyclient "github.com/apernet/hysteria/core/v2/client"
)

type hy2Session struct {
	client       hyclient.Client
	controlToken string
	packets      *packetTracker
	closed       chan struct{}
	closeOnce    sync.Once
	closeErr     error
	pendingMu    sync.Mutex
	pendingUDP   map[*ownedHY2Datagram]struct{}
}

// WrapHY2 adapts existing clients and test factories without implementing TCP
// as a Hysteria client. The caller transfers ownership of client to the session.
func WrapHY2(client hyclient.Client, controlToken string) Session {
	return &hy2Session{client: client, controlToken: controlToken, closed: make(chan struct{})}
}

// DialHY2 makes the synchronous Hysteria factory cancellable. Packet sockets
// are tracked before the factory returns, so cancellation can abort handshake
// I/O immediately. An unbuffered handoff closes any late, undelivered client.
func DialHY2(ctx context.Context, opts DialOptions, factory HY2Factory) (Session, error) {
	session, _, err := DialHY2WithCompletion(ctx, opts, factory)
	return session, err
}

// DialHY2WithCompletion retains actual factory/late-root cleanup completion.
// Cancellation closes tracked sockets promptly, but is not a drain guarantee.
// A success receipt covers native handoff, not the delivered root's lifetime.
func DialHY2WithCompletion(ctx context.Context, opts DialOptions, factory HY2Factory) (Session, <-chan struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, completedOpen(), err
	}
	if opts.HY2 == nil {
		return nil, completedOpen(), &Error{Code: errConfig.Code, Cause: errors.New("HY2 configuration is required")}
	}
	if factory == nil {
		factory = hyclient.NewClient
	}
	cfg := *opts.HY2
	packets := &packetTracker{factory: cfg.ConnFactory}
	cfg.ConnFactory = packets
	type result struct {
		session *hy2Session
		err     error
	}
	results := make(chan result)
	completion := make(chan struct{})
	admitted := make(chan bool, 1)
	go func() {
		defer close(completion)
		client, _, err := factory(&cfg)
		session := WrapHY2(client, opts.ControlToken).(*hy2Session)
		session.packets = packets
		if err == nil && client == nil {
			err = &Error{Code: ErrProtocol.Code, Cause: errors.New("HY2 factory returned no client")}
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			_ = session.Close()
			session = nil
		}
		select {
		case results <- result{session: session, err: err}:
			if !<-admitted && session != nil {
				_ = session.Close()
			}
		case <-ctx.Done():
			if session != nil {
				_ = session.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		_ = packets.Close()
		return nil, completion, ctx.Err()
	case result := <-results:
		if err := ctx.Err(); err != nil {
			admitted <- false
			return nil, completion, errors.Join(result.err, err)
		}
		admitted <- true
		if result.err != nil {
			return nil, completion, result.err
		}
		return result.session, completion, nil
	}
}

func (s *hy2Session) Kind() Kind { return HY2 }

func (s *hy2Session) OpenControl(ctx context.Context) (net.Conn, error) {
	return s.openTCP(ctx, "_hy2tun_control_:0")
}

func (s *hy2Session) OpenControlWithCompletion(ctx context.Context) (net.Conn, <-chan struct{}, error) {
	return s.openTCPWithCompletion(ctx, "_hy2tun_control_:0")
}

func (s *hy2Session) OpenTCP(ctx context.Context, req TCPRequest) (net.Conn, error) {
	conn, _, err := s.OpenTCPWithCompletion(ctx, req)
	return conn, err
}

func (s *hy2Session) OpenTCPWithCompletion(ctx context.Context, req TCPRequest) (net.Conn, <-chan struct{}, error) {
	if (req.ReverseToken == "") == (req.Target == "") {
		return nil, completedOpen(), &Error{Code: ErrProtocol.Code, Cause: errors.New("TCP request requires exactly one token or target")}
	}
	if req.ReverseToken != "" {
		if !validHY2Token(req.ReverseToken) {
			return nil, completedOpen(), &Error{Code: ErrProtocol.Code, Cause: errors.New("invalid reverse TCP token")}
		}
		return s.openTCPWithCompletion(ctx, "_hy2tun_data_"+req.ReverseToken+":0")
	}
	return s.openTCPWithCompletion(ctx, req.Target)
}

func (s *hy2Session) openTCP(ctx context.Context, addr string) (net.Conn, error) {
	conn, _, err := s.openTCPWithCompletion(ctx, addr)
	return conn, err
}

func (s *hy2Session) openTCPWithCompletion(ctx context.Context, addr string) (net.Conn, <-chan struct{}, error) {
	conn, completion, err := s.openWithCompletion(ctx, func() (io.Closer, error) { return s.client.TCP(addr) })
	if err != nil {
		return nil, completion, err
	}
	return conn.(net.Conn), completion, nil
}

func (s *hy2Session) OpenUDP(ctx context.Context, token string) (DatagramConn, error) {
	conn, _, err := s.OpenUDPWithCompletion(ctx, token)
	return conn, err
}

func (s *hy2Session) OpenUDPWithCompletion(ctx context.Context, token string) (DatagramConn, <-chan struct{}, error) {
	if !validHY2Token(token) {
		return nil, completedOpen(), &Error{Code: ErrProtocol.Code, Cause: errors.New("invalid reverse UDP token")}
	}
	conn, completion, err := s.openWithCompletion(ctx, func() (io.Closer, error) {
		udp, err := s.client.UDP()
		if err != nil || udp == nil {
			return udp, err
		}
		owned := &ownedHY2Datagram{DatagramConn: udp}
		if err := s.trackUDP(ctx, owned); err != nil {
			return owned, err
		}
		defer s.untrackUDP(owned)
		// Preserve the existing one-byte reverse-mapping registration datagram.
		// Subsequent Send/Receive addresses are public peer addresses, unchanged.
		return owned, owned.Send([]byte{0}, "_hy2tun_udp_"+token+":0")
	})
	if err != nil {
		return nil, completion, err
	}
	return conn.(DatagramConn), completion, nil
}

func (s *hy2Session) Probe(ctx context.Context) (ProbeInfo, error) {
	info, _, err := s.ProbeWithCompletion(ctx)
	return info, err
}

func (s *hy2Session) ProbeWithCompletion(ctx context.Context) (ProbeInfo, <-chan struct{}, error) {
	if err := s.operationErr(ctx); err != nil {
		return ProbeInfo{}, completedOpen(), err
	}
	if s.controlToken == "" || len(s.controlToken) > greetingLimit {
		return ProbeInfo{}, completedOpen(), ErrProtocol
	}
	pctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	var info ProbeInfo
	// The complete exchange remains owned until handoff. A session Close also
	// cancels a child already returned by TCP(), even if upstream root Close
	// does not propagate to it. This virtual channel never sends Hello.
	_, completion, err := s.openWithCompletion(pctx, func() (io.Closer, error) {
		conn, err := s.client.TCP("_hy2tun_probe_:0")
		if err != nil {
			return conn, err
		}
		if conn == nil {
			return nil, ErrProtocol
		}
		cancelFinished := make(chan struct{})
		stopCancel := context.AfterFunc(pctx, func() {
			defer close(cancelFinished)
			_ = conn.Close()
		})
		watchStop := make(chan struct{})
		watchFinished := make(chan struct{})
		go func() {
			defer close(watchFinished)
			select {
			case <-s.closed:
				_ = conn.Close()
			case <-watchStop:
			}
		}()
		defer func() {
			// Stop only prevents a callback that has not started; it never
			// proves an already-running Close has finished. Join both owned
			// paths before this native operation can complete its receipt.
			// Fixed channels avoid registration-vs-WaitGroup Add/Wait races.
			stopped := stopCancel()
			close(watchStop)
			if !stopped {
				<-cancelFinished
			}
			<-watchFinished
			_ = conn.Close()
		}()
		deadline, _ := pctx.Deadline()
		_ = conn.SetDeadline(deadline)
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		text := hex.EncodeToString(nonce[:])
		if err := WriteProbeRequest(conn, ProbeRequest{Version: protocolVersion, Token: s.controlToken, Nonce: text}); err != nil {
			return nil, err
		}
		reply, err := ReadProbeReply(conn)
		if err != nil {
			return nil, err
		}
		if !reply.OK {
			if reply.Error == "unauthorized" {
				return nil, ErrAuthentication
			}
			return nil, ErrProtocol
		}
		if reply.Info.Nonce != text {
			return nil, ErrProtocol
		}
		info = reply.Info
		return conn, nil
	})
	if err != nil {
		return ProbeInfo{}, completion, err
	}
	return info, completion, nil
}

func (s *hy2Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.pendingMu.Lock()
		pending := s.pendingUDP
		s.pendingUDP = nil
		s.pendingMu.Unlock()
		// Own and close children before root shutdown: the public HY2 adapter
		// contract cannot rely on the root to release a blocked registration.
		for conn := range pending {
			s.closeErr = errors.Join(s.closeErr, conn.Close())
		}
		if s.client != nil {
			s.closeErr = errors.Join(s.closeErr, s.client.Close())
		}
		if s.packets != nil {
			s.closeErr = errors.Join(s.closeErr, s.packets.Close())
		}
	})
	return s.closeErr
}

func (s *hy2Session) trackUDP(ctx context.Context, conn *ownedHY2Datagram) error {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if err := s.operationErr(ctx); err != nil {
		return err
	}
	if s.pendingUDP == nil {
		s.pendingUDP = make(map[*ownedHY2Datagram]struct{})
	}
	s.pendingUDP[conn] = struct{}{}
	return nil
}

func (s *hy2Session) untrackUDP(conn *ownedHY2Datagram) {
	s.pendingMu.Lock()
	delete(s.pendingUDP, conn)
	s.pendingMu.Unlock()
}

// Shutdown and the operation-result cleanup can race to close the same child.
// Close once makes the ownership handoff safe without assuming fake/upstream
// datagram Close supports concurrent or repeated calls.
type ownedHY2Datagram struct {
	DatagramConn
	closeOnce sync.Once
	closeErr  error
}

func (d *ownedHY2Datagram) Close() error {
	d.closeOnce.Do(func() { d.closeErr = d.DatagramConn.Close() })
	return d.closeErr
}

func (s *hy2Session) operationErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	if s.client == nil {
		return &Error{Code: ErrProtocol.Code, Cause: errors.New("HY2 client is nil")}
	}
	return nil
}

func completedOpen() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (s *hy2Session) openWithCompletion(ctx context.Context, operation func() (io.Closer, error)) (io.Closer, <-chan struct{}, error) {
	if err := s.operationErr(ctx); err != nil {
		return nil, completedOpen(), err
	}
	type result struct {
		conn io.Closer
		err  error
	}
	results := make(chan result)
	completion := make(chan struct{})
	admitted := make(chan bool, 1)
	go func() {
		// This receipt survives prompt open cancellation and root Close. It
		// covers the native result and all adapter-owned child cleanup.
		defer close(completion)
		conn, err := operation()
		if err == nil && conn == nil {
			err = &Error{Code: ErrProtocol.Code, Cause: errors.New("HY2 returned no connection")}
		}
		if err == nil {
			err = s.operationErr(ctx)
		}
		if err != nil && conn != nil {
			_ = conn.Close()
			conn = nil
		}
		select {
		case results <- result{conn: conn, err: err}:
			if !<-admitted && conn != nil {
				_ = conn.Close()
			}
		case <-ctx.Done():
			if conn != nil {
				_ = conn.Close()
			}
		case <-s.closed:
			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		_ = s.Close()
		return nil, completion, ctx.Err()
	case <-s.closed:
		return nil, completion, net.ErrClosed
	case result := <-results:
		if err := s.operationErr(ctx); err != nil {
			// The native worker owns rejected handoff cleanup; prompt
			// caller return is not evidence that this child has finished Close.
			admitted <- false
			if ctx.Err() != nil {
				_ = s.Close()
			}
			return nil, completion, err
		}
		admitted <- true
		return result.conn, completion, result.err
	}
}

func validHY2Token(token string) bool {
	if len(token) != 32 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// packetTracker implements the existing Hysteria ConnFactory interface. New
// racing cancellation closes late sockets rather than handing them to HY2.
type packetTracker struct {
	factory hyclient.ConnFactory
	mu      sync.Mutex
	closed  bool
	conns   []net.PacketConn
}

func (p *packetTracker) New(addr net.Addr) (net.PacketConn, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	var conn net.PacketConn
	var err error
	if p.factory != nil {
		conn, err = p.factory.New(addr)
	} else {
		conn, err = net.ListenUDP("udp", nil)
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, err
	}
	if conn == nil {
		return nil, &Error{Code: ErrProtocol.Code, Cause: errors.New("HY2 packet factory returned no connection")}
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	p.conns = append(p.conns, conn)
	p.mu.Unlock()
	// Preserve the concrete socket's optional interfaces (QUIC OOB, syscall,
	// buffer sizing), including those supplied by custom packet factories.
	return conn, nil
}

func (p *packetTracker) Close() error {
	p.mu.Lock()
	p.closed = true
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	var err error
	for _, conn := range conns {
		// HY2 also owns this socket and may already have closed it.
		if closeErr := conn.Close(); !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}
