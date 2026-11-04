package transport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

func TestTCPGreetingTLSAndAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, auth, token string
		version           int
		maxTLS            uint16
		want              error
	}{
		{"success", "auth", "secret", 1, tls.VersionTLS13, nil},
		{"TLS12", "auth", "secret", 1, tls.VersionTLS12, errTLS},
		{"auth", "wrong", "secret", 1, tls.VersionTLS13, ErrAuthentication},
		{"token", "auth", "wrong", 1, tls.VersionTLS13, ErrAuthentication},
		{"version", "auth", "secret", 2, tls.VersionTLS13, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, err := testutil.SelfSignedCert("tunnel.test")
			require.NoError(t, err)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var authCalled atomic.Bool
			done := make(chan error, 1)
			go func() {
				c, e := listener.Accept()
				if e != nil {
					done <- e
					return
				}
				p, e := AcceptTCP(ctx, c, TCPServerOptions{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Authenticate: func(ctx context.Context, addr net.Addr, g Greeting) (Principal, error) {
					authCalled.Store(true)
					return testTCPAuth(ctx, addr, g)
				}, Mode: config.ModeAuto, Version: "test"})
				if p != nil {
					defer p.Close()
					if p.Principal().ClientID != "client" || p.Principal().TokenDigest != sha256.Sum256([]byte("secret")) {
						e = errors.New("lost principal")
					}
				}
				done <- e
			}()
			c, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			require.NoError(t, err)
			defer c.Close()
			client := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tc.maxTLS})
			err = client.HandshakeContext(ctx)
			if tc.maxTLS == tls.VersionTLS13 {
				require.NoError(t, err)
				require.NoError(t, writeJSONFrame(client, Greeting{Magic: "hy2tun-tcp", Version: tc.version, Auth: tc.auth, ControlToken: tc.token, ClientID: "client"}, 65536))
				var ack GreetingAck
				require.NoError(t, readJSONFrame(client, &ack, 65536))
				if tc.want == nil {
					require.True(t, ack.OK)
					require.Equal(t, config.ModeAuto, ack.Mode)
				} else {
					require.False(t, ack.OK)
					require.Contains(t, []string{"unauthorized", "unsupported_protocol", "bad_message"}, ack.Error)
				}
			} else {
				require.Error(t, err)
			}
			err = waitError(t, done)
			require.Equal(t, tc.name != "version" && tc.name != "TLS12", authCalled.Load())
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
				require.False(t, FallbackAllowed(err))
			}
		})
	}
}

func TestDialTCPCertificatePolicy(t *testing.T) {
	cert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})))
	sum := sha256.Sum256(cert.Certificate[0])
	pin := hex.EncodeToString(sum[:])
	for _, tc := range []struct {
		name, sni, pin             string
		trusted, insecure, success bool
	}{
		{"verified", "tunnel.test", pin, true, false, true},
		{"pin_does_not_replace_CA", "tunnel.test", pin, false, false, false},
		{"pin_does_not_replace_SNI", "wrong.test", pin, true, false, false},
		{"insecure_still_pins", "tunnel.test", "0000000000000000000000000000000000000000000000000000000000000000", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, e := ClientTLS(config.ClientTLSConfig{SNI: tc.sni, PinSHA256: tc.pin, Insecure: tc.insecure}, "unused:1")
			require.NoError(t, e)
			if tc.trusted {
				cfg.RootCAs = pool
			}
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, e)
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				c, e := listener.Accept()
				if e != nil {
					done <- e
					return
				}
				p, e := AcceptTCP(ctx, c, TCPServerOptions{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Authenticate: testTCPAuth, Mode: config.ModeAuto})
				if p != nil {
					_ = p.Close()
				}
				done <- e
			}()
			s, e := DialTCP(ctx, DialOptions{TCPAddress: listener.Addr().String(), TLS: cfg, Auth: "auth", ControlToken: "secret", ClientID: "client"})
			if s != nil {
				_ = s.Close()
			}
			if tc.success {
				require.NoError(t, e)
				require.NoError(t, waitError(t, done))
			} else {
				require.Error(t, e)
				require.False(t, FallbackAllowed(e))
				require.Error(t, waitError(t, done))
			}
			require.Equal(t, tc.insecure, cfg.InsecureSkipVerify)
			require.Equal(t, tc.sni, cfg.ServerName)
		})
	}
}

func TestTCPGreetingAuthenticationCancellation(t *testing.T) {
	cert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			done <- e
			return
		}
		p, e := AcceptTCP(ctx, c, TCPServerOptions{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Authenticate: func(ctx context.Context, _ net.Addr, _ Greeting) (Principal, error) {
			close(started)
			<-ctx.Done()
			return Principal{}, ctx.Err()
		}, Mode: config.ModeAuto})
		if p != nil {
			_ = p.Close()
		}
		done <- e
	}()
	c, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	client := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	hctx, finish := context.WithTimeout(ctx, time.Second)
	defer finish()
	require.NoError(t, client.HandshakeContext(hctx))
	require.NoError(t, writeJSONFrame(client, Greeting{Magic: "hy2tun-tcp", Version: 1, Auth: "auth", ControlToken: "secret", ClientID: "client"}, 65536))
	waitSignal(t, started)
	require.NoError(t, client.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
	_, err = client.Read(make([]byte, 1))
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout(), "no mux or ack bytes before root authentication")
	cancel()
	require.ErrorIs(t, waitError(t, done), context.Canceled)
}

func TestTCPRemoteRootLossAllowsFallbackButLocalCloseDoesNot(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	accepted := make(chan net.Conn, 1)
	done := make(chan error, 1)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			e = WriteStreamAck(c, nil)
			accepted <- c
		}
		done <- e
	}()
	control, err := s.OpenControl(ctx)
	require.NoError(t, err)
	defer control.Close()
	require.NoError(t, waitError(t, done))
	serverControl := <-accepted
	defer serverControl.Close()
	require.NoError(t, peer.Close())
	require.Eventually(t, func() bool { return s.(*tcpSession).mux.IsClosed() }, time.Second, time.Millisecond)
	_, err = control.Read(make([]byte, 1))
	require.Error(t, err)
	require.True(t, FallbackAllowed(err))
	_, err = s.OpenControl(ctx)
	require.Error(t, err)
	require.True(t, FallbackAllowed(err), "remote TCP root loss must retain the existing lost-session classification")
	require.NoError(t, s.Close())
	_, err = s.OpenControl(ctx)
	require.ErrorIs(t, err, net.ErrClosed)
	require.False(t, FallbackAllowed(err), "explicit local shutdown is not a network failure")
}

func TestTCPMalformedYamuxRetainsProtocolClass(t *testing.T) {
	cert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type result struct {
		p   *TCPPeer
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			done <- result{err: e}
			return
		}
		p, e := AcceptTCP(ctx, c, TCPServerOptions{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Authenticate: testTCPAuth, Mode: config.ModeAuto})
		done <- result{p, e}
	}()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	client := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	require.NoError(t, client.HandshakeContext(ctx))
	require.NoError(t, writeJSONFrame(client, Greeting{Magic: "hy2tun-tcp", Version: 1, Auth: "auth", ControlToken: "secret", ClientID: "client"}, 65536))
	var ack GreetingAck
	require.NoError(t, readJSONFrame(client, &ack, 65536))
	require.True(t, ack.OK)
	r := <-done
	require.NoError(t, r.err)
	defer r.p.Close()
	_, err = client.Write([]byte{99, 2, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return r.p.session.mux.IsClosed() }, time.Second, time.Millisecond)
	_, _, err = r.p.Accept(ctx)
	require.ErrorIs(t, err, ErrProtocol)
	require.False(t, FallbackAllowed(err))
}

func testTCPAuth(ctx context.Context, addr net.Addr, g Greeting) (Principal, error) {
	if g.Auth != "auth" || g.ControlToken != "secret" {
		return Principal{}, errors.New("sensitive auth/token must not be echoed")
	}
	return Principal{ClientID: g.ClientID, Generation: 7, TokenDigest: sha256.Sum256([]byte(g.ControlToken))}, nil
}

func TestTCPGreetingCannotBypassTLS13WithConfigCallback(t *testing.T) {
	cert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	selected := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	serverCfg := &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return selected, nil }}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			done <- e
			return
		}
		p, e := AcceptTCP(ctx, c, TCPServerOptions{TLS: serverCfg, Authenticate: testTCPAuth, Mode: config.ModeAuto})
		if p != nil {
			_ = p.Close()
		}
		done <- e
	}()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	err = tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}).HandshakeContext(ctx)
	_ = c.Close()
	serverErr := waitError(t, done)
	require.Error(t, err, "a selected TLS config must not bypass the TCP TLS 1.3 floor")
	require.ErrorIs(t, serverErr, errTLS)
	require.Equal(t, uint16(tls.VersionTLS12), selected.MinVersion, "do not mutate caller configs")
}

func tcpPair(t *testing.T, limits RateLimits, up, down uint64) (Session, *TCPPeer, context.Context) {
	t.Helper()
	cert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	type result struct {
		peer *TCPPeer
		err  error
	}
	done := make(chan result, 1)
	go func() {
		c, e := listener.Accept()
		if e != nil {
			done <- result{err: e}
			return
		}
		p, e := AcceptTCP(ctx, c, TCPServerOptions{TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Authenticate: testTCPAuth, Mode: config.ModeAuto, Version: "test", Limits: limits})
		done <- result{p, e}
	}()
	s, err := DialTCP(ctx, DialOptions{TCPAddress: listener.Addr().String(), TLS: &tls.Config{InsecureSkipVerify: true}, Auth: "auth", ControlToken: "secret", ClientID: "client", Up: up, Down: down})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	r := <-done
	require.NoError(t, r.err)
	t.Cleanup(func() { _ = r.peer.Close() })
	return s, r.peer, ctx
}
