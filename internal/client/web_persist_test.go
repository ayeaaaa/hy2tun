package client

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
)

// TestClientWebCRUDPersistsConfig drives the real client API through the real
// clientui HTTP handler. Each mutation must be visible in the client's
// configuration file with the expected revision bump.
func TestClientWebCRUDPersistsConfig(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	cfg := validTestClientConfig()
	cfg.Client.ID = "web-client"
	cfg.Client.Name = "web-client"
	cfg.Mappings = []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Enabled: true},
	}
	cfg.Web = config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: string(hash),
	}
	require.NoError(t, config.SaveClient(path, cfg))

	loaded, err := config.LoadClient(path)
	require.NoError(t, err)
	cl, err := New(loaded, slog.Default())
	require.NoError(t, err)
	defer cl.Close()
	require.NotNil(t, cl.web)

	ts := httptest.NewServer(cl.web.Handler())
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"},
		"password": {"pw"},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookies := resp.Cookies()
	require.NotEmpty(t, cookies)
	cookie := cookies[0]

	base := loaded.Client.Revision
	do := func(method, target, body string) *http.Response {
		req, err := http.NewRequest(method, ts.URL+target, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "hy2tun")
		req.Header.Set("Origin", ts.URL)
		req.AddCookie(cookie)
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		return resp
	}

	// Add.
	resp = do(http.MethodPost, "/api/mappings",
		`{"id":"web-add","type":"tcp","remote_port":2223,"local_addr":"127.0.0.1:80","note":"added","enabled":true}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	saved, err := config.LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, base+1, saved.Client.Revision)
	mapping, ok := findSavedMapping(saved.Mappings, "web-add")
	require.True(t, ok)
	require.Equal(t, 2223, mapping.RemotePort)
	require.Equal(t, "127.0.0.1:80", mapping.LocalAddr)
	require.Equal(t, "added", mapping.Note)
	require.Equal(t, base+1, cl.Revision())

	// Update.
	resp = do(http.MethodPut, "/api/mappings/web-add",
		`{"id":"web-add","type":"tcp","remote_port":2224,"local_addr":"127.0.0.1:81","note":"updated","enabled":true}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	saved, err = config.LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, base+2, saved.Client.Revision)
	mapping, ok = findSavedMapping(saved.Mappings, "web-add")
	require.True(t, ok)
	require.Equal(t, 2224, mapping.RemotePort)
	require.Equal(t, "127.0.0.1:81", mapping.LocalAddr)
	require.Equal(t, "updated", mapping.Note)
	require.Equal(t, base+2, cl.Revision())

	// Delete.
	resp = do(http.MethodDelete, "/api/mappings/web-add", "")
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	saved, err = config.LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, base+3, saved.Client.Revision)
	_, ok = findSavedMapping(saved.Mappings, "web-add")
	require.False(t, ok, "the deleted mapping must be removed from the file")
	_, ok = findSavedMapping(saved.Mappings, "m1")
	require.True(t, ok, "unrelated mappings must survive")
	require.Equal(t, base+3, cl.Revision())
}

func findSavedMapping(mappings []config.Mapping, id string) (config.Mapping, bool) {
	for _, mapping := range mappings {
		if mapping.ID == id {
			return mapping, true
		}
	}
	return config.Mapping{}, false
}
