package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"hy2tun/internal/view"
)

const (
	addressFamilyIPv4 = "ipv4"
	addressFamilyIPv6 = "ipv6"
)

// mappingListenerSet owns every public socket for one mapping. TCP and UDP
// mappings use only one of the fields, but both are closed together so a
// partial bind can never leak the family that did succeed.
type mappingListenerSet struct {
	TCP []net.Listener
	UDP *multiUDPConn
	// UnavailableFamilies lists the families ("ipv4"/"ipv6") that could not be
	// bound on the latest attempt. It is runtime diagnostics, never an OS
	// error string.
	UnavailableFamilies []string

	familyErrors map[string]error
	closeOnce    sync.Once
	closeErr     error
}

// Close releases every socket owned by the set. It is idempotent.
func (s *mappingListenerSet) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var errs []error
		for _, listener := range s.TCP {
			if listener == nil {
				continue
			}
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		if s.UDP != nil {
			if err := s.UDP.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// Port returns the assigned port from the first successfully bound socket.
func (s *mappingListenerSet) Port() int {
	if s == nil {
		return 0
	}
	for _, listener := range s.TCP {
		if listener == nil {
			continue
		}
		if port := addrPort(listener.Addr()); port > 0 {
			return port
		}
	}
	if s.UDP != nil {
		return addrPort(s.UDP.LocalAddr())
	}
	return 0
}

// Warnings returns a copy of the unavailable family codes.
func (s *mappingListenerSet) Warnings() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.UnavailableFamilies...)
}

type mappingBindFamily struct {
	family string
	host   string
	v6only bool
}

// mappingBindFamilies selects the addresses for an exposure. Local always uses
// both loopback addresses and ignores listen_host. Public uses both wildcards
// when listen_host is empty or an explicit wildcard, and exactly one address
// when the operator selected a specific address.
func mappingBindFamilies(exposure view.Exposure, bindHost string) []mappingBindFamily {
	if exposure == view.ExposureLocal {
		return []mappingBindFamily{
			{family: addressFamilyIPv4, host: "127.0.0.1"},
			{family: addressFamilyIPv6, host: "::1"},
		}
	}

	host := strings.TrimSpace(bindHost)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	wildcard := []mappingBindFamily{
		{family: addressFamilyIPv4, host: "0.0.0.0"},
		{family: addressFamilyIPv6, host: "::", v6only: true},
	}
	if host == "" {
		return wildcard
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if addr.IsUnspecified() {
			// Explicit 0.0.0.0/:: is the operator asking for the wildcard,
			// not a request for single-family service.
			return wildcard
		}
		if addr.Is4() {
			return []mappingBindFamily{{family: addressFamilyIPv4, host: addr.String()}}
		}
		return []mappingBindFamily{{family: addressFamilyIPv6, host: addr.String(), v6only: true}}
	}

	// A hostname is an explicit single-address choice. Resolution and family
	// selection are left to the standard library; there is no second family to
	// warn about.
	return []mappingBindFamily{{host: host}}
}

// bindMappingListeners binds the family-aware socket set for one mapping type
// on port. A port of zero lets the first successful family choose the port and
// the remaining family reuse it. One failed family is reported in
// UnavailableFamilies; both failing is an error.
func bindMappingListeners(exposure view.Exposure, bindHost, mappingType string, port int) (*mappingListenerSet, error) {
	if mappingType != "tcp" && mappingType != "udp" {
		return nil, fmt.Errorf("bind mapping listeners: unsupported mapping type %q", mappingType)
	}

	families := mappingBindFamilies(exposure, bindHost)
	set := &mappingListenerSet{familyErrors: make(map[string]error)}
	assigned := port
	var udpConns []*net.UDPConn
	var errs []error

	for _, family := range families {
		listenPort := assigned
		addr := net.JoinHostPort(family.host, strconv.Itoa(listenPort))
		if mappingType == "tcp" {
			listener, err := listenTCPFamily(family, addr)
			if err != nil {
				set.familyErrors[family.family] = err
				errs = append(errs, fmt.Errorf("%s %s: %w", familyLabel(family), addr, err))
				continue
			}
			set.TCP = append(set.TCP, listener)
			if assigned == 0 {
				assigned = addrPort(listener.Addr())
			}
			continue
		}

		conn, err := listenUDPFamily(family, addr)
		if err != nil {
			set.familyErrors[family.family] = err
			errs = append(errs, fmt.Errorf("%s %s: %w", familyLabel(family), addr, err))
			continue
		}
		udpConns = append(udpConns, conn)
		if assigned == 0 {
			assigned = addrPort(conn.LocalAddr())
		}
	}

	if mappingType == "udp" && len(udpConns) > 0 {
		set.UDP = newMultiUDPConn(udpConns...)
	}
	if len(set.TCP) == 0 && set.UDP == nil {
		return nil, fmt.Errorf("bind %s mapping on port %d: %w", mappingType, port, errors.Join(errs...))
	}

	if len(families) > 1 {
		for _, family := range families {
			if _, failed := set.familyErrors[family.family]; failed {
				set.UnavailableFamilies = append(set.UnavailableFamilies, family.family)
			}
		}
	}
	return set, nil
}

func familyLabel(family mappingBindFamily) string {
	if family.family == "" {
		return "address"
	}
	return family.family
}

func listenTCPFamily(family mappingBindFamily, addr string) (net.Listener, error) {
	var config net.ListenConfig
	if family.v6only {
		config.Control = setIPv6Only
	}
	network := "tcp"
	switch family.family {
	case addressFamilyIPv4:
		network = "tcp4"
	case addressFamilyIPv6:
		network = "tcp6"
	}
	return config.Listen(context.Background(), network, addr)
}

func listenUDPFamily(family mappingBindFamily, addr string) (*net.UDPConn, error) {
	var config net.ListenConfig
	if family.v6only {
		config.Control = setIPv6Only
	}
	network := "udp"
	switch family.family {
	case addressFamilyIPv4:
		network = "udp4"
	case addressFamilyIPv6:
		network = "udp6"
	}
	packetConn, err := config.ListenPacket(context.Background(), network, addr)
	if err != nil {
		return nil, err
	}
	conn, ok := packetConn.(*net.UDPConn)
	if !ok {
		_ = packetConn.Close()
		return nil, fmt.Errorf("unexpected %s packet connection %T", network, packetConn)
	}
	return conn, nil
}

// setIPv6Only prevents an IPv6 wildcard socket from also claiming the IPv4
// wildcard port, so the two family sockets can coexist on one assigned port.
func setIPv6Only(_, _ string, raw syscall.RawConn) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

// udpPacketConn is the subset of *net.UDPConn used by publicUDPConn and the
// multi-socket UDP fan-in. publicUDPConn never closes it directly.
type udpPacketConn interface {
	ReadFrom([]byte) (int, net.Addr, error)
	WriteToUDPAddrPort([]byte, netip.AddrPort) (int, error)
	SetReadDeadline(time.Time) error
	LocalAddr() net.Addr
	Close() error
}

type familyUDPConn struct {
	family string
	conn   *net.UDPConn
}

type udpReadResult struct {
	data []byte
	addr net.Addr
	err  error
}

// multiUDPConn fans datagrams from the IPv4 and IPv6 sockets into one
// ReadFrom stream. It chooses the outgoing socket by destination family so a
// reply always leaves through the family that received the request.
type multiUDPConn struct {
	conns []familyUDPConn
	reads chan udpReadResult
	done  chan struct{}

	closeOnce sync.Once
	closeErr  error
	wg        sync.WaitGroup
}

func newMultiUDPConn(conns ...*net.UDPConn) *multiUDPConn {
	c := &multiUDPConn{
		reads: make(chan udpReadResult),
		done:  make(chan struct{}),
	}
	for _, conn := range conns {
		if conn == nil {
			continue
		}
		family := addressFamilyIPv4
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok && addr.IP != nil && addr.IP.To4() == nil {
			family = addressFamilyIPv6
		}
		c.conns = append(c.conns, familyUDPConn{family: family, conn: conn})
	}
	for _, conn := range c.conns {
		c.wg.Add(1)
		go c.readLoop(conn.conn)
	}
	return c
}

func (c *multiUDPConn) readLoop(conn *net.UDPConn) {
	defer c.wg.Done()

	buf := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case c.reads <- udpReadResult{err: err}:
			case <-c.done:
				return
			}
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case c.reads <- udpReadResult{data: data, addr: addr}:
		case <-c.done:
			return
		}
	}
}

func (c *multiUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if c == nil {
		return 0, nil, net.ErrClosed
	}
	select {
	case <-c.done:
		return 0, nil, net.ErrClosed
	case result := <-c.reads:
		if result.err != nil {
			return 0, nil, result.err
		}
		return copy(b, result.data), result.addr, nil
	}
}

func (c *multiUDPConn) WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error) {
	if c == nil {
		return 0, net.ErrClosed
	}
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}

	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	family := addressFamilyIPv4
	if addr.Addr().Is6() {
		family = addressFamilyIPv6
	}
	for _, conn := range c.conns {
		if conn.family == family {
			return conn.conn.WriteToUDPAddrPort(b, addr)
		}
	}
	return 0, fmt.Errorf("mapping has no %s UDP socket", family)
}

func (c *multiUDPConn) SetReadDeadline(deadline time.Time) error {
	if c == nil {
		return net.ErrClosed
	}
	var errs []error
	for _, conn := range c.conns {
		if err := conn.conn.SetReadDeadline(deadline); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *multiUDPConn) LocalAddr() net.Addr {
	if c == nil || len(c.conns) == 0 {
		return nil
	}
	return c.conns[0].conn.LocalAddr()
}

func (c *multiUDPConn) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		close(c.done)
		var errs []error
		for _, conn := range c.conns {
			if err := conn.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		c.wg.Wait()
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}
