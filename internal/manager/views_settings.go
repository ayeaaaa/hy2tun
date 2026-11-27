package manager

import (
	"strconv"
	"strings"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/manager/upstream"
)

// settingsMaskedSecret is the fixed placeholder for a stored upstream secret.
// It deliberately reveals neither the value nor its length.
const settingsMaskedSecret = "********"

// settingsDefaultSyncMode is the pre-selected rotation sync mode. The existing
// server defaults an empty mode to manual; the manager makes the choice
// explicit so an operator cannot rotate silently in the wrong mode.
const settingsDefaultSyncMode = "auto"

// serverSettingsPageData backs GET /servers/{id}/settings. It contains only
// secret-free upstream settings and write-only form defaults.
type serverSettingsPageData struct {
	pageBase
	Server      ServerView
	Settings    serverSettingsView
	Form        settingsFormValues
	ClientRows  []credentialClientView
	Rotation    *settingsRotationView
	HasSettings bool
	LoadedAt    time.Time
	LoadError   string
	Error       string
	Warning     string
	Notice      string
}

// settingsFormValues holds non-secret operator input so a failed rendered form
// can be shown again. Password/token fields are deliberately absent.
type settingsFormValues struct {
	Kind                  string
	Mode                  string
	AllowText             string
	DenyText              string
	AutoRange             string
	OfflineReserveSeconds int64
	WebUsername           string
	TrustProxy            bool
	SyncMode              string
}

func settingsFormFromSettings(settings upstream.Settings) settingsFormValues {
	return settingsFormValues{
		Mode:                  string(settings.Mode),
		AllowText:             strings.Join(settings.Ports.Allow, ", "),
		DenyText:              strings.Join(settings.Ports.Deny, ", "),
		AutoRange:             settings.Ports.AutoRange,
		OfflineReserveSeconds: settings.Ports.OfflineReserveSeconds,
		WebUsername:           settings.WebUsername,
		TrustProxy:            settings.TrustProxy,
		SyncMode:              settingsDefaultSyncMode,
	}
}

// mergeSettingsForm overlays the submitted, non-secret values for one form on
// top of freshly loaded settings. Unrelated forms keep their current values.
func mergeSettingsForm(base, submitted settingsFormValues) settingsFormValues {
	switch submitted.Kind {
	case "mode":
		if strings.TrimSpace(submitted.Mode) != "" {
			base.Mode = submitted.Mode
		}
	case "ports":
		base.AllowText = submitted.AllowText
		base.DenyText = submitted.DenyText
		base.AutoRange = submitted.AutoRange
		base.OfflineReserveSeconds = submitted.OfflineReserveSeconds
	case "web":
		base.WebUsername = submitted.WebUsername
	case "proxy":
		base.TrustProxy = submitted.TrustProxy
	case "rotate":
		base.SyncMode = submitted.SyncMode
	}
	return base
}

// serverSettingsView decorates the upstream settings snapshot with display
// helpers. It never contains a plaintext secret: masks from the upstream are
// re-masked defensively.
type serverSettingsView struct {
	upstream.Settings
}

func newServerSettingsView(settings upstream.Settings) serverSettingsView {
	settings.Ports.OfflineReserve = time.Duration(settings.Ports.OfflineReserveSeconds) * time.Second
	settings.Credentials.CurrentAuthMasked = maskSettingsSecret(settings.Credentials.CurrentAuthMasked)
	settings.Credentials.CurrentControlTokenMasked = maskSettingsSecret(settings.Credentials.CurrentControlTokenMasked)
	settings.Credentials.PreviousAuthMasked = maskSettingsSecret(settings.Credentials.PreviousAuthMasked)
	settings.Credentials.PreviousControlTokenMasked = maskSettingsSecret(settings.Credentials.PreviousControlTokenMasked)
	return serverSettingsView{Settings: settings}
}

func maskSettingsSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return settingsMaskedSecret
}

func transportModeLabel(mode config.TransportMode) string {
	switch mode.Effective() {
	case config.ModeHY2:
		return "HY2"
	case config.ModeTCP:
		return "TCP（UDP over TCP）"
	case config.ModeAuto:
		return "HY2 + TCP（自动回退）"
	default:
		return string(mode)
	}
}

func (v serverSettingsView) ModeLabel() string { return transportModeLabel(v.Mode) }

func (v serverSettingsView) WebPasswordLabel() string {
	if v.WebPasswordConfigured {
		return "已配置"
	}
	return "未配置"
}

func (v serverSettingsView) AuthTypeLabel() string {
	switch strings.ToLower(strings.TrimSpace(v.AuthType)) {
	case "password":
		return "HY2 密码"
	case "":
		return "—"
	default:
		return v.AuthType
	}
}

func (v serverSettingsView) TrustProxyLabel() string {
	if v.TrustProxy {
		return "信任"
	}
	return "不信任"
}

func (v serverSettingsView) CurrentAuthMask() string {
	return valueOrDash(v.Credentials.CurrentAuthMasked)
}

func (v serverSettingsView) CurrentControlTokenMask() string {
	return valueOrDash(v.Credentials.CurrentControlTokenMasked)
}

func (v serverSettingsView) PreviousAuthMask() string {
	return valueOrDash(v.Credentials.PreviousAuthMasked)
}

func (v serverSettingsView) PreviousControlTokenMask() string {
	return valueOrDash(v.Credentials.PreviousControlTokenMasked)
}

func (v serverSettingsView) HasPreviousGeneration() bool {
	return v.Credentials.PreviousGeneration > 0
}

func (v serverSettingsView) PreviousGenerationLabel() string {
	if v.Credentials.PreviousGeneration > 0 {
		return formatGeneration(v.Credentials.PreviousGeneration)
	}
	return "—"
}

func (v serverSettingsView) GenerationLabel() string {
	return formatGeneration(v.Credentials.Generation)
}

func (v serverSettingsView) Rotatable() bool {
	return v.Credentials.Rotatable && strings.EqualFold(strings.TrimSpace(v.AuthType), "password")
}

func (v serverSettingsView) AllowText() string { return strings.Join(v.Ports.Allow, ", ") }

func (v serverSettingsView) DenyText() string { return strings.Join(v.Ports.Deny, ", ") }

func (v serverSettingsView) ReserveLabel() string {
	if v.Ports.OfflineReserveSeconds <= 0 {
		return "—"
	}
	return (time.Duration(v.Ports.OfflineReserveSeconds) * time.Second).String()
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func formatGeneration(generation int64) string {
	if generation <= 0 {
		return "—"
	}
	return strconv.FormatInt(generation, 10)
}

// credentialClientView decorates one row of the upstream credential-sync table.
type credentialClientView struct {
	upstream.ClientCredential
}

func credentialClientRows(clients []upstream.ClientCredential) []credentialClientView {
	rows := make([]credentialClientView, 0, len(clients))
	for _, client := range clients {
		rows = append(rows, credentialClientView{ClientCredential: client})
	}
	return rows
}

func (c credentialClientView) DisplayName() string {
	if name := strings.TrimSpace(c.Name); name != "" {
		return name
	}
	return c.ClientID
}

func (c credentialClientView) OnlineLabel() string {
	if c.Online {
		return "在线"
	}
	return "离线"
}

func (c credentialClientView) StateClass() string {
	if c.Online {
		return "ok"
	}
	return "muted"
}

func (c credentialClientView) GenerationLabel() string { return formatGeneration(c.Generation) }

func (c credentialClientView) StatusLabel() string { return credentialStatusLabel(c.Status) }

func (c credentialClientView) StatusClass() string { return credentialStatusClass(c.Status) }

func (c credentialClientView) HasError() bool { return strings.TrimSpace(c.Error) != "" }

// settingsRotationView is the per-client outcome returned by a rotation. It is
// rendered in addition to the refreshed sync table so the operator sees the
// result of this specific request.
type settingsRotationView struct {
	Generation int64
	Results    []credentialResultView
}

func newSettingsRotationView(generation int64, results []upstream.CredentialResult) *settingsRotationView {
	rows := make([]credentialResultView, 0, len(results))
	for _, result := range results {
		rows = append(rows, credentialResultView{CredentialResult: result})
	}
	return &settingsRotationView{Generation: generation, Results: rows}
}

type credentialResultView struct {
	upstream.CredentialResult
}

func (r credentialResultView) DisplayName() string {
	if name := strings.TrimSpace(r.Name); name != "" {
		return name
	}
	return r.ClientID
}

func (r credentialResultView) GenerationLabel() string { return formatGeneration(r.Generation) }

func (r credentialResultView) StatusLabel() string { return credentialStatusLabel(r.Status) }

func (r credentialResultView) StatusClass() string { return credentialStatusClass(r.Status) }

func (r credentialResultView) HasError() bool { return strings.TrimSpace(r.Error) != "" }

func credentialStatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "synced":
		return "已同步"
	case "pending":
		return "待同步"
	case "unsupported":
		return "不支持推送"
	case "timeout":
		return "超时"
	case "unknown":
		return "未知"
	case "":
		return "未知"
	default:
		return status
	}
}

func credentialStatusClass(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "synced":
		return "ok"
	case "pending":
		return "warn"
	case "unsupported", "timeout":
		return "bad"
	default:
		return "muted"
	}
}
