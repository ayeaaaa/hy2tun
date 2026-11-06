package clientui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

func TestClientTrafficLabelsUseTunnelDirection(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{TxBytes: 1024, RxBytes: 2048},
		mappings: []view.MappingView{{
			Mapping: config.Mapping{ID: "m1", Type: "tcp"},
			TxBytes: 1024,
			RxBytes: 2048,
		}},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.Contains(t, body, "客户端 → 服务端")
	require.Contains(t, body, "服务端 → 客户端")
	require.GreaterOrEqual(t, strings.Count(body, "客户端 → 服务端"), 2,
		"both the overview card and mapping cell must use the direction label")
	require.NotContains(t, body, ">上行<")
	require.NotContains(t, body, ">下行<")
	require.NotContains(t, body, "↑")
	require.NotContains(t, body, "↓")

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	require.NoError(t, err)
	metrics := readClientBody(t, resp)
	require.Contains(t, metrics, "Total bytes sent from the client to the server.")
	require.Contains(t, metrics, "Total bytes sent from the server to the client.")
}
