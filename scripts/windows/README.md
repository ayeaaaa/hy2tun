# Running hy2tun on Windows

`hy2tun.exe` is a console application. There is no native Windows service in
v1; use Task Scheduler to start it at boot and restart it if it exits.

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

- Windows normally allows the outbound UDP connection to the server without a
  prompt. If outbound traffic is filtered, add an outbound rule for
  `hy2tun.exe` and UDP to the server port.
- The first time the web console starts on `0.0.0.0:7501`, Windows Defender
  Firewall may show a prompt. Allow it on **Private networks only**. Do not
  allow it on public networks; the console uses HTTP unless `web.tls` is
  configured.
- Reverse-mapping traffic arrives at the **server**, not at the Windows
  client, so no inbound port forwarding is needed on the client for mappings.
  Only the optional web console needs an inbound rule.
- If the client is behind a home router, no router port forwarding is needed
  either: the client makes an outbound QUIC connection to the server.

## Logs and troubleshooting

Task Scheduler does not capture console output. Run the client interactively
once, or set `log.file` in `client.yaml` to a path such as
`C:\ProgramData\hy2tun\hy2tun.log`. The client also exposes recent logs in its
web console.

If the task starts but the client exits immediately, check:

- the full path in **Program/script** and the quoting of **Add arguments**;
- that `C:\ProgramData\hy2tun\client.yaml` is readable by the account that
  runs the task (SYSTEM by default);
- that the server address resolves and UDP is not blocked by the network.
