package testutil

import (
	"io"
	"net"
	"sync"
	"testing"
)

// StartTCPEcho starts a TCP echo server on a loopback ephemeral port. It
// returns the listener address and an idempotent stop function that closes the
// listener and all active connections, then waits for the echo goroutines.
func StartTCPEcho(t *testing.T) (addr string, stop func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start TCP echo listener: %v", err)
	}

	var (
		mu     sync.Mutex
		conns  = make(map[net.Conn]struct{})
		closed bool
		wg     sync.WaitGroup
		once   sync.Once
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				continue
			}
			conns[conn] = struct{}{}
			mu.Unlock()

			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(conns, conn)
					mu.Unlock()
				}()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	stop = func() {
		once.Do(func() {
			_ = ln.Close()
			mu.Lock()
			closed = true
			for conn := range conns {
				_ = conn.Close()
			}
			mu.Unlock()
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	return ln.Addr().String(), stop
}

// StartUDPEcho starts a UDP echo server on a loopback ephemeral port. It
// returns the listener address and an idempotent stop function that closes the
// socket and waits for the echo goroutine.
func StartUDPEcho(t *testing.T) (addr string, stop func()) {
	t.Helper()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("start UDP echo listener: %v", err)
	}

	var (
		wg   sync.WaitGroup
		once sync.Once
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := conn.WriteToUDP(buf[:n], from); err != nil {
				// A failed reply must not stop the echo server for other
				// senders; the read loop notices when the socket is closed.
				continue
			}
		}
	}()

	stop = func() {
		once.Do(func() {
			_ = conn.Close()
			wg.Wait()
		})
	}
	t.Cleanup(stop)
	return conn.LocalAddr().String(), stop
}
