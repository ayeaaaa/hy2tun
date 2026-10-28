package client

import (
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
}
