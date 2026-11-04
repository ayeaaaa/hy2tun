package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Catches incorrect legacy defaults, transport permissions, and acceptance of
// unsupported modes; expectations are independent of Effective and Validate.
func TestTransportModeDefaultAndAllows(t *testing.T) {
	require.Equal(t, ModeHY2, TransportMode("").Effective())
	require.True(t, ModeAuto.AllowsHY2())
	require.True(t, ModeAuto.AllowsTCP())
	require.False(t, ModeTCP.AllowsHY2())
	require.Error(t, TransportMode("invalid").Validate())

	for _, tt := range []struct {
		mode      TransportMode
		effective TransportMode
		hy2       bool
		tcp       bool
		valid     bool
	}{
		{"", "hy2", true, false, true},
		{"hy2", "hy2", true, false, true},
		{"tcp", "tcp", false, true, true},
		{"hy2+tcp", "hy2+tcp", true, true, true},
		{"invalid", "invalid", false, false, false},
		{"TCP", "TCP", false, false, false},
		{" ", " ", false, false, false},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			require.Equal(t, tt.effective, tt.mode.Effective())
			require.Equal(t, tt.hy2, tt.mode.AllowsHY2())
			require.Equal(t, tt.tcp, tt.mode.AllowsTCP())
			if tt.valid {
				require.NoError(t, tt.mode.Validate())
			} else {
				require.Error(t, tt.mode.Validate())
			}
		})
	}
}

// Catches sorting/merging before port selection and losing IPv6 brackets.
func TestTCPAddressUsesFirstSpecifiedPort(t *testing.T) {
	for _, tt := range []struct {
		address string
		want    string
	}{
		{"host:30002-30005,30000", "host:30002"},
		{"[2001:db8::1]:30002-30005,30000", "[2001:db8::1]:30002"},
		{"host:30005,30000-30002", "host:30005"},
		{"host:443-443,443", "host:443"},
		{" host:443 ", "host:443"},
	} {
		t.Run(tt.address, func(t *testing.T) {
			client := &ClientConfig{Mode: ModeAuto, Server: tt.address}
			got, err := client.TCPServerAddress()
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.address, client.Server)

			server := &ServerConfig{Mode: ModeAuto, Listen: tt.address}
			got, err = server.TCPListenAddress()
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.address, server.Listen)
		})
	}
	for _, tt := range []struct {
		listen string
		want   string
	}{
		{":30002-30005,30000", ":30002"},
		{":0", ":0"},
	} {
		server := &ServerConfig{Listen: tt.listen}
		got, err := server.TCPListenAddress()
		require.NoError(t, err)
		require.Equal(t, tt.want, got)
	}
}

func TestTCPAddressRejectsInvalidInput(t *testing.T) {
	for _, address := range []string{"", "missing-port", "host:30005-30002", "host:443,", "host:65536"} {
		t.Run(address, func(t *testing.T) {
			got, err := (&ClientConfig{Server: address}).TCPServerAddress()
			require.Error(t, err)
			require.Empty(t, got)
			got, err = (&ServerConfig{Listen: address}).TCPListenAddress()
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
	_, err := (&ClientConfig{Server: ":443"}).TCPServerAddress()
	require.Error(t, err)
	_, err = (&ClientConfig{Server: "host:0"}).TCPServerAddress()
	require.Error(t, err)
}

// Catches applying the effective mode back to saved legacy YAML, including
// per-server defaults, rather than leaving the optional raw field empty.
func TestModeLegacyYAMLNotMaterialized(t *testing.T) {
	for _, emptyMode := range []string{"", "mode: \"\"\n"} {
		t.Run("client/"+emptyMode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.yaml")
			require.NoError(t, os.WriteFile(path, []byte(emptyMode+`server: "host:443"
auth: pw
control:
  token: "0123456789abcdef"
`), 0600))
			cfg, err := LoadClient(path)
			require.NoError(t, err)
			require.Empty(t, cfg.Mode)
			require.Equal(t, ModeHY2, cfg.Mode.Effective())
			require.NoError(t, SaveClient(path, cfg))
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "mode:")
		})
		t.Run("server/"+emptyMode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.yaml")
			require.NoError(t, os.WriteFile(path, []byte(emptyMode+`listen: ":443"
auth:
  password: pw
control:
  token: "0123456789abcdef"
`), 0600))
			cfg, err := LoadServer(path)
			require.NoError(t, err)
			require.Empty(t, cfg.Mode)
			require.Equal(t, ModeHY2, cfg.Mode.Effective())
			require.NoError(t, SaveServer(path, cfg))
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "mode:")
		})
	}
	t.Run("multi", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client.yaml")
		require.NoError(t, os.WriteFile(path, []byte(`servers:
  - name: first
    server: "host:443"
    auth: pw
    control:
      token: "0123456789abcdef"
  - name: second
    mode: ""
    server: "host:8443"
    auth: pw
    control:
      token: "0123456789abcdef"
`), 0600))
		cfg, err := LoadClient(path)
		require.NoError(t, err)
		require.Empty(t, cfg.Mode)
		for _, server := range cfg.ServerConfigs() {
			require.Empty(t, server.Mode)
			require.Equal(t, ModeHY2, server.Mode.Effective())
		}
		require.NoError(t, SaveClient(path, cfg))
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "mode:")
	})
}

// Catches dropping mode in either conversion or allowing a top-level mode to
// override independent multi-server entries.
func TestModeMultiIndependent(t *testing.T) {
	cfg := multiServerTestConfig()
	cfg.Servers[0].Mode = ModeAuto
	cfg.Servers[1].Mode = ModeTCP
	require.NoError(t, cfg.Validate())
	servers := cfg.ServerConfigs()
	require.Len(t, servers, 2)
	require.Equal(t, ModeAuto, servers[0].Mode)
	require.Equal(t, ModeTCP, servers[1].Mode)
	for i, want := range []TransportMode{ModeAuto, ModeTCP} {
		worker := servers[i].ToClientConfig(cfg.Web, cfg.Log, "client.yaml")
		require.Equal(t, want, worker.Mode)
		require.NoError(t, worker.Validate())
	}
	servers[0].Mode = ModeHY2
	require.Equal(t, ModeAuto, cfg.Servers[0].Mode)
	for _, mode := range []TransportMode{ModeHY2, ModeTCP, ModeAuto, "invalid"} {
		cfg.Mode = mode
		require.ErrorContains(t, cfg.Validate(), "legacy")
	}
}

func TestModeSingleSnapshotPreservesMode(t *testing.T) {
	for _, mode := range []TransportMode{"", ModeHY2, ModeTCP, ModeAuto} {
		cfg := validClientConfig()
		cfg.Mode = mode
		require.NoError(t, cfg.Validate())
		servers := cfg.ServerConfigs()
		require.Len(t, servers, 1)
		require.Equal(t, mode, servers[0].Mode)
		worker := servers[0].ToClientConfig(cfg.Web, cfg.Log, "")
		require.Equal(t, mode, worker.Mode)
		require.NoError(t, worker.Validate())
	}
}

func TestModeConfigValidation(t *testing.T) {
	for _, mode := range []TransportMode{"", ModeHY2, ModeTCP, ModeAuto, "invalid"} {
		t.Run(string(mode), func(t *testing.T) {
			client := validClientConfig()
			client.Mode = mode
			server := validServerConfig()
			server.Mode = mode
			multi := multiServerTestConfig()
			multi.Servers[1].Mode = mode
			if mode == "invalid" {
				require.ErrorContains(t, client.Validate(), "mode")
				require.ErrorContains(t, server.Validate(), "mode")
				require.ErrorContains(t, multi.Validate(), "servers[1].mode")
			} else {
				require.NoError(t, client.Validate())
				require.NoError(t, server.Validate())
				require.NoError(t, multi.Validate())
			}
		})
	}
}

// Catches missing/wrong serialization tags on each of the three mode fields.
func TestModeSerialization(t *testing.T) {
	for _, mode := range []TransportMode{"", ModeHY2, ModeTCP, ModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			for _, cfg := range []any{
				&ClientConfig{Mode: mode},
				&ClientServerConfig{Mode: mode},
				&ServerConfig{Mode: mode},
			} {
				yamlData, err := yaml.Marshal(cfg)
				require.NoError(t, err)
				jsonData, err := json.Marshal(cfg)
				require.NoError(t, err)
				for _, data := range []struct {
					raw       []byte
					unmarshal func([]byte, any) error
				}{
					{yamlData, yaml.Unmarshal},
					{jsonData, json.Unmarshal},
				} {
					var fields map[string]any
					require.NoError(t, data.unmarshal(data.raw, &fields))
					if mode == "" {
						require.NotContains(t, fields, "mode")
					} else {
						require.Equal(t, string(mode), fields["mode"])
					}
					switch cfg.(type) {
					case *ClientConfig:
						var loaded ClientConfig
						require.NoError(t, data.unmarshal(data.raw, &loaded))
						require.Equal(t, mode, loaded.Mode)
					case *ClientServerConfig:
						var loaded ClientServerConfig
						require.NoError(t, data.unmarshal(data.raw, &loaded))
						require.Equal(t, mode, loaded.Mode)
					case *ServerConfig:
						var loaded ServerConfig
						require.NoError(t, data.unmarshal(data.raw, &loaded))
						require.Equal(t, mode, loaded.Mode)
					}
				}
			}
		})
	}
}

// Catches checking only normalized unique port counts: degenerate ranges and
// repeated single-port sets are still forbidden input syntax in pure TCP mode.
func TestTCPModeRejectsHopping(t *testing.T) {
	for _, address := range []string{
		"host:30002-30005,30000", "host:443,8443", "host:443-443", "host:443,443",
	} {
		t.Run(address, func(t *testing.T) {
			client := validClientConfig()
			client.Mode, client.Server = ModeTCP, address
			require.ErrorContains(t, client.Validate(), "tcp")
			server := validServerConfig()
			server.Mode, server.Listen = ModeTCP, address
			require.ErrorContains(t, server.Validate(), "tcp")
			multi := multiServerTestConfig()
			multi.Servers[1].Mode, multi.Servers[1].Server = ModeTCP, address
			require.ErrorContains(t, multi.Validate(), "servers[1]")
		})
	}
	for _, tt := range []struct {
		name string
		udp  ClientTransportUDPConfig
	}{
		{"fixed", ClientTransportUDPConfig{HopInterval: Duration(30 * time.Second)}},
		{"min", ClientTransportUDPConfig{MinHopInterval: Duration(15 * time.Second)}},
		{"max", ClientTransportUDPConfig{MaxHopInterval: Duration(45 * time.Second)}},
		{"random", ClientTransportUDPConfig{MinHopInterval: Duration(15 * time.Second), MaxHopInterval: Duration(45 * time.Second)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := validClientConfig()
			client.Mode = ModeTCP
			client.Transport = &ClientTransportConfig{UDP: &tt.udp}
			require.ErrorContains(t, client.Validate(), "tcp")
			multi := multiServerTestConfig()
			multi.Servers[1].Mode = ModeTCP
			multi.Servers[1].Transport = &ClientTransportConfig{UDP: &tt.udp}
			require.ErrorContains(t, multi.Validate(), "servers[1]")
			require.ErrorContains(t, multi.Validate(), "tcp")
		})
	}
}

func TestTCPModeAcceptsSinglePort(t *testing.T) {
	for _, transport := range []*ClientTransportConfig{nil, {}, {UDP: &ClientTransportUDPConfig{}}} {
		client := validClientConfig()
		client.Mode, client.Transport = ModeTCP, transport
		require.NoError(t, client.Validate())
		multi := multiServerTestConfig()
		multi.Servers[1].Mode, multi.Servers[1].Transport = ModeTCP, transport
		require.NoError(t, multi.Validate())
	}
	for _, listen := range []string{":443", "[::1]:443", ":0"} {
		server := validServerConfig()
		server.Mode, server.Listen = ModeTCP, listen
		require.NoError(t, server.Validate())
	}
}

func TestTCPModeHY2ModesKeepHoppingSupport(t *testing.T) {
	for _, mode := range []TransportMode{"", ModeHY2, ModeAuto} {
		for _, address := range []string{"host:30002-30005,30000", "host:443-443", "host:443,443"} {
			client := validClientConfig()
			client.Mode, client.Server = mode, address
			require.NoError(t, client.Validate())
			server := validServerConfig()
			server.Mode, server.Listen = mode, address
			require.NoError(t, server.Validate())
			multi := multiServerTestConfig()
			multi.Servers[1].Mode, multi.Servers[1].Server = mode, address
			require.NoError(t, multi.Validate())
		}
		client := validClientConfig()
		client.Mode, client.Server = mode, "host:30002-30005,30000"
		client.Transport = &ClientTransportConfig{UDP: &ClientTransportUDPConfig{HopInterval: Duration(30 * time.Second)}}
		require.NoError(t, client.Validate())
		multi := multiServerTestConfig()
		multi.Servers[1].Mode, multi.Servers[1].Server = mode, client.Server
		multi.Servers[1].Transport = client.Transport
		require.NoError(t, multi.Validate())
	}
}
