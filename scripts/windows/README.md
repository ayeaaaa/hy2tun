# Running hy2tun on Windows

`hy2tun.exe` is a console application. There is no native Windows service in
v1; use Task Scheduler to start it at boot and restart it if it exits.

This guide applies to hy2tun 1.6.0 and later. The OpenWrt LuCI package requires
`hy2tun (>= 1.6.0)`; LuCI itself is OpenWrt-only and does not apply to Windows.

## Install

1. Download the Windows archive from the release page and extract it, for
   example to `C:\Program Files\hy2tun\`. The archive contains `hy2tun.exe`,
   `hy2tund.exe`, `examples\client.yaml`, `examples\server.yaml`,
   `scripts\`, `README.md`, and `SHA256SUMS`; only `hy2tun.exe` is needed on a
   Windows client. To verify the download, compare
   `Get-FileHash .\hy2tun.exe -Algorithm SHA256` with the archive's
   `SHA256SUMS` file.
2. Create `C:\ProgramData\hy2tun\` and copy `examples\client.yaml` there as
   `client.yaml`. Edit the server address, password, control token, and
   mappings. The example file uses Linux paths such as
   `/etc/hy2tun/cert.pem`; replace them with Windows paths such as
   `C:\ProgramData\hy2tun\cert.pem`.
3. Restrict the configuration file, because it contains secrets:

   ```powershell
   icacls "C:\ProgramData\hy2tun\client.yaml" /inheritance:r /grant:r "SYSTEM:(F)" "Administrators:(F)"
   ```

4. Test it once from an elevated PowerShell window:

   ```powershell
   & "C:\Program Files\hy2tun\hy2tun.exe" -c "C:\ProgramData\hy2tun\client.yaml"
   ```

   The client prints the generated web password once if
   `web.password_hash` is empty. Stop it with `Ctrl+C` after confirming the
   log line that the control handshake completed.

## Transport modes

Set `mode` in `client.yaml`; omit it to keep the default HY2 behavior:

- `hy2` — HY2/QUIC over UDP only.
- `tcp` — TLS 1.3 over TCP only. `server` must be one numeric port; ranges and
  `transport.udp` hopping are rejected. UDP mappings are framed over TCP, which
  adds reliability, latency, and head-of-line cost.
- `hy2+tcp` — prefer HY2, fall back to TCP on recognized network failures, and
  probe HY2 for recovery.

In `hy2+tcp`, TCP uses the first port item in `server`; a range or port set
still uses every port for HY2 UDP hopping. The server must also be configured
for TCP on the same numeric port, and the firewall must allow both outbound UDP
and outbound TCP to that port. A switch that changes the active transport drops
existing TCP/proxy connections and may lose UDP packets; there is no seamless
or exactly-once migration. The client Web console shows the configured mode,
actual transport, switch reason/count, and last probe; use it instead of
inferring the active transport from the YAML file.

Stop the task before editing `client.yaml`, then start it again:

```powershell
Stop-ScheduledTask  -TaskName "hy2tun"
notepad "C:\ProgramData\hy2tun\client.yaml"
Start-ScheduledTask -TaskName "hy2tun"
```

A multi-server configuration can set an independent `mode` for each
`servers[]` entry. The LuCI read-only multi-server policy does not apply to
Windows; use the client Web console for per-server diagnostics and mode
changes.

## Start at boot with Task Scheduler

### GUI

1. Open **Task Scheduler** and choose **Create Task** (not "Create Basic
   Task").
2. **General**:
   - Name: `hy2tun`
   - Select **Run whether user is logged on or not**.
   - Select **Run with highest privileges** if the client must bind a local
     privileged port or write to a protected directory.
   - Configure for: Windows 10 / Windows Server 2016 or newer.
3. **Triggers** → **New** → **At startup** → OK.
4. **Actions** → **New**:
   - Action: **Start a program**
   - Program/script: `C:\Program Files\hy2tun\hy2tun.exe`
   - Add arguments: `-c "C:\ProgramData\hy2tun\client.yaml"`
   - Start in: `C:\Program Files\hy2tun`
5. **Settings**:
   - **Allow task to be run on demand**.
   - **Restart the task if it fails**: every 1 minute, up to 999 times.
   - **If the task is already running, do not start a new instance**.
   - Clear **Stop the task if it runs longer than**.
6. **Conditions**: clear **Start the task only if the computer is on AC
   power** if this is a laptop or a device that may run on battery.
7. Save the task. Enter the account password if Task Scheduler asks for it.

### PowerShell equivalent

Run the following in an elevated PowerShell window:

```powershell
$action = New-ScheduledTaskAction `
  -Execute "C:\Program Files\hy2tun\hy2tun.exe" `
  -Argument '-c "C:\ProgramData\hy2tun\client.yaml"' `
  -WorkingDirectory "C:\Program Files\hy2tun"

$trigger = New-ScheduledTaskTrigger -AtStartup

$principal = New-ScheduledTaskPrincipal `
  -UserId "SYSTEM" -LogonType ServiceAccount -RunLevel Highest

$settings = New-ScheduledTaskSettingsSet `
  -RestartCount 999 `
  -RestartInterval (New-TimeSpan -Minutes 1) `
  -ExecutionTimeLimit ([TimeSpan]::Zero) `
  -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName "hy2tun" `
  -Action $action -Trigger $trigger -Principal $principal -Settings $settings
```

Start or stop it with:

```powershell
Start-ScheduledTask -TaskName "hy2tun"
Stop-ScheduledTask  -TaskName "hy2tun"
Get-ScheduledTaskInfo -TaskName "hy2tun"
```

## Firewall notes

- Outbound transport depends on mode: `hy2` needs UDP only, `tcp` needs TCP
  only, and `hy2+tcp` needs both on the same numeric server port. Windows
  normally allows outbound UDP without a prompt; if traffic is filtered, add
  an outbound rule for `hy2tun.exe` and the required protocol(s). The UDP and
  TCP transports are separate connections, so one rule does not open the
  other.
- The first time the web console starts on `0.0.0.0:7501`, Windows Defender
  Firewall may show a prompt. Allow it on **Private networks only**. Do not
  allow it on public networks; the console uses HTTP unless `web.tls` is
  configured.
- Reverse-mapping traffic arrives at the **server**, not at the Windows
  client, so no inbound port forwarding is needed on the client for mappings.
  Only the optional web console needs an inbound rule.
- If the client is behind a home router, no router port forwarding is needed
  either: the client makes an outbound connection to the server (QUIC/UDP for
  `hy2`, TLS/TCP for `tcp` or fallback).

## Backup and rollback

Before changing the mode or replacing the binary, stop the task and create the
backup files used by the rollback commands:

```powershell
Stop-ScheduledTask -TaskName "hy2tun"
Copy-Item "C:\ProgramData\hy2tun\client.yaml" `
  "C:\ProgramData\hy2tun\client.yaml.bak" -Force
Copy-Item "C:\Program Files\hy2tun\hy2tun.exe" `
  "C:\Program Files\hy2tun\hy2tun.exe.bak" -Force
Start-ScheduledTask -TaskName "hy2tun"
```

Keep the previous release archive as well. To roll back:

```powershell
Stop-ScheduledTask -TaskName "hy2tun"
Copy-Item "C:\ProgramData\hy2tun\client.yaml.bak" `
  "C:\ProgramData\hy2tun\client.yaml" -Force
Copy-Item "C:\Program Files\hy2tun\hy2tun.exe.bak" `
  "C:\Program Files\hy2tun\hy2tun.exe" -Force
Start-ScheduledTask -TaskName "hy2tun"
```

Restore the previous `mode` as part of the configuration rollback. If the
server mode was changed, restore it separately; the Windows client cannot
change the server's listeners. Keep the backup until the client reconnects and
the mappings are verified.

## Logs and troubleshooting

Task Scheduler does not capture console output. Run the client interactively
once, or set `log.file` in `client.yaml` to a path such as
`C:\ProgramData\hy2tun\hy2tun.log`. The client also exposes recent logs in its
web console.

If the task starts but the client exits immediately, check:

- the full path in **Program/script** and the quoting of **Add arguments**;
- that `C:\ProgramData\hy2tun\client.yaml` is readable by the account that
  runs the task (SYSTEM by default);
- that the server address resolves and UDP is not blocked by the network;
- for `tcp` or `hy2+tcp`, that TCP to the same numeric server port is not
  blocked and the server has a TCP listener;
- that a pure `tcp` configuration uses one numeric port without ranges or
  `transport.udp` hop intervals.
