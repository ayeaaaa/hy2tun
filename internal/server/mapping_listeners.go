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

	// multiUDPReadBufferSize matches the largest UDP payload so a caller with a
	// large buffer never sees a datagram truncated by the fan-in.
	multiUDPReadBufferSize = 65535
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

type mappingBindGroup struct {
	family      string
	hosts       []string
	v6only      bool
	warnMissing bool
}

// mappingBindGroups selects the addresses for an exposure. Local always uses
// both loopback addresses and ignores listen_host. Public uses both wildcards
// when listen_host is empty or an explicit wildcard, exactly one address when
// the operator selected a specific address, and one bindable address per
// resolved family for a hostname. Empty exposure is the legacy public default;
// any other invalid exposure fails closed.
func mappingBindGroups(exposure view.Exposure, bindHost string) ([]mappingBindGroup, error) {
	if exposure != "" && !exposure.Valid() {
		return nil, fmt.Errorf("invalid exposure %q", exposure)
	}
	if exposure == view.ExposureLocal {
		return []mappingBindGroup{
			{family: addressFamilyIPv4, hosts: []string{"127.0.0.1"}, warnMissing: true},
			{family: addressFamilyIPv6, hosts: []string{"::1"}, warnMissing: true},
		}, nil
	}

	host := strings.TrimSpace(bindHost)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	wildcard := []mappingBindGroup{
		{family: addressFamilyIPv4, hosts: []string{"0.0.0.0"}, warnMissing: true},
		{family: addressFamilyIPv6, hosts: []string{"::"}, v6only: true, warnMissing: true},
	}
	if host == "" {
		return wildcard, nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		if addr.IsUnspecified() {
			// Explicit 0.0.0.0/:: is the operator asking for the wildcard,
			// not a request for single-family service.
			return wildcard, nil
		}
		if addr.Is4() {
			return []mappingBindGroup{{family: addressFamilyIPv4, hosts: []string{addr.String()}}}, nil
		}
		return []mappingBindGroup{{family: addressFamilyIPv6, hosts: []string{addr.String()}, v6only: true}}, nil
	}

	// A hostname is not an explicit single-address choice. Resolve it and try
	// every address of each family until one binds; a family with no address
	// or no bindable address is reported as unavailable, never silently
	// dropped.
	addrs, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve listen host %q: %w", host, err)
	}
	var ipv4Hosts, ipv6Hosts []string
	seen := make(map[netip.Addr]struct{}, len(addrs))
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !addr.IsValid() {
			continue
		}
		if _, duplicate := seen[addr]; duplicate {
			continue
		}
		seen[addr] = struct{}{}
		if addr.Is4() {
			ipv4Hosts = append(ipv4Hosts, addr.String())
			continue
		}
		ipv6Hosts = append(ipv6Hosts, addr.String())
	}
	if len(ipv4Hosts) == 0 && len(ipv6Hosts) == 0 {
		return nil, fmt.Errorf("resolve listen host %q: no addresses", host)
	}
	return []mappingBindGroup{
		{family: addressFamilyIPv4, hosts: ipv4Hosts, warnMissing: true},
		{family: addressFamilyIPv6, hosts: ipv6Hosts, v6only: true, warnMissing: true},
	}, nil
}

// bindMappingListeners binds the family-aware socket set for one mapping type
// on port. A port of zero lets the first successful family choose the port and
// the remaining family reuse it. One failed family is reported in
// UnavailableFamilies; both failing is an error.
func bindMappingListeners(exposure view.Exposure, bindHost, mappingType string, port int) (*mappingListenerSet, error) {
	if mappingType != "tcp" && mappingType != "udp" {
		return nil, fmt.Errorf("bind mapping listeners: unsupported mapping type %q", mappingType)
	}

	families, err := mappingBindGroups(exposure, bindHost)
	if err != nil {
		return nil, err
	}
	set := &mappingListenerSet{familyErrors: make(map[string]error)}
	assigned := port
	var udpConns []*net.UDPConn
	var errs []error

	for _, family := range families {
		if len(family.hosts) == 0 {
			set.familyErrors[family.family] = fmt.Errorf("no %s address", family.family)
			errs = append(errs, fmt.Errorf("%s: no address for listen host", family.family))
			continue
		}

		bound := false
		var lastErr error
		for _, host := range family.hosts {
			listenPort := assigned
			addr := net.JoinHostPort(host, strconv.Itoa(listenPort))
			if mappingType == "tcp" {
				listener, err := listenTCPFamily(family.family, family.v6only, addr)
				if err != nil {
					lastErr = err
					continue
				}
				set.TCP = append(set.TCP, listener)
				if assigned == 0 {
					assigned = addrPort(listener.Addr())
				}
				bound = true
				break
			}

			conn, err := listenUDPFamily(family.family, family.v6only, addr)
			if err != nil {
				lastErr = err
				continue
			}
			udpConns = append(udpConns, conn)
			if assigned == 0 {
				assigned = addrPort(conn.LocalAddr())
			}
			bound = true
			break
		}
		if bound {
			continue
		}
		set.familyErrors[family.family] = lastErr
		errs = append(errs, fmt.Errorf("%s %s: %w", familyLabel(family.family), family.hosts[0], lastErr))
	}

	if mappingType == "udp" && len(udpConns) > 0 {
		set.UDP = newMultiUDPConn(udpConns...)
	}
	if len(set.TCP) == 0 && set.UDP == nil {
		return nil, fmt.Errorf("bind %s mapping on port %d: %w", mappingType, port, errors.Join(errs...))
	}

	for _, family := range families {
		if !family.warnMissing {
			continue
		}
		if _, failed := set.familyErrors[family.family]; failed {
			set.UnavailableFamilies = append(set.UnavailableFamilies, family.family)
		}
	}
	return set, nil
}

func familyLabel(family string) string {
	if family == "" {
		return "address"
	}
	return family
}

func listenTCPFamily(family string, v6only bool, addr string) (net.Listener, error) {
	var config net.ListenConfig
	if v6only {
		config.Control = setIPv6Only
	}
	network := "tcp"
	switch family {
	case addressFamilyIPv4:
		network = "tcp4"
	case addressFamilyIPv6:
		network = "tcp6"
	}
	return config.Listen(context.Background(), network, addr)
}

func listenUDPFamily(family string, v6only bool, addr string) (*net.UDPConn, error) {
	var config net.ListenConfig
	if v6only {
		config.Control = setIPv6Only
	}
	network := "udp"
	switch family {
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
	data    []byte
	addr    net.Addr
	err     error
	release chan<- []byte
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

	buf := make([]byte, multiUDPReadBufferSize)
	returned := make(chan []byte, 1)
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
		select {
		case c.reads <- udpReadResult{data: buf[:n], addr: addr, release: returned}:
			// Wait for the consumer to copy the datagram before reusing the
			// per-socket buffer. This keeps one buffer per socket and removes
			// the per-datagram allocation/copy on the read path.
			select {
			case buf = <-returned:
			case <-c.done:
				return
			}
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
		n := copy(b, result.data)
		if result.release != nil {
			result.release <- result.data[:cap(result.data)]
		}
		return n, result.addr, nil
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
