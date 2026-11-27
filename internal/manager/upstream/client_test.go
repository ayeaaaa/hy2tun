package upstream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/manager/upstream"
	"hy2tun/internal/view"
)

const (
	testUsername = "admin"
	testPassword = "upstream-password"
)

type recordedRequest struct {
	Method      string
	Path        string
	Origin      string
	CSRF        string
	ContentType string
	Session     string
	Body        []byte
}

type requestLog struct {
	mu    sync.Mutex
	items []recordedRequest
}

func (l *requestLog) record(r *http.Request) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	session := ""
	if cookie, err := r.Cookie("hy2tun_session"); err == nil {
		session = cookie.Value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items = append(l.items, recordedRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		Origin:      r.Header.Get("Origin"),
		CSRF:        r.Header.Get("X-Requested-With"),
		ContentType: r.Header.Get("Content-Type"),
		Session:     session,
		Body:        body,
	})
}

func (l *requestLog) snapshot() []recordedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]recordedRequest, len(l.items))
	copy(out, l.items)
	return out
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func loginRoute(log *requestLog, cookie string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		http.SetCookie(w, &http.Cookie{Name: "hy2tun_session", Value: cookie, Path: "/"})
		w.WriteHeader(http.StatusNoContent)
	}
}

func newTestClient(t *testing.T, baseURL, password string, skipVerify bool) *upstream.Client {
	t.Helper()
	client, err := upstream.NewClient(upstream.ServerRecord{
		ID:               "srv_test",
		Name:             "test",
		BaseURL:          baseURL,
		UpstreamUsername: testUsername,
		TLSSkipVerify:    skipVerify,
	}, []byte(password))
	require.NoError(t, err)
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func mustRequest(t *testing.T, requests []recordedRequest, index int) recordedRequest {
	t.Helper()
	require.Greater(t, len(requests), index)
	return requests[index]
}

func TestLoginJoinsBasePathAndRetainsCookie(t *testing.T) {
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hy2tund/api/login", loginRoute(log, "cookie-1"))
	mux.HandleFunc("GET /hy2tund/api/overview", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{
			"online_clients": 2,
			"active_mappings": 3,
			"tx_bytes": 1024,
			"rx_bytes": 2048,
			"listen": "0.0.0.0:443",
			"uptime_seconds": 7200
		}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL+"/hy2tund/", testPassword, false)
	require.NoError(t, client.Login(context.Background()))

	overview, err := client.Overview(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, overview.OnlineClients)
	require.Equal(t, 3, overview.ActiveMappings)
	require.Equal(t, uint64(1024), overview.TxBytes)
	require.Equal(t, uint64(2048), overview.RxBytes)
	require.Equal(t, "0.0.0.0:443", overview.Listen)
	require.Equal(t, 2*time.Hour, overview.Uptime)

	requests := log.snapshot()
	require.Len(t, requests, 2)

	login := mustRequest(t, requests, 0)
	require.Equal(t, http.MethodPost, login.Method)
	require.Equal(t, "/hy2tund/api/login", login.Path)
	form, err := url.ParseQuery(string(login.Body))
	require.NoError(t, err)
	require.Equal(t, testUsername, form.Get("username"))
	require.Equal(t, testPassword, form.Get("password"))

	read := mustRequest(t, requests, 1)
	require.Equal(t, http.MethodGet, read.Method)
	require.Equal(t, "/hy2tund/api/overview", read.Path)
	require.Equal(t, "cookie-1", read.Session, "login cookie must be retained")
}

func TestReadsDecodeUpstreamContracts(t *testing.T) {
	log := &requestLog{}
	lastSeen := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	clientView := view.ClientView{
		ID:       "c1",
		Name:     "home",
		Version:  "v1.7.4",
		Revision: 7,
		Online:   true,
		LastSeen: lastSeen,
		Mappings: []view.MappingView{{
			Mapping: config.Mapping{
				ID:         "m1",
				Type:       "tcp",
				RemotePort: 2222,
				LocalAddr:  "127.0.0.1:22",
				Note:       "ssh",
				Enabled:    true,
			},
			Status:              "running",
			AssignedPort:        2222,
			Exposure:            view.ExposureLocal,
			UnavailableFamilies: []string{"ipv6"},
			TxBytes:             11,
			RxBytes:             12,
		}},
		Transport:            "hy2",
		CredentialGeneration: 2,
		CredentialSynced:     true,
		CredentialCapable:    true,
	}
	clientJSON, err := json.Marshal(clientView)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
	mux.HandleFunc("GET /api/clients", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, "["+string(clientJSON)+"]")
	})
	mux.HandleFunc("GET /api/clients/c1", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, string(clientJSON))
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{
			"web_username": "admin",
			"web_password_configured": true,
			"auth_type": "password",
			"trust_proxy": false,
			"mode": "hy2",
			"ports": {
				"allow": ["443"],
				"deny": ["22"],
				"auto_range": "20000-30000",
				"offline_reserve_seconds": 30
			},
			"credentials": {
				"generation": 2,
				"previous_generation": 1,
				"current_auth_masked": "********",
				"current_control_token_masked": "********",
				"previous_auth_masked": "********",
				"previous_control_token_masked": "********",
				"rotatable": true
			},
			"clients": [{
				"client_id": "c1",
				"name": "home",
				"online": true,
				"generation": 2,
				"status": "synced",
				"last_seen": "2026-11-27T10:00:00Z"
			}],
			"read_only": [{"label": "listen", "value": "0.0.0.0:443"}]
		}`)
	})
	mux.HandleFunc("GET /api/config/summary", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{
			"config_path": "/etc/hy2tund/config.yaml",
			"listen": "0.0.0.0:443",
			"listen_host": "0.0.0.0",
			"web_listen": "127.0.0.1:8443",
			"web_username": "admin",
			"web_tls": true,
			"tls": true,
			"state_file": "/var/lib/hy2tund/state.json",
			"ports_allow": ["443"],
			"ports_deny": ["22"],
			"ports_auto_range": "20000-30000",
			"offline_reserve_seconds": 45,
			"trust_proxy": false,
			"log_level": "info",
			"mode": "hy2",
			"version": "v1.7.4",
			"control_token_configured": true,
			"web_password_configured": true
		}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL, testPassword, false)
	require.NoError(t, client.Login(context.Background()))

	clients, err := client.Clients(context.Background())
	require.NoError(t, err)
	require.Len(t, clients, 1)
	require.Equal(t, "c1", clients[0].ID)
	require.Equal(t, int64(7), clients[0].Revision)
	require.True(t, clients[0].Online)
	require.Equal(t, lastSeen, clients[0].LastSeen)
	require.True(t, clients[0].CredentialSynced)
	require.True(t, clients[0].CredentialCapable)
	require.Len(t, clients[0].Mappings, 1)
	mapping := clients[0].Mappings[0]
	require.Equal(t, "m1", mapping.ID)
	require.Equal(t, "ssh", mapping.Note)
	require.Equal(t, 2222, mapping.AssignedPort)
	require.Equal(t, "running", mapping.Status)
	require.Equal(t, view.ExposureLocal, mapping.Exposure)
	require.Equal(t, []string{"ipv6"}, mapping.UnavailableFamilies)

	detail, err := client.Client(context.Background(), "c1")
	require.NoError(t, err)
	require.Equal(t, "home", detail.Name)
	require.Equal(t, "v1.7.4", detail.Version)
	require.Len(t, detail.Mappings, 1)

	settings, err := client.Settings(context.Background())
	require.NoError(t, err)
	require.Equal(t, "admin", settings.WebUsername)
	require.True(t, settings.WebPasswordConfigured)
	require.Equal(t, config.ModeHY2, settings.Mode)
	require.False(t, settings.TrustProxy)
	require.Equal(t, []string{"443"}, settings.Ports.Allow)
	require.Equal(t, []string{"22"}, settings.Ports.Deny)
	require.Equal(t, "20000-30000", settings.Ports.AutoRange)
	require.Equal(t, int64(30), settings.Ports.OfflineReserveSeconds)
	require.Equal(t, 30*time.Second, settings.Ports.OfflineReserve)
	require.Equal(t, int64(2), settings.Credentials.Generation)
	require.Equal(t, int64(1), settings.Credentials.PreviousGeneration)
	require.True(t, settings.Credentials.Rotatable)
	require.Len(t, settings.Clients, 1)
	require.Equal(t, "c1", settings.Clients[0].ClientID)
	require.Equal(t, int64(2), settings.Clients[0].Generation)
	require.Len(t, settings.ReadOnly, 1)
	require.Equal(t, "listen", settings.ReadOnly[0].Label)

	summary, err := client.ConfigSummary(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v1.7.4", summary.Version)
	require.Equal(t, config.ModeHY2, summary.Mode)
	require.Equal(t, "0.0.0.0:443", summary.Listen)
	require.Equal(t, "127.0.0.1:8443", summary.WebListen)
	require.True(t, summary.WebTLS)
	require.True(t, summary.ControlTokenConfigured)
	require.Equal(t, []string{"443"}, summary.PortsAllow)
	require.Equal(t, int64(45), summary.OfflineReserveSeconds)
	require.Equal(t, 45*time.Second, summary.OfflineReserve)
}

func TestUnauthorizedTriggersOneRelogin(t *testing.T) {
	t.Run("retry succeeds once", func(t *testing.T) {
		var mu sync.Mutex
		loginCount, clientsCount := 0, 0
		sessions := []string{}

		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			loginCount++
			count := loginCount
			mu.Unlock()
			cookie := fmt.Sprintf("cookie-%d", count)
			mu.Lock()
			sessions = append(sessions, cookie)
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "hy2tun_session", Value: cookie, Path: "/"})
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("GET /api/clients", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			clientsCount++
			count := clientsCount
			mu.Unlock()
			if count == 1 {
				writeJSON(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
				return
			}
			cookie, err := r.Cookie("hy2tun_session")
			if err != nil || cookie.Value != "cookie-2" {
				writeJSON(w, http.StatusUnauthorized, `{"error":"stale cookie"}`)
				return
			}
			writeJSON(w, http.StatusOK, `[]`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		client := newTestClient(t, ts.URL, testPassword, false)
		require.NoError(t, client.Login(context.Background()))

		clients, err := client.Clients(context.Background())
		require.NoError(t, err)
		require.Empty(t, clients)

		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, 2, loginCount, "must re-login exactly once")
		require.Equal(t, 2, clientsCount, "must retry exactly once")
		require.Equal(t, []string{"cookie-1", "cookie-2"}, sessions)
	})

	t.Run("repeated 401 stops after one retry", func(t *testing.T) {
		var mu sync.Mutex
		loginCount, clientsCount := 0, 0

		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			loginCount++
			mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "hy2tun_session", Value: "cookie", Path: "/"})
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("GET /api/clients", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			clientsCount++
			mu.Unlock()
			writeJSON(w, http.StatusUnauthorized, `{"error":"unauthorized"}`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		client := newTestClient(t, ts.URL, testPassword, false)
		require.NoError(t, client.Login(context.Background()))

		_, err := client.Clients(context.Background())
		var apiErr *upstream.APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, http.StatusUnauthorized, apiErr.Status)

		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, 2, loginCount)
		require.Equal(t, 2, clientsCount, "401 retry must not loop")
	})
}

func TestMutationHeadersAndOrigin(t *testing.T) {
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hy2tund/api/login", loginRoute(log, "cookie-1"))
	mux.HandleFunc("PUT /hy2tund/api/settings/web", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL+"/hy2tund", testPassword, false)
	require.NoError(t, client.Login(context.Background()))
	require.NoError(t, client.UpdateWebAdmin(context.Background(), "admin2", "current-secret", "new-secret"))

	requests := log.snapshot()
	require.Len(t, requests, 2)
	mutation := mustRequest(t, requests, 1)
	require.Equal(t, http.MethodPut, mutation.Method)
	require.Equal(t, "/hy2tund/api/settings/web", mutation.Path)
	require.Equal(t, ts.URL, mutation.Origin, "Origin must be scheme+host without the base path")
	require.Equal(t, "hy2tun", mutation.CSRF)
	require.Equal(t, "application/json", mutation.ContentType)
	require.Equal(t, "cookie-1", mutation.Session)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(mutation.Body, &payload))
	require.Equal(t, "admin2", payload["username"])
	require.Equal(t, "current-secret", payload["current_password"])
	require.Equal(t, "new-secret", payload["new_password"])
	require.Equal(t, "new-secret", payload["confirm_password"])
}

func TestPushMappingsSendsFullListAndBaseRevision(t *testing.T) {
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
	mux.HandleFunc("PUT /api/clients/c1/mappings", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{"revision":8}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL, testPassword, false)
	require.NoError(t, client.Login(context.Background()))

	mappings := []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Note: "ssh", Enabled: true},
		{ID: "m2", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:51820", Note: "wg", Enabled: false},
	}
	require.NoError(t, client.PushMappings(context.Background(), "c1", 7, mappings, nil))

	requests := log.snapshot()
	require.Len(t, requests, 2)
	push := mustRequest(t, requests, 1)
	require.Equal(t, http.MethodPut, push.Method)
	require.Equal(t, "/api/clients/c1/mappings", push.Path)
	require.Equal(t, ts.URL, push.Origin)
	require.Equal(t, "hy2tun", push.CSRF)
	require.NotContains(t, string(push.Body), `"exposures"`)

	var payload struct {
		BaseRevision int64                    `json:"base_revision"`
		Mappings     []config.Mapping         `json:"mappings"`
		Exposures    map[string]view.Exposure `json:"exposures"`
	}
	require.NoError(t, json.Unmarshal(push.Body, &payload))
	require.Equal(t, int64(7), payload.BaseRevision)
	require.Equal(t, mappings, payload.Mappings)
	require.Nil(t, payload.Exposures)
}

func TestPushMappingsIncludesLocalExposures(t *testing.T) {
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
	mux.HandleFunc("PUT /api/clients/c1/mappings", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{"revision":8}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL, testPassword, false)
	require.NoError(t, client.Login(context.Background()))

	mappings := []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Enabled: true},
		{ID: "m2", Type: "tcp", RemotePort: 8080, LocalAddr: "127.0.0.1:80", Enabled: true},
	}
	exposures := map[string]view.Exposure{"m2": view.ExposureLocal}
	require.NoError(t, client.PushMappings(context.Background(), "c1", 7, mappings, exposures))

	requests := log.snapshot()
	require.Len(t, requests, 2)
	push := mustRequest(t, requests, 1)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(push.Body, &raw))
	require.Contains(t, raw, "exposures")
	var gotExposures map[string]view.Exposure
	require.NoError(t, json.Unmarshal(raw["exposures"], &gotExposures))
	require.Equal(t, exposures, gotExposures)

	var gotMappings []config.Mapping
	require.NoError(t, json.Unmarshal(raw["mappings"], &gotMappings))
	require.Equal(t, mappings, gotMappings)
	var baseRevision int64
	require.NoError(t, json.Unmarshal(raw["base_revision"], &baseRevision))
	require.Equal(t, int64(7), baseRevision)
}

func TestPushMappingsPreservesConflictCodes(t *testing.T) {
	codes := []string{
		"conflict",
		"client_offline",
		"client_not_ready",
		"push_rejected",
		"partial_apply",
		"exposure_conflict",
	}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			var mu sync.Mutex
			pushes := 0
			mux := http.NewServeMux()
			mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
				http.SetCookie(w, &http.Cookie{Name: "hy2tun_session", Value: "cookie-1", Path: "/"})
				w.WriteHeader(http.StatusNoContent)
			})
			mux.HandleFunc("PUT /api/clients/c1/mappings", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				pushes++
				mu.Unlock()
				writeJSON(w, http.StatusConflict,
					fmt.Sprintf(`{"error":"push failed: %s","code":%q,"revision":9,"partial":true}`, code, code))
			})
			ts := httptest.NewServer(mux)
			t.Cleanup(ts.Close)

			client := newTestClient(t, ts.URL, testPassword, false)
			require.NoError(t, client.Login(context.Background()))

			err := client.PushMappings(context.Background(), "c1", 7, []config.Mapping{
				{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Enabled: true},
			}, nil)
			var apiErr *upstream.APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, http.StatusConflict, apiErr.Status)
			require.Equal(t, code, apiErr.Code, "server code must be preserved")
			require.Contains(t, apiErr.Message, code)
			require.NotContains(t, err.Error(), testPassword)

			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, 1, pushes, "409 must not be retried")
		})
	}
}

func TestExposureAndSettingsMutations(t *testing.T) {
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
	mux.HandleFunc("PUT /api/clients/c1/mappings/m1/exposure", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{}`)
	})
	mux.HandleFunc("PUT /api/settings/mode", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{"mode":"tcp","active_transport":"tcp"}`)
	})
	mux.HandleFunc("PUT /api/settings/ports", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{}`)
	})
	mux.HandleFunc("PUT /api/settings/proxy", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{}`)
	})
	mux.HandleFunc("PUT /api/settings/credentials", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		writeJSON(w, http.StatusOK, `{
			"generation": 3,
			"results": [{"ClientID":"c1","Name":"home","Generation":3,"Status":"synced"}]
		}`)
	})
	mux.HandleFunc("POST /api/settings/credentials/revoke", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL, testPassword, false)
	ctx := context.Background()
	require.NoError(t, client.Login(ctx))

	require.NoError(t, client.SetExposure(ctx, "c1", "m1", view.ExposureLocal))
	require.NoError(t, client.UpdateMode(ctx, config.ModeTCP))
	require.NoError(t, client.UpdatePorts(ctx, []string{"443"}, []string{"22"}, "20000-30000", 45*time.Second))
	require.NoError(t, client.SetTrustProxy(ctx, true))
	rotate, err := client.RotateCredentials(ctx, "new-auth", "new-control-token", "auto")
	require.NoError(t, err)
	require.Equal(t, int64(3), rotate.Generation)
	require.Len(t, rotate.Results, 1)
	require.Equal(t, "c1", rotate.Results[0].ClientID)
	require.Equal(t, "synced", rotate.Results[0].Status)
	require.NoError(t, client.RevokeCredentials(ctx))

	requests := log.snapshot()
	require.Len(t, requests, 7)
	byPath := make(map[string]recordedRequest, len(requests))
	for _, request := range requests[1:] {
		byPath[request.Path] = request
		require.Equal(t, ts.URL, request.Origin)
		require.Equal(t, "hy2tun", request.CSRF)
	}

	require.JSONEq(t, `{"exposure":"local"}`, string(byPath["/api/clients/c1/mappings/m1/exposure"].Body))
	require.JSONEq(t, `{"mode":"tcp"}`, string(byPath["/api/settings/mode"].Body))
	require.JSONEq(t, `{
		"allow": ["443"],
		"deny": ["22"],
		"auto_range": "20000-30000",
		"offline_reserve_seconds": 45
	}`, string(byPath["/api/settings/ports"].Body))
	require.JSONEq(t, `{"trust_proxy":true}`, string(byPath["/api/settings/proxy"].Body))
	require.JSONEq(t, `{
		"auth": "new-auth",
		"control_token": "new-control-token",
		"sync_mode": "auto"
	}`, string(byPath["/api/settings/credentials"].Body))
	require.JSONEq(t, `{}`, string(byPath["/api/settings/credentials/revoke"].Body))
}

func TestAPIErrorRetryAfter(t *testing.T) {
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
	mux.HandleFunc("GET /api/clients", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusTooManyRequests, `{"error":"slow down","code":"rate_limited"}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL, testPassword, false)
	require.NoError(t, client.Login(context.Background()))

	_, err := client.Clients(context.Background())
	var apiErr *upstream.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusTooManyRequests, apiErr.Status)
	require.Equal(t, "rate_limited", apiErr.Code)
	require.Equal(t, "slow down", apiErr.Message)
	require.Equal(t, 30*time.Second, apiErr.RetryAfter)
}

func TestAPIErrorRedactsSecrets(t *testing.T) {
	t.Run("login password", func(t *testing.T) {
		const secret = "s3cr3t-login-password"
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusUnauthorized, `{"error":"bad credentials for `+secret+`"}`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		client := newTestClient(t, ts.URL, secret, false)
		err := client.Login(context.Background())
		require.Error(t, err)
		require.NotContains(t, err.Error(), secret)
		require.Contains(t, err.Error(), "[REDACTED]")
	})

	t.Run("web admin passwords", func(t *testing.T) {
		const current = "s3cr3t-current-password"
		const next = "s3cr3t-new-password"
		log := &requestLog{}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
		mux.HandleFunc("PUT /api/settings/web", func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			writeJSON(w, http.StatusForbidden,
				`{"error":"rejected `+current+` and `+next+`","code":"invalid_password"}`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		client := newTestClient(t, ts.URL, testPassword, false)
		require.NoError(t, client.Login(context.Background()))

		err := client.UpdateWebAdmin(context.Background(), "admin2", current, next)
		var apiErr *upstream.APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, http.StatusForbidden, apiErr.Status)
		require.Equal(t, "invalid_password", apiErr.Code)
		require.NotContains(t, err.Error(), current)
		require.NotContains(t, err.Error(), next)
	})

	t.Run("rotation tokens", func(t *testing.T) {
		const auth = "s3cr3t-auth-token"
		const control = "s3cr3t-control-token"
		log := &requestLog{}
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
		mux.HandleFunc("PUT /api/settings/credentials", func(w http.ResponseWriter, r *http.Request) {
			log.record(r)
			writeJSON(w, http.StatusBadRequest,
				`{"error":"rejected `+auth+` and `+control+`","code":"invalid_credentials"}`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		client := newTestClient(t, ts.URL, testPassword, false)
		require.NoError(t, client.Login(context.Background()))

		_, err := client.RotateCredentials(context.Background(), auth, control, "manual")
		var apiErr *upstream.APIError
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, "invalid_credentials", apiErr.Code)
		require.NotContains(t, err.Error(), auth)
		require.NotContains(t, err.Error(), control)
	})
}

func TestTLSVerificationFollowsServerRecord(t *testing.T) {
	log := &requestLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", loginRoute(log, "cookie-1"))
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)

	verified := newTestClient(t, ts.URL, testPassword, false)
	require.Error(t, verified.Login(context.Background()))

	insecure := newTestClient(t, ts.URL, testPassword, true)
	require.NoError(t, insecure.Login(context.Background()))
}

func TestNewClientRejectsInvalidBaseURLs(t *testing.T) {
	cases := []string{
		"",
		"ftp://example.net",
		"https:///missing-host",
		"https://user:url-secret@example.net",
		"https://example.net/hy2tund?query=1",
		"https://example.net/hy2tund#fragment",
	}
	for _, baseURL := range cases {
		t.Run(baseURL, func(t *testing.T) {
			client, err := upstream.NewClient(upstream.ServerRecord{
				BaseURL:          baseURL,
				UpstreamUsername: testUsername,
			}, []byte(testPassword))
			require.Error(t, err)
			require.Nil(t, client)
			require.NotContains(t, err.Error(), "url-secret")
		})
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	client := newTestClient(t, origin.URL, testPassword, false)
	err := client.Login(context.Background())
	var apiErr *upstream.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusTemporaryRedirect, apiErr.Status)
	require.Zero(t, targetHits.Load(), "credentials must not be replayed to a redirect target")
}

func TestMutationForbiddenDoesNotRetry(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "hy2tun_session", Value: "cookie-1", Path: "/"})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/settings/proxy", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		writeJSON(w, http.StatusForbidden, `{"error":"forbidden"}`)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	client := newTestClient(t, ts.URL, testPassword, false)
	require.NoError(t, client.Login(context.Background()))

	err := client.SetTrustProxy(context.Background(), true)
	var apiErr *upstream.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusForbidden, apiErr.Status)
	require.NotEmpty(t, strings.TrimSpace(err.Error()))

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, attempts, "403 must not be retried")
}
