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

func TestExposureClientBadge(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{Name: "home", Server: "example.com:443", Connected: true},
		mappings: []view.MappingView{
			{
				Mapping:             config.Mapping{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Enabled: true},
				Status:              "running",
				AssignedPort:        2222,
				Exposure:            view.ExposureLocal,
				UnavailableFamilies: []string{"ipv6", "ipv4"},
			},
			{
				Mapping:             config.Mapping{ID: "m2", Type: "tcp", RemotePort: 2223, LocalAddr: "127.0.0.1:23", Enabled: true},
				Status:              "running",
				AssignedPort:        2223,
				Exposure:            view.ExposurePublic,
				UnavailableFamilies: []string{"ipv6"},
			},
			{
				Mapping: config.Mapping{ID: "m3", Type: "udp", RemotePort: 5353, LocalAddr: "127.0.0.1:53", Enabled: true},
				Status:  "pending",
			},
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie,
		map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)

	require.Contains(t, body, "仅本机")
	require.Contains(t, body, "公网")
	require.Contains(t, body, "—", "an unknown legacy exposure must render as an em dash")
	require.Contains(t, body, "IPv6 不可用")
	require.Contains(t, body, "IPv4/IPv6 不可用",
		"family labels must be canonicalized to IPv4 before IPv6")
	require.Contains(t, body, `data-mapping-exposure="local"`)
	require.Contains(t, body, `data-mapping-exposure="public"`)
	require.Contains(t, body, `data-mapping-exposure=""`)
	require.Contains(t, body, `data-exposure-badge`)
	require.Contains(t, body, `data-exposure-badge>—</span>`,
		"an unknown legacy exposure must render an em dash badge")
	require.Contains(t, body, `data-exposure-warning`)

	require.NotContains(t, body, `data-action="toggle-exposure"`,
		"the client console must not expose a mapping exposure control")
	require.NotContains(t, body, `data-exposure-control`)
	require.NotContains(t, body, "/exposure")

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/mappings", "", cookie,
		map[string]string{"Accept": "application/json"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	mappings := readClientJSON[[]mappingResponse](t, resp)
	require.Len(t, mappings, 3)
	require.Equal(t, view.ExposureLocal, mappings[0].Exposure)
	require.ElementsMatch(t, []string{"ipv6", "ipv4"}, mappings[0].UnavailableFamilies)
	require.Equal(t, view.ExposurePublic, mappings[1].Exposure)
	require.Equal(t, []string{"ipv6"}, mappings[1].UnavailableFamilies)
	require.Empty(t, mappings[2].Exposure)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/static/app.js", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	script := readClientBody(t, resp)
	require.Contains(t, script, "mappingExposure")
	require.Contains(t, script, "不可用")
	require.NotContains(t, script, "toggle-exposure")
}
