package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

// benchTCPPair builds one real TLS/yamux session over loopback for transport
// benchmarks. It mirrors tcpPair without requiring *testing.T.
func benchTCPPair(b *testing.B) (Session, *TCPPeer, context.Context, func()) {
	b.Helper()
	cert, err := testutil.SelfSignedCert("tunnel.test")
	if err != nil {
		b.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		peer *TCPPeer
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- result{err: err}
			return
		}
		peer, err := AcceptTCP(ctx, conn, TCPServerOptions{
			TLS:          &tls.Config{Certificates: []tls.Certificate{cert}},
			Authenticate: testTCPAuth,
			Mode:         config.ModeAuto,
			Version:      "bench",
		})
		done <- result{peer: peer, err: err}
	}()
	session, err := DialTCP(ctx, DialOptions{
		TCPAddress:   listener.Addr().String(),
		TLS:          &tls.Config{InsecureSkipVerify: true},
		Auth:         "auth",
		ControlToken: "secret",
		ClientID:     "bench",
	})
	if err != nil {
		cancel()
		_ = listener.Close()
		b.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		cancel()
		_ = session.Close()
		_ = listener.Close()
		b.Fatal(r.err)
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			_ = session.Close()
			if r.peer != nil {
				_ = r.peer.Close()
			}
			_ = listener.Close()
		})
	}
	return session, r.peer, ctx, cleanup
}

// BenchmarkTCPMultiplex measures parallel stream open/echo/close throughput and
// allocations through the real TLS/yamux session and application ack path.
func BenchmarkTCPMultiplex(b *testing.B) {
	session, peer, ctx, cleanup := benchTCPPair(b)
	defer cleanup()

	var streams sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			header, conn, err := peer.Accept(ctx)
			if err != nil {
				return
			}
			if header.Kind != StreamTCPProxy {
				_ = conn.Close()
				continue
			}
			if err := WriteStreamAck(conn, nil); err != nil {
				_ = conn.Close()
				continue
			}
			streams.Add(1)
			go func() {
				defer streams.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	payload := make([]byte, 1024)
	echo := make([]byte, len(payload))
	b.SetBytes(int64(len(payload) * 2))
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			conn, err := session.OpenTCP(ctx, TCPRequest{Target: "bench.invalid:80"})
			if err != nil {
				b.Error(err)
				return
			}
			if _, err := conn.Write(payload); err != nil {
				b.Error(err)
				_ = conn.Close()
				return
			}
			if _, err := io.ReadFull(conn, echo); err != nil {
				b.Error(err)
				_ = conn.Close()
				return
			}
			if err := conn.Close(); err != nil {
				b.Error(err)
				return
			}
		}
	})

	b.StopTimer()
	cleanup()
	select {
	case <-acceptDone:
	case <-time.After(5 * time.Second):
		b.Error("TCP accept loop did not stop")
	}
	streams.Wait()
}

// BenchmarkUDPFrame measures the bounded TCP datagram adapter's send/receive
// frame path, including queueing and frame encoding/decoding.
func BenchmarkUDPFrame(b *testing.B) {
	for _, size := range []int{64, 512, 4096} {
		b.Run(fmt.Sprintf("payload-%d", size), func(b *testing.B) {
			clientConn, serverConn := net.Pipe()
			sender := NewDatagramConn(clientConn)
			receiver := NewDatagramConn(serverConn)
			defer func() { _ = sender.Close() }()
			defer func() { _ = receiver.Close() }()

			payload := make([]byte, size)
			const addr = "127.0.0.1:12345"
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := sender.Send(payload, addr); err != nil {
					b.Fatal(err)
				}
				got, gotAddr, err := receiver.Receive()
				if err != nil {
					b.Fatal(err)
				}
				if len(got) != size || gotAddr != addr {
					b.Fatalf("frame mismatch: got %d bytes addr %q", len(got), gotAddr)
				}
			}
		})
	}
}
