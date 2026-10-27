package shared

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFormatBytes(t *testing.T) {
	require.Equal(t, "0 B", FormatBytes(0))
	require.Equal(t, "512 B", FormatBytes(512))
	require.Equal(t, "1.00 KiB", FormatBytes(1024))
	require.Equal(t, "1.50 KiB", FormatBytes(1536))
	require.Equal(t, "1.00 MiB", FormatBytes(1024*1024))
}

func TestFormatDuration(t *testing.T) {
	require.Equal(t, "0秒", FormatDuration(0))
	require.Equal(t, "5秒", FormatDuration(5*time.Second))
	require.Equal(t, "1分 30秒", FormatDuration(90*time.Second))
	require.Equal(t, "1小时 30分", FormatDuration(90*time.Minute))
	require.Equal(t, "1天 0小时", FormatDuration(24*time.Hour))
}

func TestStatusHelpers(t *testing.T) {
	require.Equal(t, "运行中", StatusLabel("running"))
	require.Equal(t, "待启动", StatusLabel("pending"))
	require.Equal(t, "失败", StatusLabel("failed"))
	require.Equal(t, "已禁用", StatusLabel("disabled"))
	require.Equal(t, "离线", StatusLabel("offline"))
	require.Equal(t, "未知", StatusLabel("other"))

	require.Equal(t, "ok", StatusClass("running"))
	require.Equal(t, "warn", StatusClass("pending"))
	require.Equal(t, "bad", StatusClass("failed"))
	require.Equal(t, "muted", StatusClass("disabled"))
	require.Equal(t, "muted", StatusClass("offline"))
	require.Equal(t, "muted", StatusClass("other"))
}

func TestFormatTime(t *testing.T) {
	require.Equal(t, "—", FormatTime(time.Time{}))
	require.Equal(t, "2026-10-27 12:34:56", FormatTime(time.Date(2026, 10, 27, 12, 34, 56, 0, time.Local)))
}
