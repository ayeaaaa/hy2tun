package serverui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

// modeUnsupportedServerAPI exposes only the historical API surface. Its dynamic
// type deliberately does not implement ModeAPI.
type modeUnsupportedServerAPI struct {
	API
}

func serverModeBody(mode, password string) string {
	return `{"mode":"` + mode + `","current_password":"` + password + `"}`
}

func TestModeAPICSRFAndPassword(t *testing.T) {
	api := &fakeServerAPI{settings: SettingsView{Mode: config.ModeHY2}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := serverModeBody("tcp", "pw")
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode", body, cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode", body, cookie,
		map[string]string{"X-Requested-With": "hy2tun"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readBody(t, resp)
	require.Zero(t, api.modeCalls, "CSRF failure must not invoke UpdateMode")

	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		serverModeBody("tcp", "wrong"), cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readBody(t, resp)
	require.Zero(t, api.modeCalls, "wrong password must not invoke UpdateMode")

	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		`{"mode":"tcp"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readBody(t, resp)
	require.Zero(t, api.modeCalls, "missing current password must not invoke UpdateMode")

	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode", body, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	state := assertServerTransportState(t, readBody(t, resp))
	require.Equal(t, config.ModeTCP, state.Mode)
	require.Equal(t, "tcp", state.ActiveTransport)
	require.Equal(t, 1, api.modeCalls)
	require.Equal(t, []config.TransportMode{config.ModeTCP}, api.modeValues)
}

func TestModeAPISuccessReturnsTransportState(t *testing.T) {
	cases := []struct {
		mode   config.TransportMode
		active string
	}{
		{config.ModeHY2, "hy2"},
		{config.ModeTCP, "tcp"},
		{config.ModeAuto, ""},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			api := &fakeServerAPI{settings: SettingsView{
				Mode:        config.ModeHY2,
				WebUsername: "admin",
				Credentials: CredentialSettingsView{
					Generation:                2,
					CurrentAuthMasked:         "plain-auth-value",
					CurrentControlTokenMasked: "plain-token-value",
				},
				Clients: []ClientCredentialView{{ClientID: "c1", Name: "home"}},
			}}
			_, ts := newTestWeb(t, api)
			cookie := login(t, ts)

			resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
				serverModeBody(string(tc.mode), "pw"), cookie, csrfHeaders(ts.URL))
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body := readBody(t, resp)
			state := assertServerTransportState(t, body)
			require.Equal(t, tc.mode, state.Mode)
			require.Equal(t, tc.active, state.ActiveTransport)
			require.Zero(t, state.SwitchCount)

			require.NotContains(t, body, "plain-auth-value")
			require.NotContains(t, body, "plain-token-value")
			for _, field := range []string{
				`"credentials"`, `"web_username"`, `"clients"`, `"ports"`,
				`"read_only"`, `"auth_type"`, `"trust_proxy"`,
			} {
				require.NotContains(t, body, field, "success body must not include settings field %s", field)
			}
		})
	}
}

func TestModeAPIInvalidAndSaveFailure(t *testing.T) {
	api := &fakeServerAPI{
		settings: SettingsView{Mode: config.ModeHY2},
		summary:  ConfigSummary{Mode: config.ModeHY2},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		serverModeBody("bogus", "pw"), cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readBody(t, resp), "invalid_mode")
	require.Zero(t, api.modeCalls)

	api.modeErr = errors.New("disk full")
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		serverModeBody("tcp", "pw"), cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	readBody(t, resp)
	require.Equal(t, 1, api.modeCalls)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	settings := readJSON[SettingsView](t, resp)
	require.Equal(t, config.ModeHY2, settings.Mode)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/config/summary", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	summary := readJSON[ConfigSummary](t, resp)
	require.Equal(t, config.ModeHY2, summary.Mode)
}

func TestTransportStateSecretFree(t *testing.T) {
	const (
		plainAuth  = "plain-auth-value"
		plainToken = "plain-token-value"
	)
	api := &fakeServerAPI{
		settings: SettingsView{
			Mode: config.ModeAuto,
			Credentials: CredentialSettingsView{
				Generation:                2,
				CurrentAuthMasked:         plainAuth,
				CurrentControlTokenMasked: plainToken,
			},
		},
		clients: []view.ClientView{{ID: "c1", Name: "home", Online: true, Transport: "tcp"}},
	}
	web, ts := newTestWeb(t, api)
	web.eventInterval = 10 * time.Millisecond
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	settingsBody := readBody(t, resp)
	require.Contains(t, settingsBody, `"mode":"hy2+tcp"`)
	require.NotContains(t, settingsBody, plainAuth)
	require.NotContains(t, settingsBody, plainToken)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/settings", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	pageBody := readBody(t, resp)
	require.NotContains(t, pageBody, plainAuth)
	require.NotContains(t, pageBody, plainToken)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	event, data := readSSEEvent(t, bufio.NewReader(resp.Body))
	require.Equal(t, "snapshot", event)
	require.NotContains(t, data, plainAuth)
	require.NotContains(t, data, plainToken)
	resp.Body.Close()
	cancel()

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/metrics", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	metricsBody := readBody(t, resp)
	require.NotContains(t, metricsBody, plainAuth)
	require.NotContains(t, metricsBody, plainToken)
	require.Contains(t, metricsBody, "hy2tun_server_up 1")
}

func TestModeAPIPageOptionsAndWarning(t *testing.T) {
	api := &fakeServerAPI{settings: SettingsView{Mode: config.ModeAuto}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/settings", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBody(t, resp)
	fragment := selectFragment(t, body, "transport-mode")
	require.Equal(t, 3, strings.Count(fragment, "<option"))
	for _, mode := range []string{"hy2", "tcp", "hy2+tcp"} {
		require.Contains(t, fragment, `value="`+mode+`"`)
	}
	require.Contains(t, body, "切换会断开现有连接")
	require.Contains(t, body, "可能丢失 UDP 数据包")
}

func TestModeAPIPageClientTransport(t *testing.T) {
	api := &fakeServerAPI{
		clients: []view.ClientView{{ID: "c1", Name: "home", Online: true, Transport: "tcp"}},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients/c1/detail", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBody(t, resp)
	require.Contains(t, body, "TCP（UDP over TCP）")
	require.Contains(t, body, `data-client-transport="tcp"`)
}

func TestModeAPIPageUnsupported(t *testing.T) {
	api := modeUnsupportedServerAPI{API: &fakeServerAPI{settings: SettingsView{Mode: config.ModeHY2}}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/settings", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBody(t, resp)
	require.NotContains(t, body, `id="settings-mode-form"`)
	require.NotContains(t, body, `id="transport-mode"`)

	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/mode",
		serverModeBody("tcp", "pw"), cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	require.Contains(t, readBody(t, resp), "not_implemented")
}

func assertServerTransportState(t *testing.T, body string) view.TransportState {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &fields))
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	require.ElementsMatch(t, []string{
		"mode", "active_transport", "switch_reason", "last_switch_at",
		"last_probe_at", "last_probe_error", "switch_count",
	}, keys)
	var state view.TransportState
	require.NoError(t, json.Unmarshal([]byte(body), &state))
	return state
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
