package clientui

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/view"
)

func clientFailoverBody() string {
	return `{
		"connect_timeout":"8s",
		"heartbeat_interval":"5s",
		"heartbeat_timeout":"20s",
		"probe_interval":"15s",
		"min_tcp_hold":"30s",
		"recovery_successes":3
	}`
}

func TestFailoverAPICSRFWithoutPassword(t *testing.T) {
	api := &fakeClientAPI{settings: SettingsView{}}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover",
		clientFailoverBody(), cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.failoverCalls, "CSRF failure must not invoke UpdateFailover")

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover",
		clientFailoverBody(), cookie, map[string]string{"X-Requested-With": "hy2tun"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readClientBody(t, resp)
	require.Zero(t, api.failoverCalls, "CSRF failure must not invoke UpdateFailover")

	// A valid session plus CSRF is enough; no current password is required.
	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover",
		clientFailoverBody(), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	state := readClientJSON[FailoverView](t, resp)
	require.Equal(t, "8s", state.ConnectTimeout)
	require.Equal(t, "20s", state.HeartbeatTimeout)
	require.Equal(t, 3, state.RecoverySuccesses)
	require.Equal(t, 1, api.failoverCalls)
	require.Equal(t, "8s", api.failoverValues[0].ConnectTimeout.String())
}

func TestFailoverAPIServerSelection(t *testing.T) {
	us := &fakeClientAPI{settings: SettingsView{Name: "us"}}
	sg := &fakeClientAPI{settings: SettingsView{Name: "sg"}}
	multi := &fakeMultiAPI{
		servers: map[string]*fakeClientAPI{"us": us, "sg": sg},
		order:   []string{"us", "sg"},
	}
	_, ts := newTestClientWeb(t, multi)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover?server=sg",
		clientFailoverBody(), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	readClientBody(t, resp)
	require.Equal(t, 1, sg.failoverCalls)
	require.Zero(t, us.failoverCalls, "another worker must not be updated")

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover?server=missing",
		clientFailoverBody(), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, sg.failoverCalls)
	require.Zero(t, us.failoverCalls)
}

func TestFailoverAPIInvalidAndSaveFailure(t *testing.T) {
	api := &fakeClientAPI{settings: SettingsView{}}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover",
		`{"connect_timeout":"not-a-duration"}`, cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "invalid_failover")
	require.Zero(t, api.failoverCalls)

	api.failoverErr = errors.New("disk full")
	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover",
		clientFailoverBody(), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "internal")
}

func TestFailoverAPIPageAndUnsupported(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{Name: "home", Server: "example.com:443"},
		settings: SettingsView{
			Name: "home",
			Failover: FailoverView{
				ConnectTimeout:    "8s",
				HeartbeatInterval: "5s",
				HeartbeatTimeout:  "20s",
				ProbeInterval:     "15s",
				MinTCPHold:        "30s",
				RecoverySuccesses: 3,
			},
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.Contains(t, body, `id="settings-failover-form"`)
	require.Contains(t, body, `id="failover-connect-timeout"`)
	require.Contains(t, body, `value="8s"`)
	require.Contains(t, body, `id="failover-recovery-successes"`)
	require.Contains(t, body, "保存后立即用于后续判断")

	unsupported := modeUnsupportedAPI{API: &fakeClientAPI{settings: SettingsView{}}}
	_, ts = newTestClientWeb(t, unsupported)
	cookie = loginClientWeb(t, ts)
	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = readClientBody(t, resp)
	require.NotContains(t, body, `id="settings-failover-form"`)

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/failover",
		clientFailoverBody(), cookie, clientModeHeaders(ts))
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	require.True(t, strings.Contains(readClientBody(t, resp), "not_implemented"))
}
