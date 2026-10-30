# LuCI Basic Client Configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the OpenWrt LuCI `hy2tun` page a safe single-server configuration panel with save/validate, start/stop/restart, Web-console launch, status, and logs.

**Architecture:** Add a local `hy2tun config get|set` helper that owns YAML parsing, validation, atomic writes, revision checks, and bcrypt hashing. The LuCI controller invokes that helper through fixed paths and exposes JSON endpoints; the LuCI view renders a basic single-server form and service controls. Multi-server and advanced configurations remain read-only in LuCI and use the client console/SSH.

**Tech Stack:** Go 1.26, `gopkg.in/yaml.v3`, `golang.org/x/crypto/bcrypt`, LuCI Lua (`luci-compat`), Lua/JS templates, OpenWrt `.ipk` gzip-tar packaging.

**Spec:** `docs/superpowers/specs/2026-10-29-luci-basic-config-design.md`

## Global Constraints

- Target release: `v1.5.0`; keep `hy2tund` protocol and data plane unchanged.
- LuCI manages only single-server basic configuration. `servers:` configurations are read-only and link to the client Web console.
- A running service makes the form read-only. Save is allowed only while stopped; apply changes with stop → edit → save/save-start.
- `expected_revision` is the SHA-256 of the config file bytes. A mismatch returns a conflict and must not write.
- Revision check, optional stopped check, and write execute under a `<config>.lock` advisory lock on Linux; Windows may degrade to no lock.
- Preserve known single-server advanced fields: `tls.ca`, `tls.pin_sha256`, `obfs`, `bandwidth`, `transport`, `proxy`, `credentials`, `client.id`, `client.revision`, `web.tls`, `web.trust_proxy`, `log.level`, `log.file`.
- Unknown top-level YAML keys block saving; known advanced fields do not. YAML comments may be lost when the file is re-serialized.
- Secrets (HY2 auth, control token, Web password/hash) are never returned by `config get`, never logged, and never placed in URLs.
- New Web passwords are at least 8 characters and confirmed; new/changed HY2 auth is at least 8 bytes; control token remains at least 16 bytes.
- Server migration requires new auth and control token, clears `client.id`/`client.revision`, and clears an unchanged `tls.sni`.
- Multi-server configs skip the LuCI Web-password start gate and rely on the client's existing generated-password behavior.
- LuCI POST actions use `post = true` and the LuCI CSRF token; commands use fixed paths and `shellquote` for paths.
- `luci-app-hy2tun` depends on `hy2tun (>= 1.5.0)`.
- The `hy2tun` post-install script must not copy an active config or enable the service. Install `client.yaml.example` only.
- OpenWrt `.ipk` files remain gzip-tar containers (`debian-binary`, `data.tar.gz`, `control.tar.gz`), not Debian `ar`.
- Run `go test ./...`, `go test -race ./...`, `go vet ./...`, `make build`, and `scripts/openwrt/test-ipk.sh` before completion.
- Update English and Chinese docs and changelogs together.

## Review Focus

- The running client writes back assigned ports/client identity; LuCI must never overwrite a live config. Tests must cover running-state refusal, stop polling, and revision conflict.
- A malformed, multi-server, unknown-field, or advanced single-server config must not be silently truncated or overwritten. Tests must cover each read/write path.
- Secrets must not leak through `config get`, CLI errors, JSON, logs, or the page. Tests must cover blank-secret preservation and bcrypt verification.
- Server migration must not reuse the old server's client identity, SNI, or credentials. Tests must cover normalized-equivalence, real host change, old invalid server, and advanced-field locking.
- Start can fail after procd accepts it. Tests must cover `--check-config`, Web-password preflight, 5-second running poll, and returning logs without losing a successfully saved config.

---

### Task 1: Normalize server identity

**Files:**
- Modify: `internal/config/servers.go`
- Test: `internal/config/servers_test.go`

**Interfaces:**
- Produces: `func ServerIdentity(value string) (string, error)`
- Consumes: existing `parseClientServer(value string) (string, []ports.Range, bool, error)`

- [ ] **Step 1: Write failing normalization tests**

Add table tests in `internal/config/servers_test.go`:

```go
func TestServerIdentityNormalizesEquivalentForms(t *testing.T) {
	tests := map[string]string{
		" Example.COM:20000-20000 ":          "example.com:20000",
		"host:20002-20003,20000-20001":       "host:20000-20003",
		"host:20000,20001-20003":             "host:20000-20003",
		"[2001:DB8::1]:443":                  "[2001:db8::1]:443",
		"host:20000-20000,20000-20000":       "host:20000",
	}
	for input, want := range tests {
		got, err := ServerIdentity(input)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

func TestServerIdentityRejectsInvalidServer(t *testing.T) {
	_, err := ServerIdentity("missing-port")
	require.ErrorContains(t, err, "host:port")
}
```

Run: `go test ./internal/config -run TestServerIdentity -count=1`
Expected: FAIL because `ServerIdentity` is undefined.

- [ ] **Step 2: Implement normalization**

In `internal/config/servers.go`, add `ServerIdentity`. Use `parseClientServer` to get the trimmed host and parsed ranges, lowercase the host, sort ranges by `Start` then `End`, merge overlapping/adjacent ranges, and format the canonical result with `net.JoinHostPort`. A single-port range formats as `port`; a multi-port range formats as `start-end`. Return the parse error unchanged when the input is invalid.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/config -run TestServerIdentity -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/config/servers.go internal/config/servers_test.go
git commit -m "feat(config): normalize server identity"
```

---

### Task 2: Read basic configuration and validation state

**Files:**
- Create: `internal/config/basic.go`
- Test: `internal/config/basic_test.go`

**Interfaces:**
- Produces: `type BasicConfig struct { ... }`
- Produces: `type BasicErrorCode string` and constants `BasicCodeParse`, `BasicCodeValidation`, `BasicCodeConflict`, `BasicCodeUnknownFields`, `BasicCodeMulti`, `BasicCodeAdvanced`, `BasicCodeServiceRunning`, `BasicCodeInternal`
- Produces: `type BasicError struct { Code BasicErrorCode; Message string; Advanced bool }`
- Produces: `func GetBasic(path string) (*BasicConfig, error)`
- Produces: `func FileRevision(path string) (string, error)`
- Produces internal helpers `loadClientForEdit`, `hasUnknownTopLevelKeys`, `isAdvancedConfig`, `serverLocked`, `validateBasicState`

`BasicConfig` JSON fields are exactly:

```go
type BasicConfig struct {
	Exists               bool      `json:"exists"`
	Multi                bool      `json:"multi"`
	Revision             string    `json:"revision"`
	Valid                bool      `json:"valid"`
	ValidationError      string    `json:"validation_error"`
	AdvancedError        bool      `json:"advanced_error"`
	UnknownFields        bool      `json:"unknown_fields"`
	Server               string    `json:"server"`
	ServerIdentity       string    `json:"server_identity"`
	ServerLocked         bool      `json:"server_locked"`
	CredentialGeneration int64     `json:"credential_generation"`
	AuthSet              bool      `json:"auth_set"`
	ControlTokenSet      bool      `json:"control_token_set"`
	TLSSNI               string    `json:"tls_sni"`
	TLSInsecure          bool      `json:"tls_insecure"`
	ClientName           string    `json:"client_name"`
	Mappings             []Mapping `json:"mappings"`
	WebListen            string    `json:"web_listen"`
	WebUsername          string    `json:"web_username"`
	PasswordSet          bool      `json:"password_set"`
	WebTLS               bool      `json:"web_tls"`
	Advanced             bool      `json:"advanced"`
	LogLevel             string    `json:"log_level"`
	LogFile              string    `json:"log_file"`
}
```

- [ ] **Step 1: Write failing read-model tests**

Add tests covering:

```go
func TestGetBasicMissingFileReturnsDefaults(t *testing.T) {
	got, err := GetBasic(filepath.Join(t.TempDir(), "client.yaml"))
	require.NoError(t, err)
	require.False(t, got.Exists)
	require.False(t, got.Valid)
	require.Empty(t, got.ValidationError)
	require.Equal(t, "openwrt", got.ClientName)
	require.Equal(t, "0.0.0.0:7501", got.WebListen)
	require.Equal(t, "admin", got.WebUsername)
	require.Empty(t, got.Revision)
}

func TestGetBasicReturnsRawValuesAndSecretsAsBooleans(t *testing.T) {
	path := writeTestClientConfig(t, `server: " Host:443 "
auth: "secret-auth"
control:
  token: "0123456789abcdef"
client:
  name: "router"
mappings:
  - id: ""
    type: tcp
    remote_port: 0
    local_addr: "127.0.0.1:22"
web:
  listen: "0.0.0.0:7501"
  username: ""
  password_hash: "$2b$10$...valid..."
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.Equal(t, " Host:443 ", got.Server)
	require.Equal(t, "host:443", got.ServerIdentity)
	require.True(t, got.AuthSet)
	require.True(t, got.ControlTokenSet)
	require.Empty(t, got.Mappings[0].ID)
	require.Equal(t, "", got.WebUsername)
}
```

Also assert:

- YAML parse failure returns `*BasicError` with `Code == BasicCodeParse` and no fields.
- A parsed config with missing auth returns fields, `Valid == false`, `ValidationError` containing `auth is required`.
- A multi-server config sets `Multi == true` and returns `web_listen`, `web_tls`, `password_set`, `log_file` without secrets.
- `advanced` is true for `tls.ca`, `obfs`, `bandwidth`, `transport`, `proxy`, `web.tls`, `web.trust_proxy`, `log.level: debug`, and `log.file`.
- Unknown top-level key sets `UnknownFields == true`; known keys do not.
- Single-server `web.listen` non-empty with no hash sets `Valid == false`; multi-server without hash does not fail this rule.
- `CheckClient` failure (missing CA or invalid bcrypt) sets `Valid == false` and `AdvancedError == true` when the basic copy is valid.

Run: `go test ./internal/config -run TestGetBasic -count=1`
Expected: FAIL because `GetBasic` and `BasicConfig` are undefined.

- [ ] **Step 2: Implement `BasicConfig`, revision, and read helpers**

In `internal/config/basic.go`:

- Implement `FileRevision` as SHA-256 hex of file bytes; return `""` and `nil` when the file does not exist.
- Implement `loadClientForEdit(path)` using `os.ReadFile` + `yaml.Unmarshal` only. Do **not** call `LoadClient` or `Validate`; invalid-but-parseable configs must remain editable.
- Implement `hasUnknownTopLevelKeys(data []byte)` with `yaml.Node`; known top-level keys are `server`, `servers`, `auth`, `tls`, `obfs`, `bandwidth`, `transport`, `control`, `client`, `mappings`, `web`, `proxy`, `log`, `credentials`. Unknown keys set the flag.
- Implement `isAdvancedConfig` using the exact list in the spec.
- Implement `serverLocked` as true when `tls.ca`/`tls.pin_sha256`, `obfs`, `bandwidth`, or `transport` is set, or `credentials.generation > 1`.
- Implement `validateBasicState(cfg)` on a deep-enough copy: clone top-level mappings/servers before calling `Validate`/`CheckClient`; return `(valid, validationError, advancedError)`.
- Implement `GetBasic`: missing file returns the specified defaults; existing file returns raw values, `credential_generation` normalized from 0 to 1, no synthesized mapping IDs, and the validation/advanced/unknown fields above. Multi-server returns only the read-only fields.

- [ ] **Step 3: Run read-model tests**

Run: `go test ./internal/config -run 'TestGetBasic|TestServerIdentity' -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/config/basic.go internal/config/basic_test.go
git commit -m "feat(config): read basic client configuration"
```

---

### Task 3: Write basic configuration with revision and lock protection

**Files:**
- Modify: `internal/config/basic.go`
- Create: `internal/config/lock_unix.go`
- Create: `internal/config/lock_windows.go`
- Test: `internal/config/basic_test.go`

**Interfaces:**
- Produces: `type BasicUpdate struct { ... }`
- Produces: `type SetBasicOptions struct { BeforeWrite func() error }`
- Produces: `func SetBasic(path string, update *BasicUpdate, opts SetBasicOptions) (*BasicConfig, error)`
- Produces internal `withConfigLock(path string, fn func() error) error`
- Produces `BasicError` with stable codes and exit mapping documented below.

`BasicUpdate` fields:

```go
type BasicUpdate struct {
	ExpectedRevision  string     `json:"expected_revision"`
	Server            *string    `json:"server"`
	Auth              *string    `json:"auth"`
	ControlToken      *string    `json:"control_token"`
	TLSSNI            *string    `json:"tls_sni"`
	TLSInsecure       *bool      `json:"tls_insecure"`
	ClientName        *string    `json:"client_name"`
	Mappings          *[]Mapping `json:"mappings"`
	WebListen         *string    `json:"web_listen"`
	WebUsername       *string    `json:"web_username"`
	WebPassword       *string    `json:"web_password"`
	WebPasswordConfirm *string   `json:"web_password_confirm"`
}
```

- [ ] **Step 1: Write failing write-model tests**

Add tests covering:

```go
func TestSetBasicCreatesConfigAndHashesPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	server := "host:443"
	auth := "auth-secret"
	token := "0123456789abcdef"
	name := "router"
	mappings := []Mapping{{Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:22", Enabled: true}}
	listen := "0.0.0.0:7501"
	username := "admin"
	password := "password123"
	confirm := "password123"
	got, err := SetBasic(path, &BasicUpdate{
		Server: &server, Auth: &auth, ControlToken: &token,
		ClientName: &name, Mappings: &mappings,
		WebListen: &listen, WebUsername: &username,
		WebPassword: &password, WebPasswordConfirm: &confirm,
	}, SetBasicOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, got.Revision)
	require.NotEmpty(t, got.Mappings[0].ID)
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(cfg.Web.PasswordHash), []byte(password)))
}
```

Also assert:

- Blank `auth`, `control_token`, `web_password` preserve existing values.
- New config missing auth/token/password is rejected with `BasicCodeValidation`.
- `web_password` shorter than 8 characters or mismatched confirmation is rejected.
- Disabling `web.listen` clears `web.password_hash`; re-enabling without a new password is rejected.
- `expected_revision` mismatch returns `BasicCodeConflict`; existing config requires a non-empty expected revision.
- `BeforeWrite` returning a `*BasicError{Code: BasicCodeServiceRunning}` prevents writing.
- Two concurrent `SetBasic` calls with the same revision: exactly one succeeds and one returns conflict.
- Multi-server and unknown-top-level-field configs return `BasicCodeMulti`/`BasicCodeUnknownFields`.
- Server migration requires new auth/token, clears `client.id` and `client.revision`, and clears an unchanged SNI.
- Changing only equivalent server spelling does not migrate.
- A real server change with `tls.ca`/`obfs`/`bandwidth`/`transport` or generation >1 returns `BasicCodeAdvanced`.
- Missing required JSON fields return validation errors; explicit empty `mappings` and empty `web_listen` are accepted.
- Advanced fields (`obfs`, `proxy`, `web.tls`, `log.file`, etc.) survive a basic save.
- A validation failure leaves the original file and `.bak` untouched; `.bak` exists after a successful overwrite; mode remains 0600.
- Generated mapping IDs survive a second save when the page sends the returned IDs.

Run: `go test ./internal/config -run TestSetBasic -count=1`
Expected: FAIL because `SetBasic` and `BasicUpdate` are undefined.

- [ ] **Step 2: Implement update validation and merge**

In `internal/config/basic.go`:

- Reject nil required pointers: `Server`, `ClientName`, `Mappings`, `WebListen`, `WebUsername`; return `BasicCodeValidation`.
- Reject multi-server configs and unknown top-level keys before any write.
- Compare `ExpectedRevision` with `FileRevision` under the lock; existing file requires non-empty expected revision.
- Load with `loadClientForEdit`; compute old server identity; apply basic fields.
- If server identity changes, require non-empty `Auth` and `ControlToken`; reject if old server has locked advanced fields or generation > 1; clear `Client.ID`, set `Client.Revision = 0`; clear `TLSSNI` when the submitted SNI equals the old SNI.
- Auth changes require at least 8 bytes and generation <= 1; token changes preserve existing token when blank; token validation remains at least 16 bytes.
- Web changes: empty listen clears hash; non-empty listen requires username; new password requires 8+ characters and matching confirmation; bcrypt hash with `bcrypt.DefaultCost`; blank password preserves an existing hash.
- Call `BeforeWrite` inside the lock immediately before `SaveClient`.
- Run basic-copy validation first; return `BasicCodeValidation` on failure. Run full `Validate`/`CheckClient`; return `BasicCodeAdvanced` with `Advanced: true` on failure.
- Use `SaveClient`; treat `ErrCommitted` as success. Return `GetBasic(path)`.

- [ ] **Step 3: Add cross-platform lock helpers**

`lock_unix.go` (`//go:build !windows`): open `<path>.lock` with `O_CREATE|O_RDWR`, `0600`, call `syscall.Flock(fd, LOCK_EX)`, defer unlock/close. `lock_windows.go` (`//go:build windows`): no-op wrapper. `withConfigLock` returns callback errors unchanged.

- [ ] **Step 4: Run write-model tests**

Run: `go test ./internal/config -run 'TestSetBasic|TestGetBasic' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/basic.go internal/config/basic_test.go internal/config/lock_unix.go internal/config/lock_windows.go
git commit -m "feat(config): write basic client configuration safely"
```

---

### Task 4: Add the `hy2tun config` CLI

**Files:**
- Modify: `cmd/hy2tun/main.go`
- Create: `cmd/hy2tun/config.go`
- Create: `cmd/hy2tun/config_test.go`

**Interfaces:**
- Produces: `func runConfig(args []string, stdin io.Reader, stdout, stderr io.Writer) int`
- Produces package var `serviceRunning = func() bool { ... }` for tests and `--require-stopped`.
- Commands:
  - `hy2tun config get -c <path>`
  - `hy2tun config set -c <path> [--require-stopped]`
- Exit codes: `0` success; `1` internal/parse; `2` validation; `3` conflict; `4` unknown/multi; `5` advanced; `6` service running.

- [ ] **Step 1: Write failing CLI tests**

Add `cmd/hy2tun/config_test.go`:

```go
func TestRunConfigGetMissingReturnsDefaults(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runConfig([]string{"get", "-c", filepath.Join(t.TempDir(), "client.yaml")}, strings.NewReader(""), &out, &errOut)
	require.Equal(t, 0, code)
	require.Contains(t, out.String(), `"exists":false`)
}

func TestRunConfigSetConflictExitCode(t *testing.T) {
	path := writeCLIConfig(t)
	payload := `{"expected_revision":"wrong","server":"host:443","client_name":"r","mappings":[],"web_listen":"","web_username":""}`
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 3, code)
	require.Contains(t, errOut.String(), `"code":"conflict"`)
}

func TestRunConfigSetRequireStopped(t *testing.T) {
	old := serviceRunning
	serviceRunning = func() bool { return true }
	t.Cleanup(func() { serviceRunning = old })
	path := writeCLIConfig(t)
	payload := `{"expected_revision":"...","server":"host:443","client_name":"r","mappings":[],"web_listen":"","web_username":""}`
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path, "--require-stopped"}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 6, code)
	require.Contains(t, errOut.String(), `"code":"service_running"`)
}
```

Also test parse errors (`get` exit 1), validation errors (exit 2), unknown fields/multi (exit 4), advanced (exit 5), and success (`{"ok":true,"revision":"..."}`).

Run: `go test ./cmd/hy2tun -run TestRunConfig -count=1`
Expected: FAIL because `runConfig` is undefined.

- [ ] **Step 2: Implement `runConfig`**

In `cmd/hy2tun/config.go`:

- Use `flag.NewFlagSet("config", flag.ContinueOnError)`, `-c` default `client.yaml`, `--require-stopped`.
- Dispatch `get`/`set`; print usage to stderr and return 1 for unknown commands.
- `get`: call `config.GetBasic`; JSON-encode `BasicConfig`; on error print JSON error and return 1.
- `set`: JSON-decode stdin with `DisallowUnknownFields`; call `config.SetBasic` with `BeforeWrite` that returns `&config.BasicError{Code: config.BasicCodeServiceRunning, ...}` when `--require-stopped` and `serviceRunning()` is true; print `{"ok":true,"revision":"..."}` or JSON error.
- Map `*config.BasicError.Code` to the exit codes above; use 1 for unknown/internal.
- `serviceRunning` executes `/etc/init.d/hy2tun status` and returns exit code 0.

- [ ] **Step 3: Wire `main`**

In `cmd/hy2tun/main.go`, before the existing `flag.Parse` block:

```go
if len(os.Args) > 1 && os.Args[1] == "config" {
	os.Exit(runConfig(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
}
```

Do not change existing `-version`, `-check-config`, or `-c` behavior.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/hy2tun ./internal/config -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/hy2tun/main.go cmd/hy2tun/config.go cmd/hy2tun/config_test.go
git commit -m "feat(cli): add config get and set commands"
```

---

### Task 5: Add LuCI JSON endpoints and service actions

**Files:**
- Modify: `scripts/openwrt/luci-app-hy2tun/luasrc/controller/hy2tun.lua`
- Test: `scripts/openwrt/test-ipk.sh` (static checks added in Task 7)

**Interfaces:**
- GET `admin/services/hy2tun/data` → JSON:
  `{ok, running, enabled, config, config_error, logs}`
- POST `admin/services/hy2tun/action` with form fields `op`, `payload`, `token` → JSON:
  `{ok, config, running, enabled, logs, advanced, error, config_error, saved}`
- Allowed ops: `save`, `save_start`, `start`, `stop`, `restart`, `enable`, `disable`, `logs`.
- `action` entry has `post = true` so LuCI enforces CSRF.

- [ ] **Step 1: Add controller routes**

In `index()`, keep the existing page entry and add:

```lua
local data = entry({"admin", "services", "hy2tun", "data"}, call("data"), nil)
data.leaf = true

local action = entry({"admin", "services", "hy2tun", "action"}, call("action"), nil)
action.leaf = true
action.post = true
```

- [ ] **Step 2: Implement helper functions**

Add fixed-path constants and helpers in the controller:

- `CONFIG_PATH = "/etc/hy2tun/client.yaml"`, `BINARY = "/usr/bin/hy2tun"`, `INIT = "/etc/init.d/hy2tun"`.
- `service_running()` and `service_enabled()` use `luci.sys.call(INIT .. " status >/dev/null 2>&1")` / `enabled` and compare to 0.
- `config_get()` executes `BINARY .. " config get -c " .. shellquote(CONFIG_PATH)`, captures stdout/stderr with temp files, parses `luci.jsonc`.
- `config_set(payload)` writes payload to a 0600 temp file, executes
  `BINARY .. " config set -c ... --require-stopped < temp 2> err`, reads stderr, parses JSON error, removes temp files.
- `run_service(op)` runs `INIT .. " " .. op .. " >/dev/null 2>&1"`.
- `wait_running(want, timeout=5)` polls `service_running()` with `luci.sys.call("sleep 1")`; return bool.
- `logs()` uses `config_get().log_file` when non-empty with
  `tail -n 50 -- <shellquote(path)>`; otherwise `logread | grep hy2tun | tail -50`.
- `write_json(obj)` sets `application/json` and writes `luci.jsonc.stringify(obj)`.

- [ ] **Step 3: Implement `data` and `action`**

`data` returns status/logs even when `config_get` fails: `config = nil`, `config_error` set. `action`:

- Parse `op` and `payload`; reject unknown ops.
- `save`: if `service_running()` return error code `service_running`; call `config_set`; on success return `config_get` plus status.
- `save_start`: require stopped; call `config_set`; then run `start` preflight and start; if start fails return `ok:false, saved:true, config, revision, error, logs`.
- `start`/`restart`: run `config_get`; if single-server and `web_listen` non-empty and `password_set` false, return error; run `hy2tun --check-config -c ...`; run service; poll running 5 seconds; return status/logs. `restart` is rejected unless `service_running()` is already true; stopped state must use `start`.
- `stop`: run stop; poll inactive 5 seconds; return status/logs.
- `enable`/`disable`: run init action; return status.
- `logs`: return logs.
- Map `config set` JSON codes: conflict → HTTP 409; service_running/validation/advanced/unknown_fields/multi → HTTP 400; internal → HTTP 500. Do not put secrets in errors.

- [ ] **Step 4: Verify syntax and static behavior**

On a machine with Lua 5.1: `lua -e 'assert(loadfile("scripts/openwrt/luci-app-hy2tun/luasrc/controller/hy2tun.lua"))'`.
On OpenWrt: install the package, then `lua -e 'local ok,err=pcall(require,"luci.controller.hy2tun"); print(ok,err)'`.
Expected: load succeeds; page route returns JSON without a 502.

- [ ] **Step 5: Commit**

```bash
git add scripts/openwrt/luci-app-hy2tun/luasrc/controller/hy2tun.lua
git commit -m "feat(luci): add config and service endpoints"
```

---

### Task 6: Build the LuCI basic configuration page

**Files:**
- Modify: `scripts/openwrt/luci-app-hy2tun/luasrc/view/hy2tun/status.htm`

**Interfaces:**
- Consumes: `data`/`action` endpoints from Task 5.
- Form payload fields match `BasicUpdate` exactly.
- Uses CSRF `<%=token%>` and `application/x-www-form-urlencoded`.

- [ ] **Step 1: Replace the status-only template with the page shell**

Keep `<%+header%>` / `<%+footer%>` and add:

- Status card with IDs `hy2tun-running`, `hy2tun-enabled`, `hy2tun-config-state`.
- Buttons `hy2tun-start`, `hy2tun-stop`, `hy2tun-restart`, `hy2tun-enable`, `hy2tun-disable`, `hy2tun-refresh`, `hy2tun-open-console`.
- Error/notice elements `hy2tun-error`, `hy2tun-notice`.
- Form fields with IDs `hy2tun-server`, `hy2tun-auth`, `hy2tun-token`, `hy2tun-sni`, `hy2tun-insecure`, `hy2tun-client-name`, `hy2tun-web-listen`, `hy2tun-web-username`, `hy2tun-web-password`, `hy2tun-web-password-confirm`.
- Mapping table `hy2tun-mappings` with an add button and row template.
- Logs `<pre id="hy2tun-logs">`.

- [ ] **Step 2: Implement page state and rendering**

Inline JS:

- `token`, `dataUrl`, `actionUrl` from LuCI `build_url`.
- `load()` GET `data`; render status, form values, mapping rows, logs, and advanced/multi notices.
- Running state disables all form inputs and save buttons; stop button calls `stop` and reloads data.
- Multi state hides the form; shows `web_listen`/`web_tls` console link.
- Unknown fields or YAML parse error hides/disables save and shows the error.
- `server` input change: show migration notice and clear SNI.
- `credential_generation > 1` disables auth/token; `server_locked` disables server.
- `password_set` placeholder “已设置，留空保持不变”.
- Mapping rows use an incrementing DOM key, not a synthetic mapping ID; send `id: ""` for new rows.

- [ ] **Step 3: Implement save/service actions**

- `collectPayload()` reads all form fields; for blank auth/token/password send `""`; include `expected_revision`, `mappings`, and password confirmation.
- Client-side checks: required server/client name, new password 8+ and matching, web username required when listen non-empty.
- `post(op, payload)` sends form-encoded `op`, `payload`, `token`.
- Save only when stopped; buttons `save` and `save_start`.
- On success, replace form data/mappings from returned `config` (including generated IDs) and update revision.
- On `saved:true` + start error, keep the new config/revision and show the error/logs.
- On conflict (409) show “配置已被修改，请刷新”.
- Open console builds `http(s)://<browser-host or loopback>:<port>/` from `web_listen` and `web_tls`; disable unless running and listen non-empty.
- Refresh logs button calls `logs`.

- [ ] **Step 4: Verify page behavior manually**

On OpenWrt with the package installed:

- Open the page; no 502.
- Fill a config; save; start; open console.
- Confirm running form is read-only; stop; edit; save-start.
- Confirm error banner and log rendering.
- Confirm multi config hides the form and shows the console link.

- [ ] **Step 5: Commit**

```bash
git add scripts/openwrt/luci-app-hy2tun/luasrc/view/hy2tun/status.htm
git commit -m "feat(luci): add basic client configuration page"
```

---

### Task 7: Update OpenWrt packaging and examples

**Files:**
- Create: `examples/client-basic.yaml`
- Modify: `scripts/openwrt/build-ipk.sh`
- Modify: `scripts/openwrt/build-luci-ipk.sh`
- Modify: `scripts/openwrt/test-ipk.sh`
- Modify: `scripts/openwrt/README.md`

**Interfaces:**
- Package installs `/etc/hy2tun/client.yaml.example` from `examples/client-basic.yaml`.
- Post-install does not create `/etc/hy2tun/client.yaml` and does not enable the service.
- LuCI control dependency: `Depends: luci-base, luci-compat, hy2tun (>= 1.5.0)`.

- [ ] **Step 1: Add the basic example**

Create `examples/client-basic.yaml` with a runnable single-server shape:

```yaml
server: "us.example.com:24443"
auth: "change-me"
tls:
  sni: "us.example.com"
  insecure: false
control:
  token: "0123456789abcdef"
client:
  name: "openwrt"
mappings:
  - id: ""
    type: tcp
    remote_port: 0
    local_addr: "127.0.0.1:22"
    enabled: true
web:
  listen: "0.0.0.0:7501"
  username: "admin"
  password_hash: ""
log:
  level: "info"
```

Do not include CA/obfs/bandwidth/proxy/transport fields.

- [ ] **Step 2: Update package builders**

In `build-ipk.sh`, install `examples/client-basic.yaml` as `client.yaml.example`; replace postinst with a script that only `chmod 600 /etc/hy2tun/client.yaml` when it already exists and never copies/enables. In `build-luci-ipk.sh`, change the dependency line to the versioned form.

- [ ] **Step 3: Update package tests**

In `test-ipk.sh`:

- Assert `client.yaml.example` is present.
- Assert the client postinst does not contain `cp` or `enable`.
- Assert LuCI control contains `hy2tun (>= 1.5.0)`.
- Extract the controller/view and assert strings:
  - `post = true`
  - `config get`
  - `hy2tun-server`
  - `hy2tun-mappings`
  - `hy2tun-save` or the chosen save button ID.

Run: `scripts/openwrt/test-ipk.sh`
Expected: PASS.

- [ ] **Step 4: Update OpenWrt README**

Replace the “edit YAML then start” flow with:

1. Install both packages.
2. Open LuCI → Services → hy2tun.
3. Fill the basic form, save, start.
4. Open the Web console.
5. Explain multi-server/advanced uses the client console/SSH.

- [ ] **Step 5: Commit**

```bash
git add examples/client-basic.yaml scripts/openwrt/build-ipk.sh scripts/openwrt/build-luci-ipk.sh scripts/openwrt/test-ipk.sh scripts/openwrt/README.md
git commit -m "feat(openwrt): ship basic LuCI onboarding"
```

---

### Task 8: Documentation, release, and real-device verification

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `CHANGELOG.md`
- Modify: `CHANGELOG.zh-CN.md`

**Interfaces:**
- Release `v1.5.0` with bilingual notes and all existing assets.
- OpenWrt package names and architecture suffixes unchanged.

- [ ] **Step 1: Update bilingual docs**

In both READMEs, add an OpenWrt section covering:

- New first-run flow: install → LuCI form → save → start → console.
- Single-server fields managed in LuCI.
- Multi-server/advanced fields via client console/SSH.
- Running-state read-only and revision conflict behavior.
- Web password minimum and secret handling.
- `client.yaml.example` is basic-only; no auto-copy/enable.

- [ ] **Step 2: Add bilingual changelog entries**

`CHANGELOG.md` and `CHANGELOG.zh-CN.md` for `v1.5.0`:

- LuCI basic configuration form, start/stop/restart, console link, logs.
- `hy2tun config get/set` helper with revision/lock/validation.
- Safe migration and advanced/multi-server protection.
- OpenWrt install flow no longer auto-creates config or enables service.
- Package dependency `hy2tun >= 1.5.0`.

- [ ] **Step 3: Run full verification**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
make build
scripts/openwrt/test-ipk.sh
```

Expected: all pass.

- [ ] **Step 4: Build and install on OpenWrt**

On ImmortalWrt 24.10.2 x86_64:

1. Install `hy2tun_1.5.0_amd64.ipk` and `luci-app-hy2tun_1.5.0_all.ipk`.
2. Confirm `/etc/hy2tun/client.yaml` does not exist and service is disabled.
3. Open the page; fill server/auth/token/mapping/web password; save; start.
4. Confirm page says running; open console and log in.
5. Stop; edit mapping; save-start; verify mapping via the server.
6. Test conflict: open two tabs, save from both, second gets conflict.
7. Test multi-server: use a `servers:` config; page hides form and links to console.
8. Test advanced fields: save basic changes and confirm `obfs`/`proxy`/`web.tls` remain.
9. Test invalid YAML: page shows error; stop/disable still available if running.
10. Test log file: set `log.file`; page reads its tail.

- [ ] **Step 5: Commit docs and release**

```bash
git add README.md README.zh-CN.md CHANGELOG.md CHANGELOG.zh-CN.md
git commit -m "docs: document LuCI basic configuration"
git push origin main
git tag -a v1.5.0 -m "hy2tun v1.5.0"
git push origin v1.5.0
```

Wait for CI/Release; verify assets include:

- `hy2tun_1.5.0_{amd64,arm64,arm,mips,mipsle}.ipk`
- `luci-app-hy2tun_1.5.0_all.ipk`
- Linux/Windows archives and `SHA256SUMS`.

- [ ] **Step 6: Upgrade production and verify**

Upgrade server/client to `v1.5.0`, verify 3 mappings and TCP/UDP echo, record backups, and leave `main` clean. No additional commit is expected.
