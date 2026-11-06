package serverui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/view"
)

// fakeExposureAPI adds the optional exposure mutation to the shared fake API.
// The real serverui tests cannot import internal/server because that package
// imports serverui; the marker interfaces below mirror its typed errors.
type fakeExposureAPI struct {
	*fakeServerAPI

	mu        sync.Mutex
	calls     int
	clientID  string
	mappingID string
	exposure  view.Exposure
	result    view.MappingView
	err       error
}

func (f *fakeExposureAPI) SetMappingExposure(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.clientID = clientID
	f.mappingID = mappingID
	f.exposure = exposure
	if f.err != nil {
		return view.MappingView{}, f.err
	}
	result := f.result
	if result.ID == "" {
		result.ID = mappingID
	}
	result.Exposure = exposure
	return result, nil
}

func (f *fakeExposureAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeExposureAPI) lastCall() (string, string, view.Exposure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clientID, f.mappingID, f.exposure
}

// fakeMappingNotFoundError mirrors server.MappingNotFoundError's marker.
type fakeMappingNotFoundError struct {
	clientID  string
	mappingID string
}

func (e *fakeMappingNotFoundError) Error() string {
	return fmt.Sprintf("mapping not found: client %q mapping %q", e.clientID, e.mappingID)
}

func (e *fakeMappingNotFoundError) MappingNotFound() bool { return true }

// fakeInvalidExposureError mirrors server.ExposureInvalidError's marker.
type fakeInvalidExposureError struct {
	value view.Exposure
}

func (e *fakeInvalidExposureError) Error() string {
	return fmt.Sprintf("invalid mapping exposure: %q", e.value)
}

func (e *fakeInvalidExposureError) InvalidExposure() bool { return true }

// fakeExposureSwitchError mirrors server.ExposureSwitchError, including the
// persistence flag that must be classified as 500 rather than 409 and the
// churn case that wraps control.ErrClientOffline but still knows the active
// state.
type fakeExposureSwitchError struct {
	active      view.Exposure
	unavailable []string
	closed      bool
	persistence bool
	err         error
}

func (e *fakeExposureSwitchError) Error() string {
	return "mapping exposure switch failed"
}

func (e *fakeExposureSwitchError) Unwrap() error { return e.err }

func (e *fakeExposureSwitchError) ExposureConflict() bool { return !e.persistence }

func (e *fakeExposureSwitchError) ActiveExposure() view.Exposure {
	if e.active == "" {
		return view.ExposurePublic
	}
	return e.active
}

func (e *fakeExposureSwitchError) ActiveUnavailableFamilies() []string {
	return append([]string(nil), e.unavailable...)
}

func (e *fakeExposureSwitchError) MappingClosed() bool { return e.closed }

type exposureErrorBody struct {
	Error               string        `json:"error"`
	Code                string        `json:"code"`
	ActiveExposure      view.Exposure `json:"active_exposure"`
	UnavailableFamilies []string      `json:"unavailable_families"`
	Closed              *bool         `json:"closed"`
}

func exposureURL(ts *httptest.Server, clientID, mappingID string) string {
	return ts.URL + "/api/clients/" + url.PathEscape(clientID) +
		"/mappings/" + url.PathEscape(mappingID) + "/exposure"
}

func readExposureSuccess(t *testing.T, resp *http.Response) (view.MappingView, map[string]json.RawMessage) {
	t.Helper()
	body := readBody(t, resp)
	var mapping view.MappingView
	require.NoError(t, json.Unmarshal([]byte(body), &mapping))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	return mapping, raw
}

func readExposureError(t *testing.T, resp *http.Response) (exposureErrorBody, map[string]json.RawMessage) {
	t.Helper()
	body := readBody(t, resp)
	var payload exposureErrorBody
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	return payload, raw
}

func readErrorPayload(t *testing.T, resp *http.Response) (map[string]string, map[string]json.RawMessage) {
	t.Helper()
	body := readBody(t, resp)
	var payload map[string]string
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	return payload, raw
}

func TestExposureAPICSRFAndPassword(t *testing.T) {
	api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)
	target := exposureURL(ts, "c1", "m1")
	body := `{"exposure":"local"}`

	// Session authentication is required even with a valid CSRF header.
	resp := doRequest(t, ts.Client(), http.MethodPut, target, body, nil, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.callCount())

	// The CSRF header is required and the API must not be reached without it.
	resp = doRequest(t, ts.Client(), http.MethodPut, target, body, cookie,
		map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.callCount(), "CSRF failure must not call SetMappingExposure")

	// A session plus CSRF is enough: no current password is part of the body.
	resp = doRequest(t, ts.Client(), http.MethodPut, target, body, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	readBody(t, resp)
	require.Equal(t, 1, api.callCount())

	clientID, mappingID, exposure := api.lastCall()
	require.Equal(t, "c1", clientID)
	require.Equal(t, "m1", mappingID)
	require.Equal(t, view.ExposureLocal, exposure)
}

func TestExposureAPIUnsupported(t *testing.T) {
	// fakeServerAPI deliberately does not implement ExposureAPI.
	_, ts := newTestWeb(t, &fakeServerAPI{})
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
		`{"exposure":"local"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	require.Contains(t, readBody(t, resp), "not_implemented")
}

func TestExposureAPINotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{
			name: "client",
			err:  control.ErrClientNotFound,
			code: "client_not_found",
		},
		{
			name: "mapping",
			err:  &fakeMappingNotFoundError{clientID: "c1", mappingID: "missing"},
			code: "mapping_not_found",
		},
		{
			name: "wrapped mapping",
			err:  fmt.Errorf("lookup mapping: %w", &fakeMappingNotFoundError{clientID: "c1", mappingID: "missing"}),
			code: "mapping_not_found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}, err: tc.err}
			_, ts := newTestWeb(t, api)
			cookie := login(t, ts)

			resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "missing"),
				`{"exposure":"local"}`, cookie, csrfHeaders(ts.URL))
			require.Equal(t, http.StatusNotFound, resp.StatusCode)
			payload := readJSON[map[string]string](t, resp)
			require.Equal(t, tc.code, payload["code"])
			require.Equal(t, 1, api.callCount())
		})
	}
}

func TestExposureAPIInvalidAndConflict(t *testing.T) {
	t.Run("invalid value is rejected before the API", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"bogus"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "invalid_exposure", payload["code"])
		require.Zero(t, api.callCount())
	})

	t.Run("missing value is rejected before the API", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "invalid_exposure", payload["code"])
		require.Zero(t, api.callCount())
	})

	t.Run("malformed body", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "invalid_body", payload["code"])
		require.Zero(t, api.callCount())
	})

	t.Run("trailing content", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"local"}{"extra":true}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "invalid_body", payload["code"])
		require.Zero(t, api.callCount())
	})

	t.Run("oversized body", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		body := `{"exposure":"local"}` + strings.Repeat(" ", 70<<10)
		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			body, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "invalid_body", payload["code"])
		require.Zero(t, api.callCount())
	})

	t.Run("server rejects the exposure", func(t *testing.T) {
		api := &fakeExposureAPI{
			fakeServerAPI: &fakeServerAPI{},
			err:           &fakeInvalidExposureError{value: "bogus"},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"local"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "invalid_exposure", payload["code"])
		require.Equal(t, 1, api.callCount())
	})

	t.Run("conflict reports the active exposure", func(t *testing.T) {
		api := &fakeExposureAPI{
			fakeServerAPI: &fakeServerAPI{},
			err: &fakeExposureSwitchError{
				active:      view.ExposurePublic,
				unavailable: []string{"ipv6"},
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"local"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		payload, raw := readExposureError(t, resp)
		require.Equal(t, "exposure_conflict", payload.Code)
		require.Equal(t, view.ExposurePublic, payload.ActiveExposure)
		require.Equal(t, []string{"ipv6"}, payload.UnavailableFamilies)
		require.NotNil(t, payload.Closed)
		require.False(t, *payload.Closed)
		require.JSONEq(t, `"public"`, string(raw["active_exposure"]))
		require.JSONEq(t, `["ipv6"]`, string(raw["unavailable_families"]))
		require.JSONEq(t, `false`, string(raw["closed"]))
		require.Equal(t, 1, api.callCount())
	})

	t.Run("closed conflict says the mapping is closed", func(t *testing.T) {
		api := &fakeExposureAPI{
			fakeServerAPI: &fakeServerAPI{},
			err: &fakeExposureSwitchError{
				active: view.ExposureLocal,
				closed: true,
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"public"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		payload, raw := readExposureError(t, resp)
		require.Equal(t, "exposure_conflict", payload.Code)
		require.Equal(t, view.ExposureLocal, payload.ActiveExposure)
		require.NotNil(t, payload.Closed)
		require.True(t, *payload.Closed)
		require.JSONEq(t, `"local"`, string(raw["active_exposure"]))
		require.JSONEq(t, `[]`, string(raw["unavailable_families"]),
			"the conflict DTO must emit an empty array, not null")
		require.JSONEq(t, `true`, string(raw["closed"]))
	})

	t.Run("conflict wrapping client offline keeps active state", func(t *testing.T) {
		api := &fakeExposureAPI{
			fakeServerAPI: &fakeServerAPI{},
			err: &fakeExposureSwitchError{
				active:      view.ExposureLocal,
				unavailable: []string{"ipv6"},
				closed:      true,
				err:         control.ErrClientOffline,
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"public"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		payload, raw := readExposureError(t, resp)
		require.Equal(t, "exposure_conflict", payload.Code,
			"a conflict carrying active state must not be reduced to client_offline")
		require.Equal(t, view.ExposureLocal, payload.ActiveExposure)
		require.Equal(t, []string{"ipv6"}, payload.UnavailableFamilies)
		require.NotNil(t, payload.Closed)
		require.True(t, *payload.Closed)
		require.JSONEq(t, `"local"`, string(raw["active_exposure"]))
		require.JSONEq(t, `["ipv6"]`, string(raw["unavailable_families"]))
		require.JSONEq(t, `true`, string(raw["closed"]))
	})

	t.Run("persistence failure is internal, not a conflict", func(t *testing.T) {
		api := &fakeExposureAPI{
			fakeServerAPI: &fakeServerAPI{},
			err: &fakeExposureSwitchError{
				active:      view.ExposureLocal,
				unavailable: []string{"ipv6"},
				persistence: true,
				err:         errors.New("disk full"),
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"public"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "internal", payload["code"])
		require.NotContains(t, payload["error"], "disk full")
		require.Equal(t, 1, api.callCount())
	})

	t.Run("client offline without conflict detail", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}, err: control.ErrClientOffline}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"local"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		payload, raw := readErrorPayload(t, resp)
		require.Equal(t, "client_offline", payload["code"])
		require.NotContains(t, raw, "active_exposure")
		require.NotContains(t, raw, "unavailable_families")
		require.NotContains(t, raw, "closed")
	})

	t.Run("unexpected failure is internal", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}, err: errors.New("boom")}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"local"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		payload := readJSON[map[string]string](t, resp)
		require.Equal(t, "internal", payload["code"])
		require.NotContains(t, payload["error"], "boom")
	})
}

func TestExposureAPISuccess(t *testing.T) {
	t.Run("local with unavailable family", func(t *testing.T) {
		api := &fakeExposureAPI{
			fakeServerAPI: &fakeServerAPI{},
			result: view.MappingView{
				Mapping: config.Mapping{
					ID:         "m1",
					Type:       "tcp",
					RemotePort: 2222,
					LocalAddr:  "127.0.0.1:22",
					Note:       "ssh",
					Enabled:    true,
				},
				Status:              "running",
				Error:               "bind_failed",
				AssignedPort:        2222,
				UnavailableFamilies: []string{"ipv6"},
				TxBytes:             1234,
				RxBytes:             5678,
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"local"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		mapping, raw := readExposureSuccess(t, resp)

		// The success body is the same raw view.MappingView shape as the GETs,
		// so all mapping fields and their PascalCase runtime names are present.
		require.Equal(t, "m1", mapping.ID)
		require.Equal(t, "tcp", mapping.Type)
		require.Equal(t, 2222, mapping.RemotePort)
		require.Equal(t, "127.0.0.1:22", mapping.LocalAddr)
		require.Equal(t, "ssh", mapping.Note)
		require.True(t, mapping.Enabled)
		require.Equal(t, "running", mapping.Status)
		require.Equal(t, "bind_failed", mapping.Error)
		require.Equal(t, 2222, mapping.AssignedPort)
		require.Equal(t, view.ExposureLocal, mapping.Exposure)
		require.Equal(t, []string{"ipv6"}, mapping.UnavailableFamilies)
		require.Equal(t, uint64(1234), mapping.TxBytes)
		require.Equal(t, uint64(5678), mapping.RxBytes)

		require.Len(t, raw, 13, "success must expose the full raw MappingView")
		for _, key := range []string{
			"id", "type", "remote_port", "local_addr", "note", "enabled",
			"Status", "Error", "AssignedPort", "Exposure",
			"UnavailableFamilies", "TxBytes", "RxBytes",
		} {
			require.Contains(t, raw, key)
		}
		require.JSONEq(t, `"local"`, string(raw["Exposure"]))
		require.JSONEq(t, `["ipv6"]`, string(raw["UnavailableFamilies"]))

		clientID, mappingID, exposure := api.lastCall()
		require.Equal(t, "c1", clientID)
		require.Equal(t, "m1", mappingID)
		require.Equal(t, view.ExposureLocal, exposure)
	})

	t.Run("public without warning", func(t *testing.T) {
		api := &fakeExposureAPI{
			fakeServerAPI: &fakeServerAPI{},
			result: view.MappingView{
				Mapping:      config.Mapping{ID: "m1", Type: "udp", RemotePort: 51820, LocalAddr: "127.0.0.1:51820", Enabled: true},
				Status:       "running",
				AssignedPort: 51820,
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"public"}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		mapping, raw := readExposureSuccess(t, resp)
		require.Equal(t, view.ExposurePublic, mapping.Exposure)
		require.Empty(t, mapping.UnavailableFamilies)
		// Raw view.MappingView has no omitempty, so a nil family list is null.
		// This matches the existing GET mapping JSON exactly.
		require.Equal(t, "null", string(raw["UnavailableFamilies"]))
		require.JSONEq(t, `""`, string(raw["Error"]))

		_, _, exposure := api.lastCall()
		require.Equal(t, view.ExposurePublic, exposure)
	})

	t.Run("unknown fields are ignored", func(t *testing.T) {
		api := &fakeExposureAPI{fakeServerAPI: &fakeServerAPI{}}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodPut, exposureURL(ts, "c1", "m1"),
			`{"exposure":"local","unknown":{"nested":true}}`, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		readBody(t, resp)
		_, _, exposure := api.lastCall()
		require.Equal(t, view.ExposureLocal, exposure)
	})
}
