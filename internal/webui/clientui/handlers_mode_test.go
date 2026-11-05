package clientui

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

// modeUnsupportedAPI exposes only the historical API surface. Its dynamic type
// deliberately does not implement ModeAPI.
type modeUnsupportedAPI struct {
	API
}

func clientModeBody(mode, password string) string {
	return `{"mode":"` + mode + `","current_password":"` + password + `"}`
}

func clientModeHeaders(ts *httptest.Server) map[string]string {
	return map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL}
}

func TestModeAPICSRFAndPassword(t *testing.T) {
	api := &fakeClientAPI{
		settings: SettingsView{Transport: view.TransportState{Mode: config.ModeHY2}},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	body := clientModeBody("tcp", "pw")
	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode", body, cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode", body, cookie,
		map[string]string{"X-Requested-With": "hy2tun"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readClientBody(t, resp)
	require.Zero(t, api.modeCalls, "CSRF failure must not invoke UpdateMode")

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		clientModeBody("tcp", "wrong"), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readClientBody(t, resp)
	require.Zero(t, api.modeCalls, "wrong password must not invoke UpdateMode")

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		`{"mode":"tcp"}`, cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readClientBody(t, resp)
	require.Zero(t, api.modeCalls, "missing current password must not invoke UpdateMode")

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode", body, cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	state := readClientJSON[view.TransportState](t, resp)
	require.Equal(t, config.ModeTCP, state.Mode)
	require.Equal(t, 1, api.modeCalls)
	require.Equal(t, []config.TransportMode{config.ModeTCP}, api.modeValues)
}

func TestModeAPIServerSelection(t *testing.T) {
	us := &fakeClientAPI{
		status:   view.ClientStatus{Name: "us", Server: "us.example.com:443", Connected: true, Transport: view.TransportState{Mode: config.ModeHY2}},
		settings: SettingsView{Name: "us", Transport: view.TransportState{Mode: config.ModeHY2}},
	}
	sg := &fakeClientAPI{
		status:   view.ClientStatus{Name: "sg", Server: "sg.example.com:443", Connected: true, Transport: view.TransportState{Mode: config.ModeHY2, ActiveTransport: "hy2"}},
		settings: SettingsView{Name: "sg", Transport: view.TransportState{Mode: config.ModeHY2}},
	}
	multi := &fakeMultiAPI{
		servers: map[string]*fakeClientAPI{"us": us, "sg": sg},
		order:   []string{"us", "sg"},
	}
	_, ts := newTestClientWeb(t, multi)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode?server=sg",
		clientModeBody("tcp", "pw"), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	state := readClientJSON[view.TransportState](t, resp)
	require.Equal(t, config.ModeTCP, state.Mode)
	require.Equal(t, 1, sg.modeCalls)
	require.Equal(t, config.ModeTCP, sg.status.Transport.Mode)
	require.Zero(t, us.modeCalls, "another worker must not be updated")
	require.Equal(t, config.ModeHY2, us.status.Transport.Mode)

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode?server=missing",
		clientModeBody("tcp", "pw"), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, sg.modeCalls)
	require.Zero(t, us.modeCalls)
}

func TestModeAPIInvalidAndSaveFailure(t *testing.T) {
	api := &fakeClientAPI{
		status:   view.ClientStatus{Transport: view.TransportState{Mode: config.ModeHY2}},
		settings: SettingsView{Transport: view.TransportState{Mode: config.ModeHY2}},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		clientModeBody("bogus", "pw"), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "invalid_mode")
	require.Zero(t, api.modeCalls)

	api.modeErr = errors.New("disk full")
	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		clientModeBody("tcp", "pw"), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	readClientBody(t, resp)
	require.Equal(t, 1, api.modeCalls)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	settings := readClientJSON[SettingsView](t, resp)
	require.Equal(t, config.ModeHY2, settings.Transport.Mode)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	status := readClientJSON[statusResponse](t, resp)
	require.Equal(t, config.ModeHY2, status.Mode)
}

func TestTransportStateSecretFree(t *testing.T) {
	const (
		plainAuth  = "plain-auth-value"
		plainToken = "plain-token-value"
	)
	transport := view.TransportState{
		Mode:            config.ModeAuto,
		ActiveTransport: "tcp",
		SwitchReason:    "network_failure",
		LastSwitchAt:    time.Unix(1700000000, 0).UTC(),
		LastProbeAt:     time.Unix(1700000010, 0).UTC(),
		LastProbeError:  "timeout",
		SwitchCount:     3,
	}
	api := &fakeClientAPI{
		status: view.ClientStatus{
			Name:      "home",
			Server:    "secret-target.example:443",
			Connected: true,
			Transport: transport,
		},
		settings: SettingsView{
			Name:      "home",
			Transport: transport,
			Credentials: CredentialSettingsView{
				Generation:                2,
				CurrentAuthMasked:         plainAuth,
				CurrentControlTokenMasked: plainToken,
			},
		},
	}
	web, ts := newTestClientWeb(t, api)
	web.eventInterval = 10 * time.Millisecond
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	statusBody := readClientBody(t, resp)
	for _, key := range []string{"mode", "active_transport", "switch_reason", "last_switch_at", "last_probe_at", "last_probe_error", "switch_count"} {
		require.Contains(t, statusBody, `"`+key+`"`)
	}
	require.NotContains(t, statusBody, plainAuth)
	require.NotContains(t, statusBody, plainToken)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	settingsBody := readClientBody(t, resp)
	require.Contains(t, settingsBody, `"transport"`)
	require.NotContains(t, settingsBody, plainAuth)
	require.NotContains(t, settingsBody, plainToken)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	event, data := readClientSSEEvent(t, bufio.NewReader(resp.Body))
	require.Equal(t, "snapshot", event)
	require.Contains(t, data, `"active_transport":"tcp"`)
	require.NotContains(t, data, plainAuth)
	require.NotContains(t, data, plainToken)
	resp.Body.Close()
	cancel()

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	pageBody := readClientBody(t, resp)
	require.NotContains(t, pageBody, plainAuth)
	require.NotContains(t, pageBody, plainToken)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/metrics", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	metricsBody := readClientBody(t, resp)
	require.NotContains(t, metricsBody, plainAuth)
	require.NotContains(t, metricsBody, plainToken)
	require.NotContains(t, metricsBody, "secret-target.example", "metrics labels must not contain targets")
}

func TestMultiMetricsShowsEachServerTransport(t *testing.T) {
	us := &fakeClientAPI{
		status: view.ClientStatus{
			Name:      "us",
			Server:    "us.example.com:443",
			Connected: true,
			Transport: view.TransportState{Mode: config.ModeAuto, ActiveTransport: "hy2", SwitchCount: 2},
		},
		settings: SettingsView{Name: "us", Transport: view.TransportState{Mode: config.ModeAuto, ActiveTransport: "hy2", SwitchCount: 2}},
	}
	sg := &fakeClientAPI{
		status: view.ClientStatus{
			Name:      "sg",
			Server:    "sg.example.com:443",
			Connected: true,
			Transport: view.TransportState{Mode: config.ModeAuto, ActiveTransport: "tcp", SwitchCount: 1},
		},
		settings: SettingsView{Name: "sg", Transport: view.TransportState{Mode: config.ModeAuto, ActiveTransport: "tcp", SwitchCount: 1}},
	}
	multi := &fakeMultiAPI{
		servers: map[string]*fakeClientAPI{"us": us, "sg": sg},
		order:   []string{"us", "sg"},
	}
	const token = "0123456789abcdef"
	web, err := NewClient(multi, config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: mustHash("pw"),
		MetricsToken: token,
	}, testSessionKey, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = web.Close() })
	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/metrics", "", nil,
		map[string]string{"Authorization": "Bearer " + token})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)

	require.Contains(t, body, `hy2tun_client_transport_active{server="us",transport="hy2"} 1`)
	require.Contains(t, body, `hy2tun_client_transport_active{server="us",transport="tcp"} 0`)
	require.Contains(t, body, `hy2tun_client_transport_active{server="sg",transport="hy2"} 0`)
	require.Contains(t, body, `hy2tun_client_transport_active{server="sg",transport="tcp"} 1`)
	require.Contains(t, body, `hy2tun_client_transport_switches_total{server="us"} 2`)
	require.Contains(t, body, `hy2tun_client_transport_switches_total{server="sg"} 1`)
	require.Contains(t, body, "hy2tun_client_up 1", "aggregate metrics must remain")

	cookie := loginClientWeb(t, ts)
	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/servers", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	serversBody := readClientBody(t, resp)
	require.Contains(t, serversBody, `"name":"us"`)
	require.Contains(t, serversBody, `"mode":"hy2+tcp"`)
	require.Contains(t, serversBody, `"active_transport":"hy2"`)
	require.Contains(t, serversBody, `"switch_count":2`)
	require.Contains(t, serversBody, `"name":"sg"`)
	require.Contains(t, serversBody, `"active_transport":"tcp"`)
}

func TestMultiMetricsEscapesTransportLabels(t *testing.T) {
	name := "edge\"one\nline"
	api := &fakeClientAPI{
		status: view.ClientStatus{
			Transport: view.TransportState{Mode: config.ModeTCP, ActiveTransport: "tcp", SwitchCount: 1},
		},
		settings: SettingsView{Transport: view.TransportState{Mode: config.ModeTCP, ActiveTransport: "tcp", SwitchCount: 1}},
	}
	multi := &fakeMultiAPI{
		servers: map[string]*fakeClientAPI{name: api},
		order:   []string{name},
	}
	_, ts := newTestClientWeb(t, multi)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/metrics", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.Contains(t, body, `server="edge\"one\nline"`)
}

func TestModeAPIPageOptionsAndWarning(t *testing.T) {
	transport := view.TransportState{
		Mode:            config.ModeAuto,
		ActiveTransport: "tcp",
		SwitchReason:    "network_failure",
		LastSwitchAt:    time.Unix(1700000000, 0).UTC(),
		LastProbeAt:     time.Unix(1700000010, 0).UTC(),
		LastProbeError:  "timeout",
		SwitchCount:     3,
	}
	api := &fakeClientAPI{
		status:   view.ClientStatus{Name: "home", Server: "example.com:443", Connected: true, Transport: transport},
		settings: SettingsView{Name: "home", Transport: transport},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	fragment := selectFragment(t, body, "transport-mode")
	require.Equal(t, 3, strings.Count(fragment, "<option"))
	for _, mode := range []string{"hy2", "tcp", "hy2+tcp"} {
		require.Contains(t, fragment, `value="`+mode+`"`)
	}
	require.Contains(t, body, "切换会断开现有连接")
	require.Contains(t, body, "可能丢失 UDP 数据包")
	require.Contains(t, body, "TCP（UDP over TCP）")

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/servers", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	serversBody := readClientBody(t, resp)
	require.Contains(t, serversBody, `data-server-mode="hy2&#43;tcp"`)
	require.Contains(t, serversBody, `data-server-transport="tcp"`)
	require.Contains(t, serversBody, "TCP（UDP over TCP）")
}

func TestModeAPIPageUnsupported(t *testing.T) {
	api := modeUnsupportedAPI{API: &fakeClientAPI{
		settings: SettingsView{Transport: view.TransportState{Mode: config.ModeHY2}},
	}}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.NotContains(t, body, `id="settings-mode-form"`)
	require.NotContains(t, body, `id="transport-mode"`)

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		clientModeBody("tcp", "pw"), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "not_implemented")
}

func selectFragment(t *testing.T, body, id string) string {
	t.Helper()
	marker := `id="` + id + `"`
	index := strings.Index(body, marker)
	require.NotEqual(t, -1, index, "select %q not found", id)
	start := strings.LastIndex(body[:index], "<select")
	require.NotEqual(t, -1, start)
	end := strings.Index(body[index:], "</select>")
	require.NotEqual(t, -1, end)
	return body[start : index+end+len("</select>")]
}
