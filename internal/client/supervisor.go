package client

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"hy2tun/internal/config"
	"hy2tun/internal/credentials"
	"hy2tun/internal/version"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/clientui"
)

// Supervisor owns the shared client web console and one independent worker per
// configured server. It implements the legacy single-server web API by
// delegating to the default (or first) worker; multi-server routes select a
// worker explicitly.
type Supervisor struct {
	cfg  *config.ClientConfig
	log  *slog.Logger
	logs *logBuffer
	web  *clientui.Server

	configMu sync.Mutex
	proxyMu  sync.Mutex
	workers  map[string]*Client
	order    []string

	started   atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

var _ clientui.API = (*Supervisor)(nil)

// NewSupervisor validates cfg, generates missing per-server client IDs, creates
// workers, applies per-server proxy listeners, and creates the shared web
// console. It does not connect to any server until Start.
func NewSupervisor(cfg *config.ClientConfig, log *slog.Logger) (*Supervisor, error) {
	if cfg == nil {
		return nil, fmt.Errorf("client config is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	logs := newLogBuffer(logBufferSize)
	log = slog.New(teeHandler{
		primary:   log.Handler(),
		secondary: slog.NewJSONHandler(logs, nil),
	})

	changed := false
	if cfg.IsMulti() {
		for i := range cfg.Servers {
			if cfg.Servers[i].Client.ID == "" {
				id, err := newClientID()
				if err != nil {
					return nil, err
				}
				cfg.Servers[i].Client.ID = id
				changed = true
			}
		}
	} else if cfg.Client.ID == "" {
		id, err := newClientID()
		if err != nil {
			return nil, err
		}
		cfg.Client.ID = id
		changed = true
	}
	if changed && cfg.ConfigPath != "" {
		if err := config.SaveClient(cfg.ConfigPath, cfg); err != nil {
			return nil, fmt.Errorf("save generated client IDs: %w", err)
		}
	}

	sup := &Supervisor{
		cfg:     cfg,
		log:     log,
		logs:    logs,
		workers: make(map[string]*Client),
		closed:  make(chan struct{}),
	}
	servers := cfg.ServerConfigs()
	for _, server := range servers {
		workerCfg := server.ToClientConfig(cfg.Web, cfg.Log, cfg.ConfigPath)
		workerCfg.Web.Listen = ""
		worker, err := NewWorker(workerCfg, log)
		if err != nil {
			sup.closeWorkers()
			return nil, fmt.Errorf("create server %q: %w", server.Name, err)
		}
		name := server.Name
		worker.SetPersist(func(next *config.ClientConfig) error {
			return sup.persistWorker(name, next)
		})
		if err := worker.ApplyProxy(server.Proxy); err != nil {
			log.Warn("server proxy unavailable at startup", "server", name, "error", err)
		}
		sup.workers[name] = worker
		sup.order = append(sup.order, name)
	}

	if cfg.WebEnabled() {
		sessionKey, err := loadClientWebSessionKey(cfg, log)
		if err != nil {
			sup.closeWorkers()
			return nil, err
		}
		web, err := clientui.New(sup, cfg.Web, sessionKey, log)
		if err != nil {
			sup.closeWorkers()
			return nil, fmt.Errorf("create client web console: %w", err)
		}
		sup.web = web
	}
	return sup, nil
}

// Start starts the shared web console and every worker. A worker failure does
// not stop the others; Start blocks until ctx is canceled or Close is called.
func (s *Supervisor) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.started.CompareAndSwap(false, true) {
		return fmt.Errorf("client already started")
	}
	if s.web != nil {
		if err := s.web.Start(); err != nil {
			return fmt.Errorf("start client web console: %w", err)
		}
	}

	var wg sync.WaitGroup
	for _, name := range s.order {
		worker := s.workers[name]
		wg.Add(1)
		go func(name string, worker *Client) {
			defer wg.Done()
			if err := worker.Start(ctx); err != nil {
				s.log.Warn("server worker stopped", "server", name, "error", err)
			}
		}(name, worker)
	}
	wg.Wait()
	return nil
}

// Close stops every worker and the shared web console.
func (s *Supervisor) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.closeWorkers()
		if s.web != nil {
			if err := s.web.Close(); err != nil {
				s.log.Warn("close client web console", "error", err)
			}
		}
	})
	return s.closeErr
}

func (s *Supervisor) closeWorkers() {
	for _, name := range s.order {
		if worker := s.workers[name]; worker != nil {
			_ = worker.Close()
		}
	}
}

// WebAddr returns the shared web console address.
func (s *Supervisor) WebAddr() string {
	if s.web != nil {
		return s.web.Addr()
	}
	return s.cfg.Web.Listen
}

// ConfigPath returns the shared client config path.
func (s *Supervisor) ConfigPath() string {
	return s.cfg.ConfigPath
}

func (s *Supervisor) defaultWorker() *Client {
	if s == nil {
		return nil
	}
	if worker := s.workers["default"]; worker != nil {
		return worker
	}
	if len(s.order) > 0 {
		return s.workers[s.order[0]]
	}
	return nil
}

// Status implements the legacy single-server web API.
func (s *Supervisor) Status() view.ClientStatus {
	if worker := s.defaultWorker(); worker != nil {
		return worker.Status()
	}
	return view.ClientStatus{Version: version.Version}
}

func (s *Supervisor) Mappings() []view.MappingView {
	if worker := s.defaultWorker(); worker != nil {
		return worker.Mappings()
	}
	return nil
}

func (s *Supervisor) AddMapping(mapping config.Mapping) error {
	worker := s.defaultWorker()
	if worker == nil {
		return fmt.Errorf("client is not initialized")
	}
	return worker.AddMapping(mapping)
}

func (s *Supervisor) UpdateMapping(mapping config.Mapping) error {
	worker := s.defaultWorker()
	if worker == nil {
		return fmt.Errorf("client is not initialized")
	}
	return worker.UpdateMapping(mapping)
}

func (s *Supervisor) DeleteMapping(id string) error {
	worker := s.defaultWorker()
	if worker == nil {
		return fmt.Errorf("client is not initialized")
	}
	return worker.DeleteMapping(id)
}

func (s *Supervisor) Logs(level string, limit int) []view.LogEntry {
	if s.logs == nil {
		return nil
	}
	return s.logs.Entries(level, limit)
}

func (s *Supervisor) Settings() clientui.SettingsView {
	worker := s.defaultWorker()
	if worker == nil {
		return clientui.SettingsView{}
	}
	settings := worker.Settings()
	settings.WebUsername = s.cfg.Web.Username
	settings.WebPasswordConfigured = s.cfg.Web.PasswordHash != ""
	return settings
}

func (s *Supervisor) UpdateWebConfig(username, passwordHash string) error {
	s.configMu.Lock()
	next := cloneSupervisorConfig(s.cfg)
	next.Web.Username = username
	next.Web.PasswordHash = passwordHash
	if err := next.Validate(); err != nil {
		s.configMu.Unlock()
		return err
	}
	if err := config.SaveClient(next.ConfigPath, next); err != nil {
		s.configMu.Unlock()
		return err
	}
	s.cfg = next
	s.configMu.Unlock()
	for _, worker := range s.workers {
		worker.setWebConfig(next.Web)
	}
	return nil
}

func (s *Supervisor) UpdateName(name string) error {
	worker := s.defaultWorker()
	if worker == nil {
		return fmt.Errorf("client is not initialized")
	}
	return worker.UpdateName(name)
}

func (s *Supervisor) RotateLocalCredentials(generation int64, auth, controlToken string) error {
	worker := s.defaultWorker()
	if worker == nil {
		return fmt.Errorf("client is not initialized")
	}
	return worker.RotateLocalCredentials(generation, auth, controlToken)
}

func (s *Supervisor) CredentialsSnapshot() *credentials.Set {
	worker := s.defaultWorker()
	if worker == nil {
		return nil
	}
	return worker.CredentialsSnapshot()
}

// UpdateProxy atomically validates, binds, and persists a per-server proxy
// configuration. Other servers and the web listener are checked first; if the
// worker cannot bind or the config cannot be saved, the previous worker state
// is restored.
func (s *Supervisor) UpdateProxy(name string, cfg config.ProxyConfig) error {
	s.proxyMu.Lock()
	defer s.proxyMu.Unlock()

	worker := s.workers[name]
	if worker == nil {
		return fmt.Errorf("server %q not found", name)
	}
	if cfg.Enabled && cfg.SOCKS5 == "" && cfg.HTTP == "" {
		return fmt.Errorf("proxy.enabled requires socks5 or http")
	}
	for _, value := range []struct {
		name string
		addr string
	}{{"socks5", cfg.SOCKS5}, {"http", cfg.HTTP}} {
		if value.addr == "" {
			continue
		}
		if err := validateProxyAddress(value.name, value.addr); err != nil {
			return err
		}
	}

	s.configMu.Lock()
	next := cloneSupervisorConfig(s.cfg)
	if next.IsMulti() {
		found := false
		for i := range next.Servers {
			if next.Servers[i].Name != name {
				continue
			}
			next.Servers[i].Proxy = cfg
			found = true
			break
		}
		if !found {
			s.configMu.Unlock()
			return fmt.Errorf("server %q not found", name)
		}
	} else {
		next.Proxy = cfg
	}
	oldCfg := worker.cfg.Proxy
	if err := next.Validate(); err != nil {
		s.configMu.Unlock()
		return err
	}
	s.configMu.Unlock()

	if err := worker.ApplyProxy(cfg); err != nil {
		return err
	}
	if err := config.SaveClient(next.ConfigPath, next); err != nil {
		if restoreErr := worker.ApplyProxy(oldCfg); restoreErr != nil {
			s.log.Warn("restore proxy after config save failure", "server", name, "error", restoreErr)
		}
		return err
	}
	s.configMu.Lock()
	s.cfg = next
	s.configMu.Unlock()
	return nil
}

// persistWorker merges a worker edit into the shared multi-server config. The
// worker has already validated and prepared its single-server snapshot.
func (s *Supervisor) persistWorker(name string, workerCfg *config.ClientConfig) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()

	next := cloneSupervisorConfig(s.cfg)
	if next.IsMulti() {
		found := false
		for i := range next.Servers {
			if next.Servers[i].Name != name {
				continue
			}
			next.Servers[i] = serverFromWorker(name, workerCfg)
			found = true
			break
		}
		if !found {
			return fmt.Errorf("server %q not found", name)
		}
	} else {
		next.Server = workerCfg.Server
		next.Auth = workerCfg.Auth
		next.TLS = workerCfg.TLS
		next.Obfs = workerCfg.Obfs
		next.Bandwidth = workerCfg.Bandwidth
		next.Transport = cloneClientTransport(workerCfg.Transport)
		next.Control = workerCfg.Control
		next.Client = workerCfg.Client
		next.Credentials = workerCfg.Credentials
		next.Mappings = append([]config.Mapping(nil), workerCfg.Mappings...)
		next.Proxy = workerCfg.Proxy
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := config.SaveClient(next.ConfigPath, next); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

func serverFromWorker(name string, workerCfg *config.ClientConfig) config.ClientServerConfig {
	return config.ClientServerConfig{
		Name:        name,
		Server:      workerCfg.Server,
		Auth:        workerCfg.Auth,
		TLS:         workerCfg.TLS,
		Obfs:        workerCfg.Obfs,
		Bandwidth:   workerCfg.Bandwidth,
		Transport:   cloneClientTransport(workerCfg.Transport),
		Control:     workerCfg.Control,
		Client:      workerCfg.Client,
		Credentials: workerCfg.Credentials,
		Mappings:    append([]config.Mapping(nil), workerCfg.Mappings...),
		Proxy:       workerCfg.Proxy,
	}
}

func cloneSupervisorConfig(cfg *config.ClientConfig) *config.ClientConfig {
	if cfg == nil {
		return nil
	}
	next := *cfg
	next.Mappings = append([]config.Mapping(nil), cfg.Mappings...)
	next.Transport = cloneClientTransport(cfg.Transport)
	next.Servers = make([]config.ClientServerConfig, len(cfg.Servers))
	for i, server := range cfg.Servers {
		next.Servers[i] = server
		next.Servers[i].Mappings = append([]config.Mapping(nil), server.Mappings...)
		next.Servers[i].Transport = cloneClientTransport(server.Transport)
	}
	return &next
}

func cloneClientTransport(value *config.ClientTransportConfig) *config.ClientTransportConfig {
	if value == nil {
		return nil
	}
	out := *value
	if value.UDP != nil {
		udp := *value.UDP
		out.UDP = &udp
	}
	return &out
}
