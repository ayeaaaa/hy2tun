package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

const (
	// requestTimeout bounds one complete upstream HTTP exchange. Callers may
	// impose a shorter deadline with their context.
	requestTimeout   = 30 * time.Second
	maxResponseBytes = 8 << 20

	csrfHeader = "X-Requested-With"
	csrfValue  = "hy2tun"

	// sessionCookieName is the existing hy2tund Web session cookie. Login must
	// set it; a proxy cookie alone does not mean the API is authenticated.
	sessionCookieName = "hy2tun_session"
)

// Client is the manager's session with one hy2tund server. All requests are
// serialized: login, refresh reads, and mutations share the same cookie jar and
// must not race a re-login or a settings change. Reads are cheap on the server,
// and the inventory already bounds upstream refreshes to one in-flight request
// per server.
type Client struct {
	baseURL  string
	origin   string
	username string
	password []byte

	httpClient *http.Client
	cookies    *cookieJar

	mu            sync.Mutex
	authenticated bool
}

// cookieJar stores the upstream cookies for the single configured origin.
// net/http/cookiejar has no clear operation, and the adapter must discard a
// stale session before re-login. Only one Web session cookie is expected; if a
// proxy adds more all are retained by name.
type cookieJar struct {
	mu      sync.Mutex
	cookies map[string]*http.Cookie
}

func newCookieJar() *cookieJar {
	return &cookieJar{cookies: make(map[string]*http.Cookie)}
}

func (j *cookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, cookie := range cookies {
		if cookie == nil {
			continue
		}
		if cookie.MaxAge < 0 {
			delete(j.cookies, cookie.Name)
			continue
		}
		cloned := *cookie
		j.cookies[cookie.Name] = &cloned
	}
}

func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	out := make([]*http.Cookie, 0, len(j.cookies))
	for name, cookie := range j.cookies {
		if !cookie.Expires.IsZero() && !now.Before(cookie.Expires) {
			delete(j.cookies, name)
			continue
		}
		cloned := *cookie
		out = append(out, &cloned)
	}
	return out
}

func (j *cookieJar) clear() {
	j.mu.Lock()
	j.cookies = make(map[string]*http.Cookie)
	j.mu.Unlock()
}

func (j *cookieJar) has(name string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.cookies[name]
	return ok
}

type httpResult struct {
	status int
	header http.Header
	body   []byte
}

// NewClient creates one upstream session adapter. password is the decrypted
// upstream administrator password and is copied; it is never logged or
// included in an error.
func NewClient(record ServerRecord, password []byte) (*Client, error) {
	if strings.TrimSpace(record.BaseURL) == "" {
		return nil, fmt.Errorf("upstream base URL is empty")
	}
	if strings.TrimSpace(record.UpstreamUsername) == "" {
		return nil, fmt.Errorf("upstream username is empty")
	}

	rawBaseURL := strings.TrimSpace(record.BaseURL)
	parsed, err := url.Parse(rawBaseURL)
	if err != nil {
		// The parse error can echo the raw URL, which may contain userinfo.
		// Keep the message generic so credentials cannot appear in an error.
		return nil, fmt.Errorf("upstream base URL is not a valid URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if parsed.Opaque != "" || parsed.Host == "" || (scheme != "http" && scheme != "https") {
		return nil, fmt.Errorf("upstream base URL must be an absolute http or https URL")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("upstream base URL must not include user information")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("upstream base URL must not include a query or fragment")
	}

	transport := newTransport(record.TLSSkipVerify)
	jar := newCookieJar()
	return &Client{
		baseURL:  strings.TrimRight(rawBaseURL, "/"),
		origin:   scheme + "://" + parsed.Host,
		username: record.UpstreamUsername,
		password: append([]byte(nil), password...),
		httpClient: &http.Client{
			Transport: transport,
			Jar:       jar,
			Timeout:   requestTimeout,
			// Never replay a form or JSON body, including the upstream
			// password, to a redirect target. hy2tund's API does not use
			// redirects, so a redirect is reported as an upstream error.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		cookies: jar,
	}, nil
}

func newTransport(skipVerify bool) *http.Transport {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.MinVersion = tls.VersionTLS12
	}
	// Only the operator's explicit inventory flag disables verification. The
	// default remains full certificate and hostname validation.
	transport.TLSClientConfig.InsecureSkipVerify = skipVerify
	return transport
}

// Login discards any previous cookie and authenticates with the stored
// credentials. A successful login requires the server to set a session cookie.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	c.authenticated = false
	c.cookies.clear()

	form := url.Values{}
	form.Set("username", c.username)
	form.Set("password", string(c.password))
	result, err := c.send(ctx, http.MethodPost, "/api/login",
		[]byte(form.Encode()), "application/x-www-form-urlencoded", false)
	if err != nil {
		return fmt.Errorf("upstream login: %w", err)
	}
	if result.status < http.StatusOK || result.status >= http.StatusMultipleChoices {
		return newAPIError(result.status, result.header, result.body, string(c.password))
	}
	if !c.cookies.has(sessionCookieName) {
		return fmt.Errorf("upstream login succeeded without setting the %s session cookie", sessionCookieName)
	}
	c.authenticated = true
	return nil
}

// CloseIdleConnections releases pooled connections. It is safe to call while
// the client is idle and is a no-op on a nil client.
func (c *Client) CloseIdleConnections() {
	if c == nil || c.httpClient == nil {
		return
	}
	c.httpClient.CloseIdleConnections()
}

func (c *Client) endpoint(apiPath string) string {
	return c.baseURL + "/" + strings.TrimPrefix(apiPath, "/")
}

func (c *Client) send(ctx context.Context, method, apiPath string, payload []byte, contentType string, mutation bool) (*httpResult, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(apiPath), body)
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if mutation {
		request.Header.Set(csrfHeader, csrfValue)
		request.Header.Set("Origin", c.origin)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("upstream %s %s: %w", method, apiPath, err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream %s response: %w", apiPath, err)
	}
	if len(responseBody) > maxResponseBytes {
		return nil, fmt.Errorf("upstream %s response exceeds %d bytes", apiPath, maxResponseBytes)
	}
	return &httpResult{
		status: response.StatusCode,
		header: response.Header.Clone(),
		body:   responseBody,
	}, nil
}

// do serializes one authenticated operation. On 401 it clears the cookie,
// logs in once, and retries the original request once. A second 401 is
// returned as an APIError; it never loops. Other statuses, including 409, are
// never retried.
func (c *Client) do(ctx context.Context, method, apiPath string, payload []byte, contentType string, mutation bool, secrets ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.authenticated {
		if err := c.loginLocked(ctx); err != nil {
			return nil, err
		}
	}

	result, err := c.send(ctx, method, apiPath, payload, contentType, mutation)
	if err != nil {
		return nil, err
	}
	if result.status == http.StatusUnauthorized {
		c.authenticated = false
		if err := c.loginLocked(ctx); err != nil {
			return nil, err
		}
		result, err = c.send(ctx, method, apiPath, payload, contentType, mutation)
		if err != nil {
			return nil, err
		}
	}

	if result.status >= http.StatusOK && result.status < http.StatusMultipleChoices {
		return result.body, nil
	}
	return nil, newAPIError(result.status, result.header, result.body, secrets...)
}

func (c *Client) getJSON(ctx context.Context, apiPath string, destination any) error {
	body, err := c.do(ctx, http.MethodGet, apiPath, nil, "", false)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return fmt.Errorf("upstream %s returned an empty response", apiPath)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("decode upstream %s response: %w", apiPath, err)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, apiPath string, payload any, secrets ...string) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}
	return c.do(ctx, method, apiPath, body, "application/json", true, secrets...)
}

// Overview fetches the secret-free server overview.
func (c *Client) Overview(ctx context.Context) (Overview, error) {
	var wire struct {
		OnlineClients  int    `json:"online_clients"`
		ActiveMappings int    `json:"active_mappings"`
		TxBytes        uint64 `json:"tx_bytes"`
		RxBytes        uint64 `json:"rx_bytes"`
		Listen         string `json:"listen"`
		UptimeSeconds  int64  `json:"uptime_seconds"`
	}
	if err := c.getJSON(ctx, "/api/overview", &wire); err != nil {
		return Overview{}, err
	}
	return Overview{
		OnlineClients:  wire.OnlineClients,
		ActiveMappings: wire.ActiveMappings,
		TxBytes:        wire.TxBytes,
		RxBytes:        wire.RxBytes,
		Listen:         wire.Listen,
		Uptime:         time.Duration(wire.UptimeSeconds) * time.Second,
	}, nil
}

// Clients fetches the full client inventory.
func (c *Client) Clients(ctx context.Context) ([]ClientView, error) {
	var clients []ClientView
	if err := c.getJSON(ctx, "/api/clients", &clients); err != nil {
		return nil, err
	}
	if clients == nil {
		clients = []ClientView{}
	}
	return clients, nil
}

// Client fetches one client's detail, including its current mapping revision.
func (c *Client) Client(ctx context.Context, id string) (ClientView, error) {
	if strings.TrimSpace(id) == "" {
		return ClientView{}, fmt.Errorf("client ID is empty")
	}
	var client ClientView
	if err := c.getJSON(ctx, "/api/clients/"+url.PathEscape(id), &client); err != nil {
		return ClientView{}, err
	}
	return client, nil
}

// Settings fetches the secret-free server settings snapshot.
func (c *Client) Settings(ctx context.Context) (Settings, error) {
	var settings Settings
	if err := c.getJSON(ctx, "/api/settings", &settings); err != nil {
		return Settings{}, err
	}
	settings.Ports.OfflineReserve = time.Duration(settings.Ports.OfflineReserveSeconds) * time.Second
	return settings, nil
}

// ConfigSummary fetches the server's read-only configuration and version
// snapshot. The manager uses Version to detect optional API capabilities.
func (c *Client) ConfigSummary(ctx context.Context) (ConfigSummary, error) {
	var summary ConfigSummary
	if err := c.getJSON(ctx, "/api/config/summary", &summary); err != nil {
		return ConfigSummary{}, err
	}
	summary.OfflineReserve = time.Duration(summary.OfflineReserveSeconds) * time.Second
	return summary, nil
}

// PushMappings replaces the client's complete mapping list at baseRevision.
// A non-empty exposures map is sent in the same request so the server can keep
// a new local mapping disabled until its exposure is applied.
func (c *Client) PushMappings(ctx context.Context, clientID string, baseRevision int64, mappings []config.Mapping, exposures map[string]view.Exposure) error {
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("client ID is empty")
	}
	if mappings == nil {
		mappings = []config.Mapping{}
	}
	if len(exposures) == 0 {
		exposures = nil
	}
	payload := struct {
		BaseRevision int64                    `json:"base_revision"`
		Mappings     []config.Mapping         `json:"mappings"`
		Exposures    map[string]view.Exposure `json:"exposures,omitempty"`
	}{
		BaseRevision: baseRevision,
		Mappings:     mappings,
		Exposures:    exposures,
	}
	_, err := c.doJSON(ctx, http.MethodPut, "/api/clients/"+url.PathEscape(clientID)+"/mappings", payload)
	return err
}

// SetExposure changes one existing mapping's public/local exposure. The server
// stores this independently of the mapping revision, so it is valid even when
// the client is offline.
func (c *Client) SetExposure(ctx context.Context, clientID, mappingID string, exposure view.Exposure) error {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(mappingID) == "" {
		return fmt.Errorf("client ID and mapping ID are required")
	}
	payload := struct {
		Exposure view.Exposure `json:"exposure"`
	}{Exposure: exposure}
	_, err := c.doJSON(ctx, http.MethodPut,
		"/api/clients/"+url.PathEscape(clientID)+"/mappings/"+url.PathEscape(mappingID)+"/exposure",
		payload)
	return err
}

// UpdateMode changes the server transport mode.
func (c *Client) UpdateMode(ctx context.Context, mode config.TransportMode) error {
	payload := struct {
		Mode config.TransportMode `json:"mode"`
	}{Mode: mode}
	_, err := c.doJSON(ctx, http.MethodPut, "/api/settings/mode", payload)
	return err
}

// UpdatePorts replaces the server port policy. The duration is sent as whole
// seconds, matching the existing API field.
func (c *Client) UpdatePorts(ctx context.Context, allow, deny []string, autoRange string, offlineReserve time.Duration) error {
	payload := struct {
		Allow                 []string `json:"allow"`
		Deny                  []string `json:"deny"`
		AutoRange             string   `json:"auto_range"`
		OfflineReserveSeconds int64    `json:"offline_reserve_seconds"`
	}{
		Allow:                 allow,
		Deny:                  deny,
		AutoRange:             autoRange,
		OfflineReserveSeconds: int64(offlineReserve / time.Second),
	}
	_, err := c.doJSON(ctx, http.MethodPut, "/api/settings/ports", payload)
	return err
}

// UpdateWebAdmin changes the upstream Web administrator. currentPassword is
// required by the server to re-authenticate; the caller is responsible for
// storing the new password only after this succeeds.
func (c *Client) UpdateWebAdmin(ctx context.Context, username, currentPassword, newPassword string) error {
	payload := struct {
		Username        string `json:"username"`
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}{
		Username:        username,
		CurrentPassword: currentPassword,
		NewPassword:     newPassword,
		ConfirmPassword: newPassword,
	}
	_, err := c.doJSON(ctx, http.MethodPut, "/api/settings/web", payload, currentPassword, newPassword)
	return err
}

// SetTrustProxy updates the server's trust-proxy setting.
func (c *Client) SetTrustProxy(ctx context.Context, trust bool) error {
	payload := struct {
		TrustProxy bool `json:"trust_proxy"`
	}{TrustProxy: trust}
	_, err := c.doJSON(ctx, http.MethodPut, "/api/settings/proxy", payload)
	return err
}

// RotateCredentials installs a new auth/control-token pair. The server returns
// its per-client sync results. Secrets are redacted from any error text.
func (c *Client) RotateCredentials(ctx context.Context, auth, controlToken, syncMode string) (RotateResult, error) {
	payload := struct {
		Auth         string `json:"auth"`
		ControlToken string `json:"control_token"`
		SyncMode     string `json:"sync_mode"`
	}{
		Auth:         auth,
		ControlToken: controlToken,
		SyncMode:     syncMode,
	}
	body, err := c.doJSON(ctx, http.MethodPut, "/api/settings/credentials", payload, auth, controlToken)
	if err != nil {
		return RotateResult{}, err
	}
	if len(body) == 0 {
		return RotateResult{}, fmt.Errorf("upstream credential rotation returned an empty response")
	}
	var result RotateResult
	if err := json.Unmarshal(body, &result); err != nil {
		return RotateResult{}, fmt.Errorf("decode upstream credential rotation response: %w", err)
	}
	if result.Results == nil {
		result.Results = []CredentialResult{}
	}
	return result, nil
}

// RevokeCredentials revokes the server's previous credential generation.
func (c *Client) RevokeCredentials(ctx context.Context) error {
	_, err := c.doJSON(ctx, http.MethodPost, "/api/settings/credentials/revoke", struct{}{})
	return err
}
