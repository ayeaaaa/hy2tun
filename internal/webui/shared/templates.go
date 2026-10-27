package shared

import (
	"fmt"
	"html/template"
	"io/fs"
	"strings"
	"time"
)

// ParsePage parses layout and page templates with the shared function map.
// Callers execute a named template, usually "layout" for pages.
func ParsePage(fsys fs.FS, files ...string) (*template.Template, error) {
	if fsys == nil {
		return nil, fmt.Errorf("shared: template filesystem is nil")
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("shared: no template files")
	}
	return template.New("root").Funcs(TemplateFuncs()).ParseFS(fsys, files...)
}

// TemplateFuncs returns the formatting helpers shared by both web consoles.
func TemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"bytes":       FormatBytes,
		"duration":    FormatDuration,
		"time":        FormatTime,
		"statusLabel": StatusLabel,
		"statusClass": StatusClass,
		"upper":       strings.ToUpper,
		"join":        strings.Join,
	}
}

// FormatBytes formats a byte count with binary units.
func FormatBytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(n)
	index := -1
	for value >= 1024 && index < len(units)-1 {
		value /= 1024
		index++
	}
	switch {
	case value >= 100:
		return fmt.Sprintf("%.0f %s", value, units[index])
	case value >= 10:
		return fmt.Sprintf("%.1f %s", value, units[index])
	default:
		return fmt.Sprintf("%.2f %s", value, units[index])
	}
}

// FormatDuration formats a duration as a compact Chinese duration.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d / time.Second)
	days := total / 86400
	hours := total % 86400 / 3600
	minutes := total % 3600 / 60
	seconds := total % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d天 %d小时", days, hours)
	case hours > 0:
		return fmt.Sprintf("%d小时 %d分", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%d分 %d秒", minutes, seconds)
	default:
		return fmt.Sprintf("%d秒", seconds)
	}
}

// FormatTime formats a timestamp for display. The zero value is an em dash.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// StatusLabel returns the Chinese label for a mapping status.
func StatusLabel(status string) string {
	switch status {
	case "running":
		return "运行中"
	case "pending":
		return "待启动"
	case "failed":
		return "失败"
	case "disabled":
		return "已禁用"
	case "offline":
		return "离线"
	default:
		return "未知"
	}
}

// StatusClass returns the CSS modifier for a mapping status.
func StatusClass(status string) string {
	switch status {
	case "running":
		return "ok"
	case "pending":
		return "warn"
	case "failed":
		return "bad"
	case "disabled", "offline":
		return "muted"
	default:
		return "muted"
	}
}
