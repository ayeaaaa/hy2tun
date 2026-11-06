package serverui

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

func TestServerTrafficLabelsUseTunnelDirection(t *testing.T) {
	api := &fakeServerAPI{
		overview: view.Overview{TxBytes: 1024, RxBytes: 2048},
		clients: []view.ClientView{{
			ID:     "c1",
			Name:   "home",
			Online: true,
			Mappings: []view.MappingView{{
				Mapping: config.Mapping{ID: "m1", Type: "tcp"},
				Status:  "running",
				TxBytes: 1024,
				RxBytes: 2048,
			}},
		}},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	for _, target := range []string{"/", "/clients/c1/detail", "/mappings"} {
		resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+target, "", cookie,
			map[string]string{"Accept": "text/html"})
		require.Equal(t, http.StatusOK, resp.StatusCode, target)
		body := readBody(t, resp)
		require.Contains(t, body, "客户端 → 服务端", target)
		require.Contains(t, body, "服务端 → 客户端", target)
		require.NotContains(t, body, "总上行", target)
		require.NotContains(t, body, "总下行", target)
		require.NotContains(t, body, "↑", target)
		require.NotContains(t, body, "↓", target)
	}

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/metrics", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	metrics := readBody(t, resp)
	require.Contains(t, metrics, "Total bytes relayed from clients toward public peers.")
	require.Contains(t, metrics, "Total bytes relayed from public peers toward clients.")
	require.NotContains(t, metrics, "Total bytes sent to clients.")
	require.NotContains(t, metrics, "Total bytes received from clients.")
}
