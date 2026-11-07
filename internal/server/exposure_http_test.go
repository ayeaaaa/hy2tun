package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/state"
	"hy2tun/internal/testutil"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/serverui"
)

const exposureHTTPPassword = "pw"

// exposureHTTPEnv wires the production server into the production Web console
// so API tests exercise the real error classification instead of a fake.
type exposureHTTPEnv struct {
	server *Server
	web    *httptest.Server
	cookie *http.Cookie
}

func startExposureHTTP(t *testing.T, statePath string) *exposureHTTPEnv {
	t.Helper()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.StateFile = statePath
	srv, stop := startServer(t, cfg)
	t.Cleanup(stop)

	hash, err := bcrypt.GenerateFromPassword([]byte(exposureHTTPPassword), bcrypt.MinCost)
	require.NoError(t, err)
	web, err := serverui.New(srv, config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: string(hash),
	}, []byte("0123456789abcdef0123456789abcdef"), slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = web.Close() })

	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"},
		"password": {exposureHTTPPassword},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())

	return &exposureHTTPEnv{server: srv, web: ts, cookie: resp.Cookies()[0]}
}

func (e *exposureHTTPEnv) putExposure(t *testing.T, clientID, mappingID, body string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPut,
		e.web.URL+"/api/clients/"+url.PathEscape(clientID)+
			"/mappings/"+url.PathEscape(mappingID)+"/exposure",
		strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", e.web.URL)
	req.AddCookie(e.cookie)

	resp, err := e.web.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, payload
}

type exposureConflictBody struct {
	Error               string        `json:"error"`
	Code                string        `json:"code"`
	ActiveExposure      view.Exposure `json:"active_exposure"`
	UnavailableFamilies []string      `json:"unavailable_families"`
	Closed              *bool         `json:"closed"`
}

func decodeExposureConflict(t *testing.T, payload []byte) exposureConflictBody {
	t.Helper()

	var conflict exposureConflictBody
	require.NoError(t, json.Unmarshal(payload, &conflict))
	return conflict
}

func TestExposureHTTPPersistenceFailureIsConflict(t *testing.T) {
	t.Run("live switch", func(t *testing.T) {
		env := startExposureHTTP(t, filepath.Join(t.TempDir(), "state.json"))
		sess := newExposureSession("live")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
		registerExposureMapping(t, env.server.mappings, sess, mapping)

		env.server.mappings.persistExposureFn = func(string, string, view.Exposure) error {
			return errors.New("disk full")
		}
		t.Cleanup(func() { env.server.mappings.persistExposureFn = nil })

		resp, payload := env.putExposure(t, "live", "m1", `{"exposure":"local"}`)
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		conflict := decodeExposureConflict(t, payload)
		require.Equal(t, "exposure_conflict", conflict.Code)
		require.Equal(t, view.ExposurePublic, conflict.ActiveExposure)
		require.NotNil(t, conflict.Closed)
		require.False(t, *conflict.Closed)

		restored := snapshotExposureRuntime(t, env.server.mappings, "live", "m1")
		require.Equal(t, view.ExposurePublic, restored.exposure)
		require.Equal(t, mappingStatusRunning, restored.status)
		requirePublicListeners(t, restored.listeners)
	})

	t.Run("offline desired state", func(t *testing.T) {
		statePath := filepath.Join(t.TempDir(), "state.json")
		env := startExposureHTTP(t, statePath)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:9001", Enabled: true}
		env.server.store.Upsert(state.ClientState{
			ID:        "offline",
			Mappings:  []config.Mapping{mapping},
			Exposures: map[string]string{"m1": "local"},
		})

		// Removing the state directory makes the desired-exposure save fail.
		require.NoError(t, os.RemoveAll(filepath.Dir(statePath)))

		resp, payload := env.putExposure(t, "offline", "m1", `{"exposure":"public"}`)
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		conflict := decodeExposureConflict(t, payload)
		require.Equal(t, "exposure_conflict", conflict.Code)
		require.Equal(t, view.ExposureLocal, conflict.ActiveExposure)
		require.NotNil(t, conflict.Closed)
		require.True(t, *conflict.Closed)
		require.Empty(t, conflict.UnavailableFamilies)
		require.Equal(t, "local", env.server.store.Exposure("offline", "m1"))
	})
}
