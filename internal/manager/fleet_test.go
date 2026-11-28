package manager

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFleetClientsAggregatesByClientID(t *testing.T) {
	inv, _, _ := testInventory(t)
	a := newFakeUpstream(t)
	b := newFakeUpstream(t)
	_, err := inv.Add(context.Background(), addServerRequest("a", a.BaseURL()))
	require.NoError(t, err)
	_, err = inv.Add(context.Background(), addServerRequest("b", b.BaseURL()))
	require.NoError(t, err)

	clients, errs := inv.FleetClients(context.Background())
	require.Empty(t, errs)
	require.Len(t, clients, 2)

	byID := make(map[string]FleetClientView, len(clients))
	for _, client := range clients {
		byID[client.ID] = client
	}
	require.Len(t, byID["c1"].Servers, 2)
	require.Equal(t, "laptop", byID["c1"].DisplayName())
	require.True(t, byID["c1"].Online)
	require.Equal(t, 2, byID["c1"].MappingCount)
	require.Len(t, byID["c2"].Servers, 2)
	require.False(t, byID["c2"].Online)
}

func TestFleetClientsPageAndDetail(t *testing.T) {
	inv, store, audit := testInventory(t)
	a := newFakeUpstream(t)
	b := newFakeUpstream(t)
	_, err := inv.Add(context.Background(), addServerRequest("us", a.BaseURL()))
	require.NoError(t, err)
	_, err = inv.Add(context.Background(), addServerRequest("sg", b.BaseURL()))
	require.NoError(t, err)

	web, err := NewWebServer(store, audit, testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	web.SetInventory(inv)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clients", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "全部客户机")
	require.Contains(t, body, "laptop")
	require.Contains(t, body, "phone")
	require.Contains(t, body, "us")
	require.Contains(t, body, "sg")

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/clients/c1", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	body = readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "连接的服务端")
	require.Contains(t, body, "us")
	require.Contains(t, body, "sg")
	require.Contains(t, body, "查看映射")
}

func TestFleetClientPageMissingReturnsNotFound(t *testing.T) {
	inv, store, audit := testInventory(t)
	web, err := NewWebServer(store, audit, testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	web.SetInventory(inv)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/clients/missing", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
