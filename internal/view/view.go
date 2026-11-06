// Package view defines the read models shared by the server and client web
// interfaces. The types contain no secrets and are safe to serialize.
package view

import (
	"time"

	"hy2tun/internal/config"
)

// Exposure controls whether a mapping listens on public interfaces or only on
// loopback. The empty value is legacy and treated as public.
type Exposure string

const (
	// ExposurePublic binds the mapping's public listeners.
	ExposurePublic Exposure = "public"
	// ExposureLocal binds loopback-only listeners.
	ExposureLocal Exposure = "local"
)

// Valid reports whether e is a defined exposure mode. Empty is not valid for
// new input; callers treat it as the legacy public default.
func (e Exposure) Valid() bool {
	return e == ExposurePublic || e == ExposureLocal
}

// MappingView is one mapping together with its runtime state.
type MappingView struct {
	config.Mapping
	Status       string
	Error        string
	AssignedPort int
	// Exposure is the server-selected exposure. Empty is legacy and treated as
	// public. Clients must treat it as read-only.
	Exposure Exposure
	// UnavailableFamilies lists address families ("ipv4", "ipv6") that could
	// not be bound for this mapping. It is runtime diagnostics, not persisted
	// state.
	UnavailableFamilies []string
	// TxBytes counts payload bytes from the client toward the server.
	// RxBytes counts payload bytes from the server toward the client.
	TxBytes uint64
	RxBytes uint64
}

// ClientView is the last-known state of one registered client.
type ClientView struct {
	ID       string
	Name     string
	Version  string
	Revision int64
	Online   bool
	LastSeen time.Time
	Mappings []MappingView
	// Transport is the actual wire transport last used by this client
	// ("hy2" or "tcp"). It is diagnostic only: Online remains the authority for
	// whether the client is currently connected.
	Transport string

	// CredentialGeneration is the credential pair the client reported.
	// CredentialSynced reports whether it matches the server's current pair.
	// CredentialCapable reports whether the client accepts pushed credential
	// updates.
	CredentialGeneration int64
	CredentialSynced     bool
	CredentialCapable    bool
}

// CredentialResult reports the outcome of syncing a credential generation to
// one client.
type CredentialResult struct {
	ClientID   string
	Name       string
	Generation int64
	Status     string
	Error      string
}

// Overview summarizes the whole server.
type Overview struct {
	OnlineClients  int
	ActiveMappings int
	// TxBytes counts payload bytes from clients toward servers; RxBytes counts
	// payload bytes from servers toward clients.
	TxBytes uint64
	RxBytes uint64
	Listen  string
	Uptime  time.Duration
}

// TransportState is one worker's secret-free policy and session snapshot.
type TransportState struct {
	Mode            config.TransportMode `json:"mode"`
	ActiveTransport string               `json:"active_transport"`
	SwitchReason    string               `json:"switch_reason"`
	LastSwitchAt    time.Time            `json:"last_switch_at"`
	LastProbeAt     time.Time            `json:"last_probe_at"`
	LastProbeError  string               `json:"last_probe_error"`
	SwitchCount     uint64               `json:"switch_count"`
}

// ClientStatus includes an internal transport snapshot; HTTP flattening is
// deliberately left to the web read-model adapter.
type ClientStatus struct {
	Name      string
	Server    string
	Version   string
	Connected bool
	Uptime    time.Duration
	// TxBytes counts payload bytes from the client toward the server.
	// RxBytes counts payload bytes from the server toward the client.
	TxBytes uint64
	RxBytes uint64

	// CredentialGeneration is the credential generation the client currently
	// holds. ServerCredentialGeneration is the generation reported by the
	// server's last HelloAck; 0 means the server predates rotation and its
	// generation is unknown. CredentialSynced reports whether the last
	// HelloAck confirmed the local pair matches the server's current pair.
	CredentialGeneration       int64
	ServerCredentialGeneration int64
	CredentialSynced           bool
	Transport                  TransportState
}

// LogEntry is one client log line exposed by the client web UI.
type LogEntry struct {
	Time    time.Time
	Level   string
	Message string
}
