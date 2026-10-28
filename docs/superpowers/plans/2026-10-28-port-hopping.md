# hy2tun Port Hopping Implementation Plan

> **Historical note:** This plan describes the v1.2 client-side implementation.
> Server-side `listen` range parsing and automatic nftables/iptables redirects
> were added later; where this document says the server stays on one port, the
> current behavior is documented in `README.md` and `scripts/port-hopping/README.md`.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add client-side Hysteria 2 UDP port hopping to `hy2tun`, with official port-set syntax, fixed/random hop intervals, Salamander obfs composition, and deployment documentation for server-side firewall redirects.

**Architecture:** The client resolves a multi-port `server` address with `extras/v2/transport/udphop` and supplies a reusable `ConnFactory` that creates one hop packet connection per HY2 connection. The server remains on a single `listen` port; operators redirect a UDP range to that port with nftables/iptables. No server code or Web editing is added in this phase.

**Tech Stack:** Go 1.26, `github.com/apernet/hysteria/extras/v2/transport/udphop`, existing `config.Duration`, `log/slog`, `testify/require`.

**Spec:** `docs/superpowers/specs/2026-10-28-port-hopping-design.md`

## Global Constraints

- Client-only code change. Do not add server-side `listen: :range` parsing or firewall rule management.
- Use `extras/v2/transport/udphop`; do not modify the Hysteria wire protocol.
- Client config field names are snake_case: `transport.udp.hop_interval`, `min_hop_interval`, `max_hop_interval`.
- Default hop interval is 30s; minimum is 5s; fixed and random intervals are mutually exclusive.
- A single-port `server` must behave exactly as before, including YAML round-trips with no `transport` section.
- A port set that normalizes to one unique port (for example `443,443` or `443-443`) is treated as single-port and must reject interval settings.
- Every hop socket must be wrapped by Salamander when `obfs.type: salamander`.
- The client must log a warning when port hopping is enabled because it cannot verify server firewall redirects.
- No secrets in logs, errors, or documentation examples.
- Keep both `README.md` and `README.zh-CN.md` in sync.
- `go test ./...`, `go test -race ./...`, and `make build` must pass.
- Commit after each task with a conventional commit message.

## Review Focus

1. **Single-port config gets polluted by `SaveClient`** — a mapping edit must not add a `transport` section or make the file fail validation on restart. Test in Task 1.
2. **A one-port “set” is mistaken for hopping** — `443,443` and `443-443` must behave as single-port. Test in Task 1.
3. **Obfs is not applied to every hop socket** — the hop factory must wrap each socket created by `udphop`, not just the first one. Test in Task 2.
4. **TLS SNI extraction breaks for a port set** — `tls.sni` empty with `host:30000-30001` must still use `host`. Test in Task 2.
5. **No warning when server redirect is missing** — the client must emit a clear warning; actual firewall behavior is covered by the manual acceptance checklist in Task 4.
6. **Hop range overlaps `ports.auto_range`** — the client cannot detect server-side auto-allocation; docs and Task 4 step 7 must force a non-overlapping range.

---

### Task 1: Client config schema and validation

**Files:**
- Modify: `internal/config/client.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `type ClientTransportConfig struct { UDP *ClientTransportUDPConfig ... }`
  - `type ClientTransportUDPConfig struct { HopInterval Duration; MinHopInterval Duration; MaxHopInterval Duration }`
  - `func (c *ClientConfig) ServerPortHopping() bool`
  - `func (c *ClientConfig) NormalizedServer() (address string, hopping bool, err error)`
  - `func (c *ClientConfig) HopInterval() (min time.Duration, max time.Duration, err error)`

- [ ] **Step 1: Write the failing config tests**

Add these tests to `internal/config/config_test.go`:

```go
func TestClientConfigAcceptsPortHoppingServer(t *testing.T) {
	for _, server := range []string{
		"127.0.0.1:443,8443",
		"127.0.0.1:20000-20010",
		"127.0.0.1:443,5000-5001,8443",
	} {
		cfg := validClientConfig()
		cfg.Server = server
		require.NoError(t, cfg.Validate(), server)
		require.True(t, cfg.ServerPortHopping(), server)
	}
}

func TestClientConfigRejectsBadPortHoppingServer(t *testing.T) {
	for _, server := range []string{
		"127.0.0.1",
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"127.0.0.1:443-",
		"127.0.0.1:443,",
		"127.0.0.1:443-442",
		":443",
	} {
		cfg := validClientConfig()
		cfg.Server = server
		require.Error(t, cfg.Validate(), server)
	}
}

func TestClientConfigNormalizesOnePortSet(t *testing.T) {
	for _, server := range []string{"127.0.0.1:443,443", "127.0.0.1:443-443"} {
		cfg := validClientConfig()
		cfg.Server = server
		require.NoError(t, cfg.Validate(), server)
		require.False(t, cfg.ServerPortHopping(), server)
		normalized, hopping, err := cfg.NormalizedServer()
		require.NoError(t, err, server)
		require.False(t, hopping, server)
		require.Equal(t, "127.0.0.1:443", normalized, server)
	}
}

func TestClientConfigPortHoppingIntervals(t *testing.T) {
	tests := []struct {
		name    string
		udp     *ClientTransportUDPConfig
		wantMin time.Duration
		wantMax time.Duration
		wantErr string
	}{
		{
			name: "default",
		},
		{
			name:    "fixed",
			udp:     &ClientTransportUDPConfig{HopInterval: Duration(30 * time.Second)},
			wantMin: 30 * time.Second,
			wantMax: 30 * time.Second,
		},
		{
			name:    "random",
			udp:     &ClientTransportUDPConfig{MinHopInterval: Duration(15 * time.Second), MaxHopInterval: Duration(45 * time.Second)},
			wantMin: 15 * time.Second,
			wantMax: 45 * time.Second,
		},
		{
			name:    "both fixed and random",
			udp:     &ClientTransportUDPConfig{HopInterval: Duration(30 * time.Second), MinHopInterval: Duration(15 * time.Second), MaxHopInterval: Duration(45 * time.Second)},
			wantErr: "hop_interval cannot be used together",
		},
		{
			name:    "only min",
			udp:     &ClientTransportUDPConfig{MinHopInterval: Duration(15 * time.Second)},
			wantErr: "min_hop_interval and max_hop_interval must both be set",
		},
		{
			name:    "too short",
			udp:     &ClientTransportUDPConfig{HopInterval: Duration(4 * time.Second)},
			wantErr: "at least 5s",
		},
		{
			name:    "min greater than max",
			udp:     &ClientTransportUDPConfig{MinHopInterval: Duration(45 * time.Second), MaxHopInterval: Duration(15 * time.Second)},
			wantErr: "must not be greater",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validClientConfig()
			cfg.Server = "127.0.0.1:30000-30001"
			if tt.udp != nil {
				cfg.Transport = &ClientTransportConfig{UDP: tt.udp}
			}
			min, max, err := cfg.HopInterval()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantMin, min)
			require.Equal(t, tt.wantMax, max)
			require.NoError(t, cfg.Validate())
		})
	}
}

func TestClientConfigRejectsHopIntervalOnSinglePort(t *testing.T) {
	for _, server := range []string{"127.0.0.1:443", "127.0.0.1:443,443"} {
		cfg := validClientConfig()
		cfg.Server = server
		cfg.Transport = &ClientTransportConfig{UDP: &ClientTransportUDPConfig{
			HopInterval: Duration(30 * time.Second),
		}}
		require.ErrorContains(t, cfg.Validate(), "requires a multi-port server address", server)
	}
}

func TestSaveClientDoesNotAddTransportForSinglePort(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	cfg := validClientConfig()
	require.NoError(t, SaveClient(path, cfg))

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "transport:")

	loaded, err := LoadClient(path)
	require.NoError(t, err)
	require.Nil(t, loaded.Transport)
}

func TestClientTransportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	cfg := validClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cfg.Transport = &ClientTransportConfig{UDP: &ClientTransportUDPConfig{
		MinHopInterval: Duration(15 * time.Second),
		MaxHopInterval: Duration(45 * time.Second),
	}}
	require.NoError(t, SaveClient(path, cfg))

	loaded, err := LoadClient(path)
	require.NoError(t, err)
	require.NotNil(t, loaded.Transport)
	require.NotNil(t, loaded.Transport.UDP)
	require.Equal(t, 15*time.Second, time.Duration(loaded.Transport.UDP.MinHopInterval))
	require.Equal(t, 45*time.Second, time.Duration(loaded.Transport.UDP.MaxHopInterval))
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/config -run 'TestClientConfig.*PortHopping|TestSaveClientDoesNotAddTransport|TestClientTransport' -v
```

Expected: FAIL because `ClientTransportConfig`, `ServerPortHopping`, and `HopInterval` do not exist.

- [ ] **Step 3: Implement the config types and validation**

In `internal/config/client.go`:

- Add imports `net`, `strconv`, `strings`, and `hy2tun/internal/ports`.
- Add the `Transport` field to `ClientConfig`:

```go
Transport *ClientTransportConfig `yaml:"transport,omitempty" json:"transport,omitempty"`
```

- Add these types near `ClientConfig`:

```go
type ClientTransportConfig struct {
	UDP *ClientTransportUDPConfig `yaml:"udp,omitempty" json:"udp,omitempty"`
}

type ClientTransportUDPConfig struct {
	HopInterval    Duration `yaml:"hop_interval,omitempty" json:"hop_interval,omitempty"`
	MinHopInterval Duration `yaml:"min_hop_interval,omitempty" json:"min_hop_interval,omitempty"`
	MaxHopInterval Duration `yaml:"max_hop_interval,omitempty" json:"max_hop_interval,omitempty"`
}
```

- Replace the `validateHostPort("server", ...)` call in `Validate` with:

```go
_, _, hopping, err := parseClientServer(c.Server)
if err != nil {
	return err
}
minHop, maxHop, err := c.HopInterval()
if err != nil {
	return err
}
if !hopping && (minHop != 0 || maxHop != 0) {
	return fmt.Errorf("transport.udp hop interval requires a multi-port server address")
}
```

- Add the parsing and interval methods:

```go
func parseClientServer(value string) (string, []ports.Range, bool, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil, false, fmt.Errorf("server is required")
	}
	host, portStr, err := net.SplitHostPort(value)
	if err != nil {
		return "", nil, false, fmt.Errorf("server must be a valid host:port or port range: %w", err)
	}
	if host == "" {
		return "", nil, false, fmt.Errorf("server must include a host")
	}
	ranges, err := ports.ParseRanges(strings.Split(portStr, ","))
	if err != nil {
		return "", nil, false, fmt.Errorf("server: %w", err)
	}
	return host, ranges, uniquePortCount(ranges) > 1, nil
}

func uniquePortCount(ranges []ports.Range) int {
	seen := make(map[int]struct{})
	for _, r := range ranges {
		for port := r.Start; port <= r.End; port++ {
			seen[port] = struct{}{}
		}
	}
	return len(seen)
}

func (c *ClientConfig) ServerPortHopping() bool {
	_, _, hopping, err := parseClientServer(c.Server)
	return err == nil && hopping
}

func (c *ClientConfig) NormalizedServer() (string, bool, error) {
	host, ranges, hopping, err := parseClientServer(c.Server)
	if err != nil {
		return "", false, err
	}
	if hopping {
		return c.Server, true, nil
	}
	if len(ranges) == 0 {
		return "", false, fmt.Errorf("server must include a port")
	}
	return net.JoinHostPort(host, strconv.Itoa(ranges[0].Start)), false, nil
}

func (c *ClientConfig) HopInterval() (time.Duration, time.Duration, error) {
	if c.Transport == nil || c.Transport.UDP == nil {
		return 0, 0, nil
	}
	udp := c.Transport.UDP
	fixed := time.Duration(udp.HopInterval)
	min := time.Duration(udp.MinHopInterval)
	max := time.Duration(udp.MaxHopInterval)

	if fixed != 0 && (min != 0 || max != 0) {
		return 0, 0, fmt.Errorf("hop_interval cannot be used together with min_hop_interval or max_hop_interval")
	}
	if min == 0 && max == 0 {
		if fixed == 0 {
			return 0, 0, nil
		}
		if fixed < 5*time.Second {
			return 0, 0, fmt.Errorf("hop interval must be at least 5s")
		}
		return fixed, fixed, nil
	}
	if min == 0 || max == 0 {
		return 0, 0, fmt.Errorf("min_hop_interval and max_hop_interval must both be set")
	}
	if min < 5*time.Second || max < 5*time.Second {
		return 0, 0, fmt.Errorf("hop interval must be at least 5s")
	}
	if min > max {
		return 0, 0, fmt.Errorf("min_hop_interval must not be greater than max_hop_interval")
	}
	return min, max, nil
}
```

Do not change `applyDefaults`; the 30s default is applied only by `udphop` at runtime.

- [ ] **Step 4: Run the config tests**

Run:

```bash
go test ./internal/config -run 'TestClientConfig.*PortHopping|TestSaveClientDoesNotAddTransport|TestClientTransport' -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/client.go internal/config/config_test.go
git commit -m "feat(config): accept client port-hopping server addresses"
```

---

### Task 2: Client resolution, hop factory, and warning

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/hop_test.go` (create)

**Interfaces:**
- Consumes:
  - `ClientConfig.ServerPortHopping() bool`
  - `ClientConfig.NormalizedServer() (string, bool, error)`
  - `ClientConfig.HopInterval() (time.Duration, time.Duration, error)`
- Produces:
  - `func resolveServerAddr(cfg *config.ClientConfig) (net.Addr, error)`
  - `func buildConnFactory(cfg *config.ClientConfig, serverAddr net.Addr) (hyclient.ConnFactory, error)`
  - `type hopConnFactory struct { addr *udphop.UDPHopAddr; interval udphop.HopIntervalConfig; psk []byte; listenUDP udphop.ListenUDPFunc }`
  - `func (f *hopConnFactory) New(net.Addr) (net.PacketConn, error)`

- [ ] **Step 1: Write the failing client tests**

Create `internal/client/hop_test.go`:

```go
package client

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/apernet/hysteria/extras/v2/transport/udphop"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func TestResolveServerAddrSinglePort(t *testing.T) {
	cfg := validTestClientConfig()
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)
	require.IsType(t, &net.UDPAddr{}, addr)
}

func TestResolveServerAddrPortHopping(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)
	require.IsType(t, &udphop.UDPHopAddr{}, addr)
	require.Equal(t, "udphop", addr.Network())
}

func TestResolveServerAddrNormalizesOnePortSet(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:443,443"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)
	require.IsType(t, &net.UDPAddr{}, addr)
	require.Equal(t, 443, addr.(*net.UDPAddr).Port)
}

func TestBuildConnFactorySelectsHop(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	require.IsType(t, &hopConnFactory{}, factory)
}

func TestBuildConnFactorySinglePortUnchanged(t *testing.T) {
	cfg := validTestClientConfig()
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	require.Nil(t, factory)

	cfg.Obfs.Type = "salamander"
	cfg.Obfs.Salamander.Password = "obfs-secret"
	factory, err = buildConnFactory(cfg, addr)
	require.NoError(t, err)
	require.IsType(t, &salamanderConnFactory{}, factory)
}

func TestHopConnFactoryCreatesAndCloses(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cfg.Transport = &config.ClientTransportConfig{UDP: &config.ClientTransportUDPConfig{
		HopInterval: config.Duration(5 * time.Second),
	}}
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	conn, err := factory.New(nil)
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, conn.Close())
}

func TestHopConnFactoryWithObfs(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cfg.Obfs.Type = "salamander"
	cfg.Obfs.Salamander.Password = "obfs-secret"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	hopFactory := factory.(*hopConnFactory)
	require.Equal(t, []byte("obfs-secret"), hopFactory.psk)

	conn, err := factory.New(nil)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestHopConnFactoryObfsWrapsPackets(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer func() { require.NoError(t, listener.Close()) }()
	port := listener.LocalAddr().(*net.UDPAddr).Port

	addr := &udphop.UDPHopAddr{
		IP:      net.IPv4(127, 0, 0, 1),
		Ports:   []uint16{uint16(port)},
		PortStr: strconv.Itoa(port),
	}
	factory := &hopConnFactory{
		addr:     addr,
		interval: udphop.HopIntervalConfig{Min: 5 * time.Second, Max: 5 * time.Second},
		psk:      []byte("obfs-secret"),
	}
	conn, err := factory.New(nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	payload := []byte("hello-port-hop")
	_, err = conn.WriteTo(payload, nil)
	require.NoError(t, err)

	require.NoError(t, listener.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 2048)
	n, _, err := listener.ReadFromUDP(buf)
	require.NoError(t, err)
	require.NotEqual(t, string(payload), string(buf[:n]))
}

func TestBuildTLSConfigUsesHostFromPortHoppingAddress(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "example.com:30000-30001"
	tlsConfig, err := buildTLSConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, "example.com", tlsConfig.ServerName)
}

func TestNewWarnsForPortHopping(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, cl.Close()) }()

	entries := cl.logs.Entries("warn", 10)
	require.NotEmpty(t, entries)
	require.Contains(t, entries[len(entries)-1].Message, "port hopping")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run:

```bash
go test ./internal/client -run 'TestResolveServerAddr|TestBuildConnFactory|TestHopConnFactory|TestBuildTLSConfigUsesHost|TestNewWarnsForPortHopping' -v
```

Expected: FAIL because `resolveServerAddr` and `hopConnFactory` do not exist, and `buildConnFactory` has the old signature.

- [ ] **Step 3: Implement address resolution and the hop factory**

In `internal/client/client.go`:

- Add imports:
  - `"strings"` (already imported; keep)
  - `"github.com/apernet/hysteria/extras/v2/transport/udphop"`

- In `New`, replace:

```go
serverAddr, err := net.ResolveUDPAddr("udp", cfg.Server)
if err != nil {
	return nil, fmt.Errorf("resolve server address %s: %w", cfg.Server, err)
}
```

with:

```go
serverAddr, err := resolveServerAddr(cfg)
if err != nil {
	return nil, err
}
```

and replace `buildConnFactory(cfg)` with `buildConnFactory(cfg, serverAddr)`.

- After the logger is created and before `New` returns, add:

```go
if cfg.ServerPortHopping() {
	minHop, maxHop, _ := cfg.HopInterval()
	log.Warn("port hopping enabled; server must redirect the entire UDP range to the HY2 listen port",
		"server", cfg.Server, "hop_min_interval", minHop.String(), "hop_max_interval", maxHop.String())
}
```

- Add `resolveServerAddr`:

```go
func resolveServerAddr(cfg *config.ClientConfig) (net.Addr, error) {
	normalized, hopping, err := cfg.NormalizedServer()
	if err != nil {
		return nil, err
	}
	if hopping {
		addr, err := udphop.ResolveUDPHopAddr(normalized)
		if err != nil {
			return nil, fmt.Errorf("resolve server address %s: %w", normalized, err)
		}
		return addr, nil
	}
	addr, err := net.ResolveUDPAddr("udp", normalized)
	if err != nil {
		return nil, fmt.Errorf("resolve server address %s: %w", normalized, err)
	}
	return addr, nil
}
```

- Replace `buildConnFactory` with:

```go
func buildConnFactory(cfg *config.ClientConfig, serverAddr net.Addr) (hyclient.ConnFactory, error) {
	var psk []byte
	switch cfg.Obfs.Type {
	case "":
	case "salamander":
		psk = []byte(cfg.Obfs.Salamander.Password)
		if len(psk) < 4 {
			return nil, fmt.Errorf("obfs.salamander.password must be at least 4 bytes")
		}
	default:
		return nil, fmt.Errorf("unsupported obfs.type %q", cfg.Obfs.Type)
	}

	if hopAddr, ok := serverAddr.(*udphop.UDPHopAddr); ok {
		minHop, maxHop, err := cfg.HopInterval()
		if err != nil {
			return nil, err
		}
		return &hopConnFactory{
			addr:     hopAddr,
			interval: udphop.HopIntervalConfig{Min: minHop, Max: maxHop},
			psk:      psk,
		}, nil
	}
	if len(psk) == 0 {
		return nil, nil
	}
	return &salamanderConnFactory{psk: psk}, nil
}
```

- Add `hopConnFactory` after `salamanderConnFactory`:

```go
// hopConnFactory creates a new UDP hop packet connection for every HY2
// connection. The factory itself is reusable across client reconnects.
type hopConnFactory struct {
	addr      *udphop.UDPHopAddr
	interval  udphop.HopIntervalConfig
	psk       []byte
	listenUDP udphop.ListenUDPFunc
}

func (f *hopConnFactory) New(net.Addr) (net.PacketConn, error) {
	open := f.listenUDP
	if open == nil {
		open = func() (net.PacketConn, error) {
			return net.ListenUDP("udp", nil)
		}
	}
	if len(f.psk) > 0 {
		base := open
		open = func() (net.PacketConn, error) {
			conn, err := base()
			if err != nil {
				return nil, err
			}
			wrapped, err := obfs.WrapPacketConnSalamander(conn, f.psk)
			if err != nil {
				_ = conn.Close()
				return nil, err
			}
			return wrapped, nil
		}
	}
	return udphop.NewUDPHopPacketConn(f.addr, f.interval, open)
}
```

- [ ] **Step 4: Run the client tests**

Run:

```bash
go test ./internal/client -run 'TestResolveServerAddr|TestBuildConnFactory|TestHopConnFactory|TestBuildTLSConfigUsesHost|TestNewWarnsForPortHopping' -v
```

Expected: PASS.

- [ ] **Step 5: Run the existing client/config tests for regressions**

Run:

```bash
go test ./internal/client ./internal/config
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/client/client.go internal/client/hop_test.go
git commit -m "feat(client): add HY2 UDP port hopping"
```

---

### Task 3: Documentation and examples

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `examples/client.yaml`
- Modify: `examples/server.yaml`
- Create: `scripts/port-hopping/README.md`

**Interfaces:**
- No code interfaces. This task documents the exact behavior from Tasks 1-2 and the manual server redirect from the spec.

- [ ] **Step 1: Add the client example**

In `examples/client.yaml`, near the `server` field, add commented guidance:

```yaml
# Optional Hysteria 2 UDP port hopping. The server must redirect every UDP
# port in this set to its HY2 listen port; hy2tun cannot verify that firewall
# rule. Use a range that does not overlap any UDP reverse-mapping port,
# including ports.allow and ports.auto_range on the server.
# server: "sg.example.com:30000-40000"
#
# transport:
#   udp:
#     hop_interval: "30s"       # fixed interval, minimum 5s
#     # min_hop_interval: "15s" # or random interval
#     # max_hop_interval: "45s"
```

Keep the active `server: "sg.example.com:443"` unchanged.

- [ ] **Step 2: Add the server example note**

In `examples/server.yaml`, near `listen`, add:

```yaml
# Port hopping uses a single HY2 listen port plus an external firewall
# redirect, for example:
#   nft add rule inet hy2tun_porthop prerouting udp dport 30000-40000 \
#     redirect to :24443
# Do not put a UDP reverse-mapping port inside the redirected range.
```

- [ ] **Step 3: Add the English README section**

In `README.md`, after the hot-configuration section and before `## Deployment notes`, add a `## Port hopping` section covering:

- What it does and what it does not do (does not hop public mapping ports).
- Client syntax: `host:443,8443`, `host:20000-30000`, mixed lists.
- `transport.udp.hop_interval` and random min/max; default 30s; minimum 5s; mutual exclusion.
- Server manual nftables and iptables examples with `redirect to :24443`.
- Requirement: Linux, nft/iptables, root or `CAP_NET_ADMIN`, cloud firewall open for the whole UDP range.
- Conflict rule: keep the range away from `ports.allow`, `ports.auto_range`, and explicit UDP mappings.
- Warning that the client cannot verify server firewall rules.
- A verification command using `conntrack` or packet capture, and a note that `udphop` may pick the same port again.
- Compatibility with standard Hysteria 2 clients/servers and obfs.

Also update the client config reference table with:

```markdown
| `transport.udp.hop_interval` | Fixed port-hop interval for a multi-port `server`. Default 30s; minimum 5s. |
| `transport.udp.min_hop_interval`, `max_hop_interval` | Random hop interval; both required, minimum 5s, mutually exclusive with `hop_interval`. |
```

- [ ] **Step 4: Add the Chinese README section**

Add the equivalent `## 端口跳跃` section to `README.zh-CN.md`, with the same commands and warnings translated into Chinese. Update its client config table with the same three fields.

- [ ] **Step 5: Add the deployment note**

Create `scripts/port-hopping/README.md` with:

- nftables add/list/delete commands;
- iptables/ip6tables add/delete commands;
- a warning to persist rules across reboot;
- a warning that the range must not overlap UDP reverse mappings or `ports.auto_range`;
- a verification checklist (`conntrack -L`, `tcpdump`, TCP/UDP mapping test);
- a note that this phase does not manage firewall rules automatically.

- [ ] **Step 6: Verify docs**

Run:

```bash
git diff --check
python3 - <<'PY'
from pathlib import Path
for name in ("README.md", "README.zh-CN.md"):
    n = Path(name).read_text().count("```")
    assert n % 2 == 0, (name, n)
print("markdown fences OK")
PY
```

Expected: no whitespace errors and balanced code fences.

- [ ] **Step 7: Commit**

```bash
git add README.md README.zh-CN.md examples/client.yaml examples/server.yaml scripts/port-hopping/README.md
git commit -m "docs: document port hopping deployment"
```

---

### Task 4: Full verification and manual acceptance

**Files:**
- No source files.
- Verify: `internal/config`, `internal/client`, `README.md`, `README.zh-CN.md`, `examples/`, `scripts/port-hopping/README.md`.

**Interfaces:**
- Consumes the completed Tasks 1-3.

- [ ] **Step 1: Run the full test suite**

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 2: Run the race detector**

```bash
go test -race ./... -count=1
```

Expected: PASS.

- [ ] **Step 3: Build the binaries**

```bash
make build
```

Expected: `bin/hy2tund` and `bin/hy2tun` are produced.

- [ ] **Step 4: Run the manual server/client acceptance**

On a Linux test server with `nft` or `iptables`:

1. Keep `listen: ":24443"`.
2. Add a redirect for a test range, for example `udp dport 30000-30010 redirect to :24443`.
3. Configure the client with `server: "host:30000-30010"` and `hop_interval: "5s"`.
4. Start the client and confirm the control handshake and the port-hopping warning.
5. Observe the client source port over several intervals; confirm it changes at least once.
6. Run a TCP mapping and a UDP mapping continuously; confirm no disconnect during hops.
7. Confirm the UDP mapping port is outside the redirect range and outside `ports.allow`/`ports.auto_range` UDP allocation.
8. Remove the redirect rule and confirm a hop to an unredirected port fails; restore the rule.
9. Record the commands and results in the task notes or PR description.

Expected: all steps pass; if a step fails, stop and fix before release.

- [ ] **Step 5: Optional release build**

If this is being cut as v1.2.0:

```bash
VERSION=1.2.0 make release
```

Expected: archives and `dist/SHA256SUMS` are produced; verify checksums with `sha256sum -c`.

- [ ] **Step 6: Final commit / handoff**

If Task 4 produced no file changes, do not create an empty commit. Report the test commands and manual results to the human partner.
