//go:build linux

package firewall

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"hy2tun/internal/ports"
)

type commandRunner interface {
	LookPath(string) (string, error)
	Run(string, ...string) error
}

type osCommandRunner struct{}

func (osCommandRunner) LookPath(file string) (string, error) { return exec.LookPath(file) }

func (osCommandRunner) Run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctx.Err() != nil {
		return fmt.Errorf("%s %v: %w", name, args, ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetupUDPPortRedirect redirects every port in ranges except the first bind
// port to the first port. The returned closer removes the rules.
func SetupUDPPortRedirect(addr *net.UDPAddr, ranges []ports.Range) (io.Closer, error) {
	return setupWithRunner(osCommandRunner{}, addr, ranges)
}

func setupWithRunner(r commandRunner, addr *net.UDPAddr, ranges []ports.Range) (io.Closer, error) {
	redirects := redirectRanges(ranges)
	if len(redirects) == 0 {
		return nil, nil
	}
	if addr == nil || addr.Port <= 0 {
		return nil, fmt.Errorf("port hopping redirect requires a bound listen address")
	}

	backend := strings.ToLower(strings.TrimSpace(os.Getenv(backendEnv)))
	switch backend {
	case "nft", "nftables":
		return setupNFTables(r, addr, ranges, redirects)
	case "iptables", "ipt":
		return setupIPTables(r, addr, redirects)
	case "":
		if _, err := r.LookPath("nft"); err == nil {
			return setupNFTables(r, addr, ranges, redirects)
		}
		return setupIPTables(r, addr, redirects)
	default:
		return nil, fmt.Errorf("unsupported %s %q", backendEnv, backend)
	}
}

func setupNFTables(r commandRunner, addr *net.UDPAddr, ranges, redirects []ports.Range) (io.Closer, error) {
	name := "hy2tun_porthop_" + ruleName(addr, ranges)
	cleanup := &closerFuncs{}
	nft := func(args ...string) error { return r.Run("nft", args...) }

	// Remove a stale table left by a previous crash before creating the new
	// one. The name is derived from the bind address and ranges, so it is
	// specific to this listener.
	_ = nft("delete", "table", "inet", name)
	if err := nft("add", "table", "inet", name); err != nil {
		return nil, err
	}
	cleanup.add(func() { _ = nft("delete", "table", "inet", name) })

	if err := nft("add", "chain", "inet", name, "prerouting",
		"{", "type", "nat", "hook", "prerouting", "priority", "-100;", "policy", "accept;", "}"); err != nil {
		_ = cleanup.Close()
		return nil, err
	}
	for _, portRange := range redirects {
		if err := nft("add", "rule", "inet", name, "prerouting",
			"udp", "dport", formatRange(portRange),
			"redirect", "to", ":"+strconv.Itoa(addr.Port)); err != nil {
			_ = cleanup.Close()
			return nil, err
		}
	}
	return cleanup, nil
}

func setupIPTables(r commandRunner, addr *net.UDPAddr, redirects []ports.Range) (io.Closer, error) {
	binaries := iptablesBinaries(r, addr)
	if len(binaries) == 0 {
		return nil, fmt.Errorf("no iptables/ip6tables binary found")
	}
	cleanup := &closerFuncs{}
	for _, bin := range binaries {
		for _, portRange := range redirects {
			rangeText := formatRange(portRange)
			target := strconv.Itoa(addr.Port)
			args := []string{"-t", "nat", "-A", "PREROUTING", "-p", "udp", "--dport", rangeText, "-j", "REDIRECT", "--to-ports", target}
			if err := r.Run(bin, args...); err != nil {
				_ = cleanup.Close()
				return nil, err
			}
			binCopy, rangeCopy, targetCopy := bin, rangeText, target
			cleanup.add(func() {
				_ = r.Run(binCopy, "-t", "nat", "-D", "PREROUTING", "-p", "udp", "--dport", rangeCopy, "-j", "REDIRECT", "--to-ports", targetCopy)
			})
		}
	}
	return cleanup, nil
}

func iptablesBinaries(r commandRunner, addr *net.UDPAddr) []string {
	var candidates []string
	if addr == nil || addr.IP == nil || addr.IP.To4() != nil || addr.IP.IsUnspecified() {
		candidates = append(candidates, "iptables")
	}
	if addr == nil || addr.IP == nil || addr.IP.To4() == nil || addr.IP.IsUnspecified() {
		candidates = append(candidates, "ip6tables")
	}
	out := make([]string, 0, len(candidates))
	seen := make(map[string]struct{})
	for _, bin := range candidates {
		if _, ok := seen[bin]; ok {
			continue
		}
		seen[bin] = struct{}{}
		if _, err := r.LookPath(bin); err == nil {
			out = append(out, bin)
		}
	}
	return out
}
