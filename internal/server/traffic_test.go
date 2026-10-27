package server

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

// TestServerCountsTCPTraffic verifies that the public TCP connection wrapper
// advances both counters, which the server overview and client views read.
func TestServerCountsTCPTraffic(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := pair.Server.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 1 && views[0].Mappings[0].Status == "running"
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	requireTCPEcho(t, conn, "traffic")
	require.NoError(t, conn.Close())

	require.NoError(t, waitFor(t, 3*time.Second, func() bool {
		overview := pair.Server.Overview()
		if overview.TxBytes == 0 || overview.RxBytes == 0 {
			return false
		}
		views := pair.Server.Clients()
		if len(views) != 1 || len(views[0].Mappings) != 1 {
			return false
		}
		mapping := views[0].Mappings[0]
		return mapping.TxBytes > 0 && mapping.RxBytes > 0
	}))
}

// TestServerCountsUDPTraffic verifies that the public UDP wrapper counts
// datagrams in both directions.
func TestServerCountsUDPTraffic(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := pair.Server.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 1 && views[0].Mappings[0].Status == "running"
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort

	reply, err := exchangeUDP(fmt.Sprintf("127.0.0.1:%d", port), []byte("udp-traffic"), 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "udp-traffic", string(reply))

	require.NoError(t, waitFor(t, 3*time.Second, func() bool {
		overview := pair.Server.Overview()
		if overview.TxBytes == 0 || overview.RxBytes == 0 {
			return false
		}
		views := pair.Server.Clients()
		if len(views) != 1 || len(views[0].Mappings) != 1 {
			return false
		}
		mapping := views[0].Mappings[0]
		return mapping.TxBytes > 0 && mapping.RxBytes > 0
	}))
}
