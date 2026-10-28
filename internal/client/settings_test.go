package client

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/webui/clientui"
)

func TestClientSettingsViewMasksSecretsAndReportsReadOnlyFields(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 2)
	cfg := loadClientConfig(t, cfgPath)
	cfg.Client.Name = "home"
	cfg.Credentials.Generation = 4
	cfg.Web = config.WebConfig{Username: "ops", PasswordHash: "$2a$10$not-used-because-web-is-disabled"}
	cfg.Bandwidth = config.BandwidthConfig{
		Up:           config.Bandwidth(1 << 20),
		Down:         config.Bandwidth(2 << 20),
		IgnoreClient: true,
	}
	socksAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	httpAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	cfg.Proxy = config.ProxyConfig{
		SOCKS5:     socksAddr,
		SOCKS5Auth: "user:secret",
		HTTP:       httpAddr,
	}
	require.NoError(t, config.SaveClient(cfgPath, cfg))

	cl, err := New(loadClientConfig(t, cfgPath), nil)
	require.NoError(t, err)
	defer cl.Close()

	settings := cl.Settings()
	require.Equal(t, "ops", settings.WebUsername)
	require.True(t, settings.WebPasswordConfigured)
	require.Equal(t, "home", settings.Name)
	require.Equal(t, int64(4), settings.Credentials.Generation)
	require.Equal(t, int64(0), settings.Credentials.ServerGeneration)
	require.False(t, settings.Credentials.Synced)
	require.Equal(t, clientui.MaskedSecret, settings.Credentials.CurrentAuthMasked)
	require.Equal(t, clientui.MaskedSecret, settings.Credentials.CurrentControlTokenMasked)

	readOnly := make(map[string]string, len(settings.ReadOnly))
	for _, field := range settings.ReadOnly {
		readOnly[field.Label] = field.Value
	}
	require.Equal(t, "127.0.0.1:443", readOnly["服务器地址"])
	require.Equal(t, cfgPath, readOnly["配置文件"])
	require.Equal(t, "未启用", readOnly["Web 监听"])
	require.Equal(t, "1 MiB/s", readOnly["带宽上行"])
	require.Equal(t, "2 MiB/s", readOnly["带宽下行"])
	require.Equal(t, "是", readOnly["忽略客户端带宽"])
	require.Equal(t, socksAddr+"（需要认证）", readOnly["SOCKS5 代理"])
	require.Equal(t, httpAddr, readOnly["HTTP 代理"])
	require.NotContains(t, readOnly["SOCKS5 代理"], "user:secret")
}
