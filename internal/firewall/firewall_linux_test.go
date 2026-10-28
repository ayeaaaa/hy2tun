//go:build linux

package firewall

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/ports"
)

type fakeRunner struct {
	paths map[string]bool
	cmds  [][]string
}

func (f *fakeRunner) LookPath(file string) (string, error) {
	if f.paths[file] {
		return "/usr/sbin/" + file, nil
	}
	return "", &fakePathError{file}
}

type fakePathError struct{ file string }

func (e *fakePathError) Error() string { return e.file + " not found" }

func (f *fakeRunner) Run(name string, args ...string) error {
	f.cmds = append(f.cmds, append([]string{name}, args...))
	return nil
}

func TestRedirectRangesSkipsFirstPort(t *testing.T) {
	require.Equal(t, []ports.Range{{Start: 25001, End: 25002}, {Start: 25004, End: 25005}},
		redirectRanges([]ports.Range{{Start: 25000, End: 25002}, {Start: 25004, End: 25005}}))
	require.Empty(t, redirectRanges([]ports.Range{{Start: 25000, End: 25000}}))
}

func TestSetupNFTables(t *testing.T) {
	t.Setenv(backendEnv, "nftables")
	runner := &fakeRunner{paths: map[string]bool{"nft": true}}
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 25000}

	closer, err := setupWithRunner(runner, addr, []ports.Range{{Start: 25000, End: 25002}})
	require.NoError(t, err)
	require.NotNil(t, closer)
	require.Len(t, runner.cmds, 4)
	require.Equal(t, []string{"nft", "delete", "table", "inet", runner.cmds[1][4]}, runner.cmds[0])
	require.Equal(t, []string{"nft", "add", "table", "inet", runner.cmds[1][4]}, runner.cmds[1])
	require.Contains(t, strings.Join(runner.cmds[2], " "), "type nat hook prerouting")
	require.Contains(t, strings.Join(runner.cmds[3], " "), "udp dport 25001-25002 redirect to :25000")

	require.NoError(t, closer.Close())
	require.Equal(t, []string{"nft", "delete", "table", "inet", runner.cmds[1][4]}, runner.cmds[4])
	require.NoError(t, closer.Close())
	require.Len(t, runner.cmds, 5)
}

func TestSetupIPTables(t *testing.T) {
	t.Setenv(backendEnv, "iptables")
	runner := &fakeRunner{paths: map[string]bool{"iptables": true}}
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 25000}

	closer, err := setupWithRunner(runner, addr, []ports.Range{{Start: 25000, End: 25002}})
	require.NoError(t, err)
	require.Len(t, runner.cmds, 1)
	require.Contains(t, strings.Join(runner.cmds[0], " "), "-A PREROUTING -p udp --dport 25001-25002 -j REDIRECT --to-ports 25000")

	require.NoError(t, closer.Close())
	require.Len(t, runner.cmds, 2)
	require.Contains(t, strings.Join(runner.cmds[1], " "), "-D PREROUTING -p udp --dport 25001-25002 -j REDIRECT --to-ports 25000")
}

func TestSetupRejectsMissingFirewall(t *testing.T) {
	t.Setenv(backendEnv, "")
	runner := &fakeRunner{paths: map[string]bool{}}
	_, err := setupWithRunner(runner, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 25000}, []ports.Range{{Start: 25000, End: 25002}})
	require.ErrorContains(t, err, "no iptables/ip6tables")
}

func TestSetupNFTablesIntegration(t *testing.T) {
	if os.Getenv("HY2TUN_FIREWALL_INTEGRATION") != "1" {
		t.Skip("set HY2TUN_FIREWALL_INTEGRATION=1 to exercise the host firewall")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not installed")
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr)
	start := addr.Port
	require.Less(t, start, 65534)
	ranges := []ports.Range{{Start: start, End: start + 2}}

	t.Setenv(backendEnv, "nftables")
	closer, err := SetupUDPPortRedirect(addr, ranges)
	require.NoError(t, err)
	require.NotNil(t, closer)
	t.Cleanup(func() { _ = closer.Close() })

	out, err := exec.Command("nft", "list", "tables").CombinedOutput()
	require.NoError(t, err)
	require.Contains(t, string(out), "hy2tun_porthop_")

	require.NoError(t, closer.Close())
	out, err = exec.Command("nft", "list", "tables").CombinedOutput()
	require.NoError(t, err)
	require.NotContains(t, string(out), "hy2tun_porthop_"+ruleName(addr, ranges))
}
