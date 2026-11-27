package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"hy2tun/internal/manager/upstream"
)

const (
	// serverRefreshTimeout bounds one complete upstream refresh (login plus the
	// overview, clients, and config-summary reads).
	serverRefreshTimeout = 15 * time.Second
	// inventoryRefreshConcurrency bounds a fleet-wide refresh. Each server also
	// has a per-server lock, so one slow server cannot block the others and
	// never receives overlapping refresh requests from this process.
	inventoryRefreshConcurrency = 4

	ServerStateOnline      = "online"
	ServerStateAuthError   = "auth_error"
	ServerStateUnreachable = "unreachable"
	ServerStateUnsupported = "unsupported"
	ServerStateUnknown     = "unknown"
)

// ClientFactory opens one upstream session. The production implementation is
// NewUpstreamClient, which adapts the manager's ServerRecord to the upstream
// package's narrow record type. Tests may wrap it to observe factory calls.
type ClientFactory func(ServerRecord, []byte) (*upstream.Client, error)

// NewUpstreamClient is the production ClientFactory. The conversion lives here
// at the package boundary so the upstream adapter never imports manager.
func NewUpstreamClient(record ServerRecord, password []byte) (*upstream.Client, error) {
	return upstream.NewClient(upstream.ServerRecord{
		ID:               record.ID,
		Name:             record.Name,
		BaseURL:          record.BaseURL,
		UpstreamUsername: record.UpstreamUsername,
		TLSSkipVerify:    record.TLSSkipVerify,
	}, password)
}

// ErrServerNotFound reports an inventory ID that does not exist. Handlers map
// it to a 404 and Task 6 reuses it through WithClient.
var ErrServerNotFound = errors.New("server not found")

// ValidationError reports operator-supplied inventory input that must be
// corrected before anything is persisted or sent upstream.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// ServerConnectionError reports a failed upstream login/read probe. State is
// one of the inventory state constants and Err is safe for operator display:
// it never contains the upstream password.
type ServerConnectionError struct {
	State string
	Err   error
}

func (e *ServerConnectionError) Error() string { return e.Err.Error() }
func (e *ServerConnectionError) Unwrap() error { return e.Err }

// AddServerRequest is the write-only payload for adding a server.
type AddServerRequest struct {
	Name          string
	BaseURL       string
	Username      string
	Password      string
	TLSSkipVerify bool
}

// UpdateServerRequest edits a server. An empty Password keeps the stored
// credential; a non-empty value is tested before it is saved.
type UpdateServerRequest struct {
	Name          string
	BaseURL       string
	Username      string
	Password      string
	TLSSkipVerify bool
}

// ServerView is the secret-free inventory display model. It deliberately has no
// password, ciphertext, cookie, or session field.
type ServerView struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	BaseURL          string    `json:"base_url"`
	UpstreamUsername string    `json:"upstream_username"`
	TLSSkipVerify    bool      `json:"tls_skip_verify"`
	State            string    `json:"state"`
	Version          string    `json:"version,omitempty"`
	Mode             string    `json:"mode,omitempty"`
	ClientCount      int       `json:"client_count"` // online clients from GET /api/overview
	MappingCount     int       `json:"mapping_count"`
	LastRefresh      time.Time `json:"last_refresh"`
	LastError        string    `json:"last_error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// serverRuntime is the in-memory cache for one inventory entry. view is
// protected by mu. The cached upstream client is reused across refreshes to
// keep its cookie jar, but its identity fields let a refresh detect a record
// or credential change (including a direct Store update) and recreate it.
type serverRuntime struct {
	mu   sync.Mutex
	view ServerView
	// client and its identity must be updated under mu.
	client              *upstream.Client
	clientBaseURL       string
	clientUsername      string
	clientTLSSkipVerify bool
	clientPasswordEnc   string
	removed             bool
}

// Inventory owns the configured server records and their display cache. The
// Store remains the source of truth for inventory and credentials; the cache
// is intentionally memory-only and is rebuilt after a restart.
type Inventory struct {
	store   *Store
	audit   *AuditLog
	factory ClientFactory
	log     *slog.Logger

	mu      sync.Mutex
	entries map[string]*serverRuntime

	// opLocks are keyed by server ID, not by serverRuntime: Update replaces the
	// runtime pointer, and a refresh that snapshotted the old pointer must still
	// serialize with that replacement. Every operation that mutates a server's
	// record or replaces its runtime holds this lock, as does Refresh.
	opMu    sync.Mutex
	opLocks map[string]*sync.Mutex

	refreshTimeout time.Duration
	now            func() time.Time
}

// NewInventory builds an inventory over an existing store. It does not start a
// background refresh: the composition root owns that lifecycle and calls
// RefreshAll on its configured interval.
func NewInventory(store *Store, audit *AuditLog, factory ClientFactory, log *slog.Logger) *Inventory {
	if factory == nil {
		factory = NewUpstreamClient
	}
	if log == nil {
		log = slog.Default()
	}
	inventory := &Inventory{
		store:          store,
		audit:          audit,
		factory:        factory,
		log:            log,
		entries:        make(map[string]*serverRuntime),
		opLocks:        make(map[string]*sync.Mutex),
		refreshTimeout: serverRefreshTimeout,
		now:            time.Now,
	}
	if store != nil {
		inventory.mu.Lock()
		inventory.syncLocked(store.State().Servers)
		inventory.mu.Unlock()
	}
	return inventory
}

// operationLock returns the per-server-ID mutex shared by Refresh, Add, Update,
// and Remove. The map outlives individual serverRuntime pointers deliberately;
// removing an ID never allows a new server with that ID, so retaining the lock
// cannot deadlock a future entry.
func (i *Inventory) operationLock(id string) *sync.Mutex {
	i.opMu.Lock()
	defer i.opMu.Unlock()
	if i.opLocks == nil {
		i.opLocks = make(map[string]*sync.Mutex)
	}
	lock := i.opLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		i.opLocks[id] = lock
	}
	return lock
}

// List returns the fleet in display-name order. It reconciles the cache with
// the store so records added by another code path still appear.
func (i *Inventory) List() []ServerView {
	if i == nil || i.store == nil {
		return []ServerView{}
	}
	i.mu.Lock()
	i.syncLocked(i.store.State().Servers)
	entries := make([]*serverRuntime, 0, len(i.entries))
	for _, entry := range i.entries {
		entries = append(entries, entry)
	}
	i.mu.Unlock()

	views := make([]ServerView, 0, len(entries))
	for _, entry := range entries {
		entry.mu.Lock()
		views = append(views, entry.view)
		entry.mu.Unlock()
	}
	sort.Slice(views, func(a, b int) bool {
		if views[a].Name == views[b].Name {
			return views[a].ID < views[b].ID
		}
		return views[a].Name < views[b].Name
	})
	return views
}

// Get returns one cached server view.
func (i *Inventory) Get(id string) (ServerView, bool) {
	if i == nil || i.store == nil {
		return ServerView{}, false
	}
	i.mu.Lock()
	i.syncLocked(i.store.State().Servers)
	entry := i.entries[id]
	i.mu.Unlock()
	if entry == nil {
		return ServerView{}, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.removed {
		return ServerView{}, false
	}
	return entry.view, true
}

// Add validates the inventory row, proves the credentials with login plus an
// overview read, encrypts the password, persists the record, and then fills the
// display cache. A failed probe is returned as a ServerView with a classified
// state and is never saved.
func (i *Inventory) Add(ctx context.Context, request AddServerRequest) (ServerView, error) {
	if i == nil || i.store == nil {
		return ServerView{}, fmt.Errorf("manager inventory is not configured")
	}
	name, baseURL, username, err := i.validateFields(request.Name, request.BaseURL, request.Username)
	if err != nil {
		return ServerView{}, err
	}
	if strings.TrimSpace(request.Password) == "" {
		return ServerView{}, &ValidationError{Message: "password is required"}
	}
	if err := i.ensureUniqueName(name, ""); err != nil {
		return ServerView{}, err
	}

	id, err := newServerID()
	if err != nil {
		return ServerView{}, err
	}
	now := i.now().UTC()
	record := ServerRecord{
		ID:               id,
		Name:             name,
		BaseURL:          baseURL,
		UpstreamUsername: username,
		TLSSkipVerify:    request.TLSSkipVerify,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	probeView, err := i.probe(ctx, record, []byte(request.Password))
	if err != nil {
		probeView.ID = record.ID
		probeView.CreatedAt = record.CreatedAt
		probeView.UpdatedAt = record.UpdatedAt
		return probeView, err
	}
	probeView.ID = record.ID

	// Hold the per-ID operation lock from the durable store write through the
	// initial refresh, so no refresh can observe a half-created runtime.
	opLock := i.operationLock(record.ID)
	opLock.Lock()
	defer opLock.Unlock()

	// The pending append is the fail-closed gate: if it cannot be durably
	// written, the inventory record is not created. The final result is written
	// only after the store commit succeeds.
	if err := i.appendAudit(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     "server.add",
		Result:     "pending",
	}); err != nil {
		return probeView, err
	}
	if err := i.store.AddServer(record, []byte(request.Password)); err != nil {
		i.appendAuditBestEffort(ctx, AuditEntry{
			ServerID:   record.ID,
			ServerName: record.Name,
			Action:     "server.add",
			Result:     "error",
			Message:    err.Error(),
		})
		return probeView, validationFromStoreError(err)
	}
	i.appendAuditBestEffort(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     "server.add",
		Result:     "ok",
	})

	i.mu.Lock()
	i.entries[id] = &serverRuntime{view: probeView}
	i.mu.Unlock()

	refreshed, refreshErr := i.refreshLocked(ctx, id)
	if refreshErr != nil {
		i.log.Debug("initial server refresh failed", "server", id, "error", refreshErr)
		return i.viewOr(probeView), nil
	}
	return refreshed, nil
}

// Update edits non-secret fields and, when supplied, tests and replaces the
// upstream password. A duplicate name or failed probe leaves both the state
// file and the cached credential untouched.
func (i *Inventory) Update(ctx context.Context, id string, request UpdateServerRequest) error {
	if i == nil || i.store == nil {
		return fmt.Errorf("manager inventory is not configured")
	}
	// Serialize the entire read/test/replace sequence with Refresh and other
	// updates for this server ID. The lock is keyed by ID so it survives the
	// runtime replacement below.
	opLock := i.operationLock(id)
	opLock.Lock()
	defer opLock.Unlock()

	record, ok := i.serverRecord(id)
	if !ok {
		return fmt.Errorf("unknown server %q", id)
	}
	name, baseURL, username, err := i.validateFields(request.Name, request.BaseURL, request.Username)
	if err != nil {
		return err
	}
	if err := i.ensureUniqueName(name, id); err != nil {
		return err
	}

	password, err := i.passwordForUpdate(id, request.Password)
	if err != nil {
		return err
	}
	updated := record
	updated.Name = name
	updated.BaseURL = baseURL
	updated.UpstreamUsername = username
	updated.TLSSkipVerify = request.TLSSkipVerify
	updated.UpdatedAt = i.now().UTC()

	probeView, err := i.probe(ctx, updated, password)
	if err != nil {
		return err
	}
	if err := i.appendAudit(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: updated.Name,
		Action:     "server.update",
		Result:     "pending",
	}); err != nil {
		return err
	}

	var passwordUpdate []byte
	if strings.TrimSpace(request.Password) != "" {
		passwordUpdate = password
	}
	if err := i.store.UpdateServer(updated, passwordUpdate); err != nil {
		i.appendAuditBestEffort(ctx, AuditEntry{
			ServerID:   record.ID,
			ServerName: updated.Name,
			Action:     "server.update",
			Result:     "error",
			Message:    err.Error(),
		})
		return validationFromStoreError(err)
	}
	i.appendAuditBestEffort(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: updated.Name,
		Action:     "server.update",
		Result:     "ok",
	})

	identityChanged := record.BaseURL != updated.BaseURL ||
		record.UpstreamUsername != updated.UpstreamUsername ||
		record.TLSSkipVerify != updated.TLSSkipVerify
	nextView := probeView
	nextView.ID = updated.ID
	nextView.Name = updated.Name
	nextView.BaseURL = updated.BaseURL
	nextView.UpstreamUsername = updated.UpstreamUsername
	nextView.TLSSkipVerify = updated.TLSSkipVerify
	nextView.CreatedAt = record.CreatedAt
	nextView.UpdatedAt = updated.UpdatedAt
	if !identityChanged {
		if previous, ok := i.Get(id); ok {
			nextView.Version = previous.Version
			nextView.Mode = previous.Mode
		}
	}

	i.mu.Lock()
	old := i.entries[id]
	if old != nil {
		old.mu.Lock()
		old.removed = true
		oldClient := old.client
		old.mu.Unlock()
		if oldClient != nil {
			oldClient.CloseIdleConnections()
		}
	}
	i.entries[id] = &serverRuntime{view: nextView}
	i.mu.Unlock()

	if _, err := i.refreshLocked(ctx, id); err != nil {
		i.log.Debug("server refresh after update failed", "server", id, "error", err)
	}
	return nil
}

// TestConnection proves new credentials without persisting anything. It is the
// add form's "test connection" action.
func (i *Inventory) TestConnection(ctx context.Context, request AddServerRequest) (ServerView, error) {
	_, baseURL, username, err := i.validateFields("probe", request.BaseURL, request.Username)
	if err != nil {
		return ServerView{}, err
	}
	if strings.TrimSpace(request.Password) == "" {
		return ServerView{}, &ValidationError{Message: "password is required"}
	}
	record := ServerRecord{
		Name:             strings.TrimSpace(request.Name),
		BaseURL:          baseURL,
		UpstreamUsername: username,
		TLSSkipVerify:    request.TLSSkipVerify,
	}
	view, err := i.probe(ctx, record, []byte(request.Password))
	return view, err
}

// TestStoredConnection proves the existing credentials with an optional
// write-only replacement password. It never updates state or the cache.
func (i *Inventory) TestStoredConnection(ctx context.Context, id string, request UpdateServerRequest) (ServerView, error) {
	record, ok := i.serverRecord(id)
	if !ok {
		return ServerView{}, fmt.Errorf("unknown server %q", id)
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = record.Name
	}
	baseURLInput := strings.TrimSpace(request.BaseURL)
	if baseURLInput == "" {
		baseURLInput = record.BaseURL
	}
	usernameInput := strings.TrimSpace(request.Username)
	if usernameInput == "" {
		usernameInput = record.UpstreamUsername
	}
	name, baseURL, username, err := i.validateFields(name, baseURLInput, usernameInput)
	if err != nil {
		return ServerView{}, err
	}
	password, err := i.passwordForUpdate(id, request.Password)
	if err != nil {
		return ServerView{}, err
	}
	probeRecord := record
	probeRecord.Name = name
	probeRecord.BaseURL = baseURL
	probeRecord.UpstreamUsername = username
	probeRecord.TLSSkipVerify = request.TLSSkipVerify
	return i.probe(ctx, probeRecord, password)
}

// Refresh reads one server with a 15-second context and caches display data.
// Login/API failures and transport failures classify the server as
// auth_error/unreachable while preserving the last successful version, counts,
// and timestamp. The inventory record is never removed.
func (i *Inventory) Refresh(ctx context.Context, id string) (ServerView, error) {
	if i == nil || i.store == nil {
		return ServerView{}, fmt.Errorf("manager inventory is not configured")
	}
	opLock := i.operationLock(id)
	opLock.Lock()
	defer opLock.Unlock()
	return i.refreshLocked(ctx, id)
}

// WithClient invokes fn with the cached upstream client for one server while
// holding that server's operation lock. The record, credential, and client
// identity are re-read after the lock so a queued Update/Refresh cannot leave
// the callback using a previous BaseURL, username, TLS setting, or password.
// fn receives the request context; it must not retain the client after return.
func (i *Inventory) WithClient(ctx context.Context, serverID string, fn func(context.Context, *upstream.Client) error) error {
	if i == nil || i.store == nil {
		return fmt.Errorf("manager inventory is not configured")
	}
	if fn == nil {
		return &ValidationError{Message: "client operation callback is required"}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	opLock := i.operationLock(serverID)
	opLock.Lock()
	defer opLock.Unlock()

	state := i.store.State()
	record, ok := serverRecordFrom(state.Servers, serverID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrServerNotFound, serverID)
	}
	i.mu.Lock()
	i.syncLocked(state.Servers)
	entry := i.entries[serverID]
	i.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("%w: %s", ErrServerNotFound, serverID)
	}

	entry.mu.Lock()
	removed := entry.removed
	client := entry.client
	identityCurrent := client != nil &&
		entry.clientBaseURL == record.BaseURL &&
		entry.clientUsername == record.UpstreamUsername &&
		entry.clientTLSSkipVerify == record.TLSSkipVerify &&
		entry.clientPasswordEnc == record.UpstreamPasswordEnc
	entry.mu.Unlock()
	if removed {
		return fmt.Errorf("%w: %s", ErrServerNotFound, serverID)
	}

	created := false
	if client != nil && !identityCurrent {
		client.CloseIdleConnections()
		entry.mu.Lock()
		if entry.client == client {
			entry.client = nil
			entry.clientBaseURL = ""
			entry.clientUsername = ""
			entry.clientTLSSkipVerify = false
			entry.clientPasswordEnc = ""
		}
		entry.mu.Unlock()
		client = nil
	}
	if client == nil {
		password, err := i.store.ServerPassword(serverID)
		if err != nil {
			return err
		}
		client, err = i.factory(record, password)
		if err != nil {
			return err
		}
		created = true
	}

	if err := fn(ctx, client); err != nil {
		if created {
			// The new client was never cached; release its transport
			// resources instead of waiting for the garbage collector.
			client.CloseIdleConnections()
		}
		return err
	}

	entry.mu.Lock()
	if entry.removed {
		entry.mu.Unlock()
		if created {
			client.CloseIdleConnections()
		}
		return fmt.Errorf("%w: %s", ErrServerNotFound, serverID)
	}
	entry.client = client
	entry.clientBaseURL = record.BaseURL
	entry.clientUsername = record.UpstreamUsername
	entry.clientTLSSkipVerify = record.TLSSkipVerify
	entry.clientPasswordEnc = record.UpstreamPasswordEnc
	entry.mu.Unlock()
	return nil
}

// refreshLocked performs one refresh while the caller holds operationLock(id).
// It re-reads both the inventory record and credential after the lock, so an
// Update that replaced the runtime cannot leave this refresh using the previous
// server's BaseURL/username. Cached-client identity is checked the same way so a
// direct Store.UpdateServer or Store.SetServerPassword caller also recovers.
func (i *Inventory) refreshLocked(ctx context.Context, id string) (ServerView, error) {
	state := i.store.State()
	record, ok := serverRecordFrom(state.Servers, id)
	if !ok {
		return ServerView{}, fmt.Errorf("unknown server %q", id)
	}
	i.mu.Lock()
	i.syncLocked(state.Servers)
	entry := i.entries[id]
	i.mu.Unlock()
	if entry == nil {
		return ServerView{}, fmt.Errorf("unknown server %q", id)
	}

	entry.mu.Lock()
	removed := entry.removed
	client := entry.client
	clientBaseURL := entry.clientBaseURL
	clientUsername := entry.clientUsername
	clientTLSSkipVerify := entry.clientTLSSkipVerify
	clientPasswordEnc := entry.clientPasswordEnc
	entry.mu.Unlock()
	if removed {
		return ServerView{}, fmt.Errorf("server %q was removed", id)
	}

	password, err := i.store.ServerPassword(id)
	if err != nil {
		return i.refreshFailure(entry, err)
	}

	if client != nil &&
		(clientBaseURL != record.BaseURL ||
			clientUsername != record.UpstreamUsername ||
			clientTLSSkipVerify != record.TLSSkipVerify ||
			clientPasswordEnc != record.UpstreamPasswordEnc) {
		client.CloseIdleConnections()
		entry.mu.Lock()
		if entry.client == client {
			entry.client = nil
			entry.clientBaseURL = ""
			entry.clientUsername = ""
			entry.clientTLSSkipVerify = false
			entry.clientPasswordEnc = ""
		}
		entry.mu.Unlock()
		client = nil
	}

	refreshCtx, cancel := context.WithTimeout(ctx, i.refreshTimeout)
	defer cancel()

	createdLocalClient := false
	if client == nil {
		client, err = i.factory(record, password)
		if err != nil {
			return i.refreshFailure(entry, err)
		}
		createdLocalClient = true
	}
	fail := func(err error) (ServerView, error) {
		if createdLocalClient && client != nil {
			// The new client was never cached; release its idle transport
			// resources instead of waiting for GC.
			client.CloseIdleConnections()
		}
		return i.refreshFailure(entry, err)
	}

	overview, err := client.Overview(refreshCtx)
	if err != nil {
		return fail(err)
	}
	summary, summaryErr := client.ConfigSummary(refreshCtx)
	if summaryErr != nil && classifyError(summaryErr, true) != ServerStateUnsupported {
		return fail(summaryErr)
	}

	entry.mu.Lock()
	if entry.removed {
		entry.mu.Unlock()
		client.CloseIdleConnections()
		return ServerView{}, fmt.Errorf("server %q was removed", id)
	}
	entry.client = client
	entry.clientBaseURL = record.BaseURL
	entry.clientUsername = record.UpstreamUsername
	entry.clientTLSSkipVerify = record.TLSSkipVerify
	entry.clientPasswordEnc = record.UpstreamPasswordEnc
	entry.view.State = ServerStateOnline
	entry.view.ClientCount = overview.OnlineClients
	entry.view.MappingCount = overview.ActiveMappings
	entry.view.LastRefresh = i.now().UTC()
	entry.view.LastError = ""
	if summaryErr == nil {
		entry.view.Version = summary.Version
		entry.view.Mode = string(summary.Mode)
	} else {
		entry.view.State = ServerStateUnsupported
		entry.view.LastError = summaryErr.Error()
	}
	view := entry.view
	entry.mu.Unlock()

	if summaryErr != nil {
		return view, &ServerConnectionError{State: ServerStateUnsupported, Err: summaryErr}
	}
	return view, nil
}

// Remove deletes the inventory entry and local client cache only. It never
// calls the upstream API and does not delete server-side client state. The
// audit append happens first so an audit outage blocks the destructive action.
func (i *Inventory) Remove(id string) error {
	return i.RemoveWithContext(context.Background(), id)
}

// RemoveWithContext is Remove with the request audit actor attached.
func (i *Inventory) RemoveWithContext(ctx context.Context, id string) error {
	if i == nil || i.store == nil {
		return fmt.Errorf("manager inventory is not configured")
	}
	opLock := i.operationLock(id)
	opLock.Lock()
	defer opLock.Unlock()

	record, ok := i.serverRecord(id)
	if !ok {
		return fmt.Errorf("unknown server %q", id)
	}
	if err := i.appendAudit(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     "server.remove",
		Result:     "pending",
	}); err != nil {
		return err
	}
	if _, err := i.store.RemoveServer(id); err != nil {
		i.appendAuditBestEffort(ctx, AuditEntry{
			ServerID:   record.ID,
			ServerName: record.Name,
			Action:     "server.remove",
			Result:     "error",
			Message:    err.Error(),
		})
		return err
	}
	i.appendAuditBestEffort(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     "server.remove",
		Result:     "ok",
	})

	i.mu.Lock()
	entry := i.entries[id]
	delete(i.entries, id)
	i.mu.Unlock()
	if entry != nil {
		entry.mu.Lock()
		entry.removed = true
		client := entry.client
		entry.client = nil
		entry.clientBaseURL = ""
		entry.clientUsername = ""
		entry.clientTLSSkipVerify = false
		entry.clientPasswordEnc = ""
		entry.mu.Unlock()
		if client != nil {
			client.CloseIdleConnections()
		}
	}
	return nil
}

func (i *Inventory) probe(ctx context.Context, record ServerRecord, password []byte) (ServerView, error) {
	probeCtx, cancel := context.WithTimeout(ctx, i.refreshTimeout)
	defer cancel()

	view := viewFromRecord(record)
	client, err := i.factory(record, password)
	if err != nil {
		return failedView(view, ServerStateUnreachable, err)
	}
	defer client.CloseIdleConnections()

	if err := client.Login(probeCtx); err != nil {
		return failedView(view, classifyError(err, false), err)
	}
	overview, err := client.Overview(probeCtx)
	if err != nil {
		return failedView(view, classifyError(err, false), err)
	}
	view.State = ServerStateOnline
	view.ClientCount = overview.OnlineClients
	view.MappingCount = overview.ActiveMappings
	view.LastRefresh = i.now().UTC()
	view.LastError = ""
	return view, nil
}

func (i *Inventory) refreshFailure(entry *serverRuntime, err error) (ServerView, error) {
	state := classifyError(err, false)
	entry.mu.Lock()
	entry.view.State = state
	entry.view.LastError = err.Error()
	if state == ServerStateAuthError {
		if entry.client != nil {
			entry.client.CloseIdleConnections()
		}
		entry.client = nil
		entry.clientBaseURL = ""
		entry.clientUsername = ""
		entry.clientTLSSkipVerify = false
		entry.clientPasswordEnc = ""
	}
	view := entry.view
	entry.mu.Unlock()
	return view, &ServerConnectionError{State: state, Err: err}
}

func (i *Inventory) viewOr(fallback ServerView) ServerView {
	if fallback.ID == "" {
		return fallback
	}
	if view, ok := i.Get(fallback.ID); ok {
		return view
	}
	return fallback
}

func (i *Inventory) validateFields(nameInput, baseURLInput, usernameInput string) (string, string, string, error) {
	name := strings.TrimSpace(nameInput)
	if name == "" {
		return "", "", "", &ValidationError{Message: "name is required"}
	}
	baseURL, err := normalizeServerBaseURL(baseURLInput)
	if err != nil {
		return "", "", "", err
	}
	username := strings.TrimSpace(usernameInput)
	if username == "" {
		return "", "", "", &ValidationError{Message: "username is required"}
	}
	return name, baseURL, username, nil
}

func (i *Inventory) ensureUniqueName(name, excludeID string) error {
	if i.store == nil {
		return nil
	}
	for _, record := range i.store.State().Servers {
		if record.ID == excludeID {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(record.Name), name) {
			return &ValidationError{Message: fmt.Sprintf("duplicate server name %q", name)}
		}
	}
	return nil
}

func (i *Inventory) passwordForUpdate(id, writeOnlyPassword string) ([]byte, error) {
	if strings.TrimSpace(writeOnlyPassword) != "" {
		return []byte(writeOnlyPassword), nil
	}
	password, err := i.store.ServerPassword(id)
	if err != nil {
		return nil, fmt.Errorf("read stored upstream password: %w", err)
	}
	return password, nil
}

func (i *Inventory) serverRecord(id string) (ServerRecord, bool) {
	if i == nil || i.store == nil {
		return ServerRecord{}, false
	}
	return serverRecordFrom(i.store.State().Servers, id)
}

func serverRecordFrom(records []ServerRecord, id string) (ServerRecord, bool) {
	for _, record := range records {
		if record.ID == id {
			return record, true
		}
	}
	return ServerRecord{}, false
}

// validationFromStoreError maps the store's atomic duplicate-name rejection to
// the same operator-facing validation class as the pre-flight name check.
func validationFromStoreError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "duplicate server name") {
		return &ValidationError{Message: err.Error()}
	}
	return err
}

func (i *Inventory) syncLocked(records []ServerRecord) {
	if i.entries == nil {
		i.entries = make(map[string]*serverRuntime)
	}
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		seen[record.ID] = struct{}{}
		entry := i.entries[record.ID]
		if entry == nil {
			i.entries[record.ID] = &serverRuntime{view: viewFromRecord(record)}
			continue
		}

		entry.mu.Lock()
		identityChanged := entry.view.BaseURL != record.BaseURL ||
			entry.view.UpstreamUsername != record.UpstreamUsername ||
			entry.view.TLSSkipVerify != record.TLSSkipVerify
		passwordChanged := entry.client != nil && entry.clientPasswordEnc != record.UpstreamPasswordEnc
		var staleClient *upstream.Client
		if identityChanged {
			// Cached version/counts belong to the previous upstream identity.
			entry.view.Version = ""
			entry.view.Mode = ""
			entry.view.ClientCount = 0
			entry.view.MappingCount = 0
			entry.view.LastRefresh = time.Time{}
			entry.view.LastError = ""
			entry.view.State = ServerStateUnknown
		}
		if identityChanged || passwordChanged {
			staleClient = entry.client
			entry.client = nil
			entry.clientBaseURL = ""
			entry.clientUsername = ""
			entry.clientTLSSkipVerify = false
			entry.clientPasswordEnc = ""
		}
		entry.view.ID = record.ID
		entry.view.Name = record.Name
		entry.view.BaseURL = record.BaseURL
		entry.view.UpstreamUsername = record.UpstreamUsername
		entry.view.TLSSkipVerify = record.TLSSkipVerify
		entry.view.CreatedAt = record.CreatedAt
		entry.view.UpdatedAt = record.UpdatedAt
		entry.mu.Unlock()

		if staleClient != nil {
			staleClient.CloseIdleConnections()
		}
	}
	for id, entry := range i.entries {
		if _, ok := seen[id]; ok {
			continue
		}
		entry.mu.Lock()
		entry.removed = true
		client := entry.client
		entry.client = nil
		entry.clientBaseURL = ""
		entry.clientUsername = ""
		entry.clientTLSSkipVerify = false
		entry.clientPasswordEnc = ""
		entry.mu.Unlock()
		if client != nil {
			client.CloseIdleConnections()
		}
		delete(i.entries, id)
	}
}

func (i *Inventory) appendAudit(ctx context.Context, entry AuditEntry) error {
	if i.audit == nil {
		return nil
	}
	entry.Actor = auditActor(ctx)
	if entry.Time.IsZero() {
		entry.Time = i.now().UTC()
	}
	return i.audit.Append(entry)
}

func (i *Inventory) appendAuditBestEffort(ctx context.Context, entry AuditEntry) {
	if err := i.appendAudit(ctx, entry); err != nil {
		i.log.Error("append manager audit entry", "action", entry.Action, "error", err)
	}
}

type auditActorKey struct{}

// WithAuditActor annotates a request context so inventory mutations record the
// manager administrator. It is exported so later task handlers can reuse it.
func WithAuditActor(ctx context.Context, actor string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, auditActorKey{}, actor)
}

func auditActor(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	actor, _ := ctx.Value(auditActorKey{}).(string)
	return actor
}

func viewFromRecord(record ServerRecord) ServerView {
	return ServerView{
		ID:               record.ID,
		Name:             record.Name,
		BaseURL:          record.BaseURL,
		UpstreamUsername: record.UpstreamUsername,
		TLSSkipVerify:    record.TLSSkipVerify,
		State:            ServerStateUnknown,
		CreatedAt:        record.CreatedAt,
		UpdatedAt:        record.UpdatedAt,
	}
}

func failedView(view ServerView, state string, err error) (ServerView, error) {
	view.State = state
	view.LastError = err.Error()
	return view, &ServerConnectionError{State: state, Err: err}
}

func classifyError(err error, optional bool) string {
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		switch apiError.Status {
		case 401, 403:
			return ServerStateAuthError
		case 404, 405, 501:
			if optional {
				return ServerStateUnsupported
			}
			return ServerStateUnreachable
		default:
			return ServerStateUnreachable
		}
	}
	return ServerStateUnreachable
}

func normalizeServerBaseURL(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", &ValidationError{Message: "base URL is required"}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// Do not echo the raw URL: it can contain userinfo or a token.
		return "", &ValidationError{Message: "base URL is not a valid URL"}
	}
	scheme := strings.ToLower(parsed.Scheme)
	if parsed.Opaque != "" || parsed.Host == "" || (scheme != "http" && scheme != "https") {
		return "", &ValidationError{Message: "base URL must be an absolute http or https URL"}
	}
	if parsed.User != nil {
		return "", &ValidationError{Message: "base URL must not include user information"}
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", &ValidationError{Message: "base URL must not include a query or fragment"}
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func newServerID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate server ID: %w", err)
	}
	return "srv_" + hex.EncodeToString(raw[:]), nil
}
