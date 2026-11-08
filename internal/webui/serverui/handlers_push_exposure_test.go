package serverui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

func exposurePushClient(mappings ...view.MappingView) []view.ClientView {
	return []view.ClientView{{
		ID:       "c1",
		Name:     "home",
		Online:   true,
		Revision: 0,
		Mappings: mappings,
	}}
}

func pushExposureRequest(t *testing.T, ts *httptest.Server, cookie *http.Cookie, body string) *http.Response {
	t.Helper()
	return doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", body, cookie, csrfHeaders(ts.URL))
}

func TestServerWebAddMappingLocalExposureUsesDisabledFirstPush(t *testing.T) {
	api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{
		clients: exposurePushClient(),
	}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":0,"mappings":[` +
		`{"id":"m1","type":"tcp","remote_port":25022,"local_addr":"127.0.0.1:22","enabled":true}],` +
		`"exposures":{"m1":"local"}}`
	resp := pushExposureRequest(t, ts, cookie, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result pushResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	resp.Body.Close()
	require.Equal(t, int64(2), result.Revision)

	require.Len(t, api.pushCalls, 2, "local add must push disabled, set exposure, then push enabled")
	require.Equal(t, int64(0), api.pushCalls[0].base)
	require.False(t, api.pushCalls[0].mappings[0].Enabled, "the first push must not bind a public listener")
	require.Equal(t, int64(1), api.pushCalls[1].base)
	require.True(t, api.pushCalls[1].mappings[0].Enabled)
	require.Equal(t, 1, api.callCount())
	clientID, mappingID, exposure := api.lastCall()
	require.Equal(t, "c1", clientID)
	require.Equal(t, "m1", mappingID)
	require.Equal(t, view.ExposureLocal, exposure)
}

func TestServerWebAddMappingLocalDisabledPushesOnce(t *testing.T) {
	api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{
		clients: exposurePushClient(),
	}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":0,"mappings":[` +
		`{"id":"m1","type":"udp","remote_port":25053,"local_addr":"127.0.0.1:51820","enabled":false}],` +
		`"exposures":{"m1":"local"}}`
	resp := pushExposureRequest(t, ts, cookie, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	require.Len(t, api.pushCalls, 1)
	require.False(t, api.pushCalls[0].mappings[0].Enabled)
	require.Equal(t, 1, api.callCount())
	_, mappingID, exposure := api.lastCall()
	require.Equal(t, "m1", mappingID)
	require.Equal(t, view.ExposureLocal, exposure)
}

func TestServerWebEditMappingExposureAfterPush(t *testing.T) {
	api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{
		clients: exposurePushClient(view.MappingView{
			Mapping: config.Mapping{
				ID: "m1", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:22", Enabled: true,
			},
			Status:   "running",
			Exposure: view.ExposurePublic,
		}),
	}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":0,"mappings":[` +
		`{"id":"m1","type":"tcp","remote_port":25022,"local_addr":"127.0.0.1:22","enabled":true}],` +
		`"exposures":{"m1":"local"}}`
	resp := pushExposureRequest(t, ts, cookie, body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	require.Len(t, api.pushCalls, 1)
	require.Equal(t, 1, api.callCount())
	_, mappingID, exposure := api.lastCall()
	require.Equal(t, "m1", mappingID)
	require.Equal(t, view.ExposureLocal, exposure)
}

func TestServerWebPushRejectsInvalidExposure(t *testing.T) {
	api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{
		clients: exposurePushClient(),
	}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":0,"mappings":[` +
		`{"id":"m1","type":"tcp","remote_port":25022,"local_addr":"127.0.0.1:22","enabled":true}],` +
		`"exposures":{"m1":"bogus"}}`
	resp := pushExposureRequest(t, ts, cookie, body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.pushes)
	require.Zero(t, api.callCount())

	body = `{"base_revision":0,"mappings":[` +
		`{"id":"m1","type":"tcp","remote_port":25022,"local_addr":"127.0.0.1:22","enabled":true}],` +
		`"exposures":{"other":"local"}}`
	resp = pushExposureRequest(t, ts, cookie, body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.pushes)
}

func TestServerWebPushExposureRequiresExposureAPI(t *testing.T) {
	api := &fakeServerAPI{clients: exposurePushClient()}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":0,"mappings":[` +
		`{"id":"m1","type":"tcp","remote_port":25022,"local_addr":"127.0.0.1:22","enabled":true}],` +
		`"exposures":{"m1":"local"}}`
	resp := pushExposureRequest(t, ts, cookie, body)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.pushes)
}

func TestServerWebMappingDrawerHasExposureField(t *testing.T) {
	api := &fakeServerAPI{clients: exposurePushClient()}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients?id=c1", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBody(t, resp)
	require.Contains(t, body, `id="mapping-exposure"`)
	require.Contains(t, body, `value="public"`)
	require.Contains(t, body, `value="local"`)
	require.Contains(t, body, "仅本机")
}
