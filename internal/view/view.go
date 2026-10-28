// Package view defines the read models shared by the server and client web
// interfaces. The types contain no secrets and are safe to serialize.
package view

import (
	"time"

	"hy2tun/internal/config"
)

// MappingView is one mapping together with its runtime state.
type MappingView struct {
	config.Mapping
	Status       string
	Error        string
	AssignedPort int
	TxBytes      uint64
	RxBytes      uint64
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
	TxBytes        uint64
	RxBytes        uint64
	Listen         string
	Uptime         time.Duration
}

// ClientStatus is the local client's connection status.
type ClientStatus struct {
	Name      string
	Server    string
	Version   string
	Connected bool
	Uptime    time.Duration
	TxBytes   uint64
	RxBytes   uint64
}

// LogEntry is one client log line exposed by the client web UI.
type LogEntry struct {
	Time    time.Time
	Level   string
	Message string
}
