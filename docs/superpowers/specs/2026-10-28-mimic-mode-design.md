# hy2tun Mimic 模式设计

- 状态：已批准
- 日期：2026-10-28
- 适用版本：v1.3
- 目标读者：实现这套功能的工程师

## 1. 背景

用户所在网络经常出现 UDP 被限速或阻断。现有日志中多次出现：

```text
control connection lost: timeout: no recent network activity
connect HY2: connect error: timeout: no recent network activity
```

端口跳跃只能绕过“单个 UDP 端口被限速”；如果 UDP/QUIC 被整体干扰，
端口跳跃帮助有限。Mimic 把 UDP 包伪装成 TCP，是这种情况下更合适的方案。

但 Mimic 是启动层能力：

- 必须两端同时开启；
- 仅 Linux；
- 需要 root/eBPF 和外部 `mimic` 程序；
- 开启后没开 Mimic 的客户端会全部失效；
- 和端口跳跃互斥。

因此本设计做一个**普通模式 / Mimic 模式二选一**的开关，而不是让两种模式
在同一端口共存。

## 2. 目标与非目标

### 2.1 目标

1. 服务端和客户端都增加 `mimic` 配置块，`enabled` 默认 `false`。
2. `enabled: false` 时完全保持现有行为，包括端口跳跃。
3. `enabled: true` 时：
   - 在 HY2 服务端/客户端启动前启动外部 `mimic` 子进程；
   - 自动生成 Mimic filter；
   - 等待 Mimic 附加完成；
   - 自动关闭 QUIC GSO；
   - 进程退出时停止 Mimic；
   - Mimic 意外退出时**终止 hy2tun 进程**并记录错误，不自动回退到普通模式，
     也不让 `Client.Start` 在无 Mimic 的情况下继续重连。
4. 服务端和客户端设置页增加可编辑的 Mimic 设置：
   - `enabled`
   - `interface`
   - `xdp_mode`
   - `path`
   - `extra_args`
   保存时只写配置文件，不热切换、不自动重启；页面显示当前运行模式和
   “重启后生效”。
5. 启动时校验：
   - Mimic 不能和端口跳跃同时配置；
   - Mimic 模式要求 Linux；
   - Mimic 模式要求 `mimic` 可执行文件存在；
   - 服务端 `listen` 必须是单端口（当前服务端本来就不支持范围）。
6. Web 设置页提供“重启应用”按钮：
   - 保存配置不会自动重启；
   - 用户输入当前管理员密码并确认后，只重启当前 hy2tun/hy2tund 进程；
   - 依赖 systemd 的 `Restart=always`；
   - 不在 Web 里执行任意 `systemctl` 或 shell 命令。
7. 文档说明切换步骤、两端必须一致、标准客户端会断开、MTU/GSO 影响、
   外部 Mimic 安装和 GPL 许可。

### 2.2 非目标

- 不做普通模式和 Mimic 模式在同一服务端同时共存。
- 不做独立 Mimic 端口/第二个 `hy2tund` 实例。
- 不做 Web 页面热切换；Web 只写配置，重启必须由用户显式点击“重启应用”。
- 不做“一键双端切换”：服务端 Web 只重启服务端，客户端 Web 只重启客户端；
  两端必须各自切换并重启。
- 不在 Web 中执行 `systemctl`、`mimic` 或任意 shell 命令。
- 不把 Mimic 二进制打包进 hy2tun release（Mimic 是 GPL-2.0-only）。
- 不支持 Windows/OpenWrt 上的 Mimic 模式。
- 不自动安装 `mimic`、内核模块或内核头文件。
- 不自动调整 MTU；只在文档中说明 12 字节开销和测试方法。
- 不改变现有 HY2 线协议或反向映射协议。

### 2.3 反目标

- 不能在 Mimic 模式下静默回退到普通模式；启动失败必须明确报错。
- 不能在 Mimic 开启时继续允许端口跳跃配置。
- 不能把 Mimic 子进程的 stdout/stderr 中的敏感信息写入日志；Mimic 输出
  只按行转发，并做长度限制。
- 不能因为普通模式配置没有 `mimic` 段而改变现有 YAML 形状。
- 不能在没有 warning 的情况下让用户以为服务端仍兼容普通客户端。

## 3. 配置

服务端和客户端使用同一个 `mimic` 配置块：

```yaml
mimic:
  enabled: false
  interface: ""
  xdp_mode: ""
  path: ""
  extra_args: []
```

字段：

| 字段 | 含义 |
| --- | --- |
| `enabled` | `false` 普通模式；`true` Mimic 模式。默认 `false`。 |
| `interface` | Mimic 附加的网卡名。空表示自动探测到对端的网卡。 |
| `xdp_mode` | 空、`native` 或 `skb`。空让 Mimic 自己选择。 |
| `path` | `mimic` 可执行文件路径。空表示从 `PATH` 查找。 |
| `extra_args` | 原样传给 `mimic run` 的额外参数，用于 padding/handshake/keepalive 调优。 |

配置校验：

- `xdp_mode` 只允许空、`native`、`skb`。
- `interface` 和 `path` 的检查**只在 `enabled: true` 时执行**；`enabled: false`
  时即使保留旧的 `interface`/`path` 也不影响普通模式启动。
- `interface` 非空时必须是当前系统存在的网卡（Mimic 模式启动时检查）。
- `path` 非空时必须是可执行文件（Mimic 模式启动时检查）。
- `extra_args` 不允许包含换行或 NUL。
- 客户端 `mimic.enabled: true` 时：
  - `server` 必须是单端口；
  - 不允许 `transport.udp` 端口跳跃配置；
  - 不允许端口集合（`host:443,8443`、`host:30000-40000`）。
- 服务端 `mimic.enabled: true` 时：
  - `listen` 必须是单端口；
  - 当前版本本来就不支持 `listen` 范围。
- `mimic.enabled: true` 在非 Linux 上通过编译，但启动时返回明确错误：
  `mimic is only supported on Linux`。

## 4. 运行时行为

### 4.1 服务端

启动顺序：

1. 读取并校验配置。
2. 绑定 HY2 UDP 端口。
3. 启动 Mimic：
   - 角色：server；
   - filter：
     - 通配监听：`local=0.0.0.0:<port>,handshake=0:3` 和
       `local=[::]:<port>,handshake=0:3`；
     - 指定 IP：`local=<ip>:<port>,handshake=0:3`；
   - 等待 Mimic 附加完成。
4. 创建 HY2 server，`QUICConfig.DisableGSO = true`。
5. 开始 Serve。
6. 关闭时先关闭 HY2 server/packet conn，再停止 Mimic。

如果 Mimic 启动失败，服务端不启动 HY2，返回错误。

### 4.2 客户端

启动顺序：

1. 读取并校验配置。
2. 解析 `server` 的单个地址；如果 DNS 返回多个 IP，全部作为 filter：
   `remote=<ip>:<port>`。
3. 启动 Mimic：
   - 角色：client；
   - 等待附加完成。
4. 创建 HY2 client，`QUICConfig.DisableGSO = true`。
5. 开始控制连接。
6. 关闭时先关闭 HY2 client/控制流，再停止 Mimic。

如果 Mimic 启动失败，客户端不建立 HY2 连接，返回错误。

### 4.3 Mimic 子进程管理

实现一个 `internal/mimic` 包，Linux 和非 Linux 两个实现：

- 解析 `mimic` 路径：显式 `path` 或 `exec.LookPath("mimic")`。
- 解析网卡：
  - 显式 `interface` 优先；
  - 客户端：空时通过对端地址做一次 UDP dial，取得本地源 IP，再匹配网卡；
  - 服务端通配监听：探测默认路由网卡；
  - 服务端指定 IP 监听：必须选择拥有该 IP 的网卡；找不到则报错，
    不能退回默认路由，否则 Mimic 可能附加到看不到监听 IP 的网卡。
- 启动参数：
  ```text
  mimic run <interface> -f <filter> [-f <filter> ...] [--xdp-mode <mode>] <extra_args...>
  ```
- 使用 `exec.CommandContext`，`Pdeathsig = SIGTERM`，退出时先 SIGINT，等待
  detach，超时后杀掉。
- 等待 `/run/mimic/*_<ifindex>.lock` 中记录的子进程 PID；最多 10 秒。
- 如果同一网卡已有存活 Mimic：
  - 本版本不接管/复用已有实例；
  - 返回明确错误，提示先停止已有 Mimic。
- 子进程意外退出：
  - 记录错误；
  - 关闭 HY2 服务端/客户端；
  - 让 `Start` 返回致命错误，主进程以非零状态退出；
  - 不允许 `Client.Start` 在 Mimic 已退出后继续重连普通 HY2。
  systemd 的 `Restart=always` 会重启进程；如果 Mimic 仍不可用，下一次启动
  会直接失败，而不是静默降级。
- Mimic 的 stderr 按行转发到 slog，单行长度上限 4 KiB，避免日志膨胀。

### 4.4 GSO

Mimic 在 socket 之后改写包，会破坏 GSO 批次。Mimic 模式必须关闭 GSO：

- 服务端：`hyserver.QUICConfig.DisableGSO = true`。
- 客户端：`hyclient.QUICConfig.DisableGSO = true`。

普通模式保持默认值，不改变性能。

### 4.5 MTU

Mimic 给每个 UDP 包增加 12 字节。文档要求：

- 使用 Mimic 时把路径 MTU 预留 12 字节；
- 例如 WireGuard over IPv4 的 1420 要降到约 1408；
- Hysteria QUIC 的 PMTUD 可能自动适应，但仍需用 iperf3/实际映射验证；
- 如果出现大包丢失，先降低 MTU 或减小 UDP 载荷。

本版本不自动修改 MTU。

### 4.6 防火墙与 NAT

Mimic 的包在线上看起来是 TCP，但在本机 netfilter 眼里仍可能是 UDP。文档
要求：

- 云安全组/上游防火墙对 HY2 端口同时放行 TCP 和 UDP；
- 主机防火墙也按“TCP 和 UDP 都要放行”处理；
- 如果只放行 UDP 或只放行 TCP，Mimic 模式可能握手失败或单向不通；
- 验收时同时检查云安全组和主机 nftables/iptables。

## 5. Web 设置

服务端和客户端设置页增加可编辑的 **Mimic 设置** 区域：

```text
传输模式: [ ] Mimic（默认普通模式）
网卡:       [自动/网卡名]
XDP 模式:   [自动/native/skb]
mimic 路径: [PATH/绝对路径]
额外参数:   [每行一个参数]
[保存]  当前运行模式: 普通/Mimic；保存后需重启
```

### 5.1 设置视图

服务端和客户端 `SettingsView` 增加：

```go
type MimicSettingsView struct {
    Enabled         bool     `json:"enabled"`
    Interface       string   `json:"interface"`
    XDPMode         string   `json:"xdp_mode"`
    Path            string   `json:"path"`
    ExtraArgs       []string `json:"extra_args"`
    Active          bool     `json:"active"`
    RestartRequired bool     `json:"restart_required"`
}
```

`SettingsView` 同时增加：

```go
RestartSupported bool `json:"restart_supported"`
```

- `Enabled` 等字段来自配置文件；
- `Active` 表示当前进程实际是否运行在 Mimic 模式；
- `RestartRequired` 为 `Enabled != Active`，或运行中的 Mimic 参数与配置不一致；
- `RestartSupported` 由 `INVOCATION_ID` 是否存在于当前进程环境决定；
- 页面必须同时显示“当前运行模式”和“配置模式”，避免用户以为保存即生效。

### 5.2 API

服务端和客户端各增加：

```text
PUT /api/settings/mimic
```

请求体：

```json
{
  "current_password": "...",
  "enabled": true,
  "interface": "eth0",
  "xdp_mode": "skb",
  "path": "/usr/bin/mimic",
  "extra_args": ["--padding=..."]
}
```

规则：

- 需要当前 Web 管理员密码、CSRF 头和同源 `Origin`；
- 复用设置页现有的失败限流；
- 校验配置后原子写入 YAML（保留 `.bak`）；
- **只写配置，不启动/停止当前 Mimic，不重启进程**；
- 返回更新后的 `MimicSettingsView`，其中 `RestartRequired: true`；
- 如果新配置非法（例如 Mimic + 端口跳跃），返回 400，运行配置和文件都不变；
- 服务端开启 Mimic 时，响应/页面必须带警告：
  “所有普通 HY2 客户端将无法连接，必须同时切换客户端”；
- 客户端开启 Mimic 时提示：“服务端和客户端都必须开启 Mimic”。

### 5.3 重启应用

Web 设置页提供显式的 **重启应用** 按钮。保存配置不会自动触发重启。

前置条件：

- 当前进程由 systemd 管理，并且 unit 使用 `Restart=always`
  （项目提供的 `hy2tund.service` / `hy2tun.service` 已满足）；
- 通过 `INVOCATION_ID` 环境变量检测 systemd；不在 systemd 下运行时返回 409，
  提示用户手动重启；
- 需要当前 Web 管理员密码、CSRF 头和同源 `Origin`；
- 失败限流沿用设置页现有策略。

行为：

1. Web 返回 `202 Accepted`，响应体包含 `{"restarting": true}`；
2. 延迟约 300ms 让 HTTP 响应刷出；
3. 直接结束当前进程（`os.Exit(0)`），不执行 shell；
4. systemd 根据 `Restart=always` 重启同一 unit；
5. 旧的 Mimic 子进程通过 `Pdeathsig` 随父进程退出；
6. 新进程重新读取配置，启动/停止 Mimic；
7. 页面显示“正在重启”，随后重新加载登录页。

安全与限制：

- 只重启当前进程，不指定 unit，不接受任意命令参数；
- **服务端 Web 不能重启客户端，客户端 Web 不能重启服务端**；两端必须分别
  保存配置并点击各自的“重启应用”；
- 服务端重启会断开所有客户端和映射；客户端重启会断开当前隧道；
- 页面必须显示中断警告；
- 非 systemd 环境、Windows、手动运行场景只显示手动重启命令；
- 如果新配置启动失败，systemd 会按 unit 策略重试；Web 可能暂时不可用，
  需要看 journal。

### 5.4 重启 API

服务端和客户端各增加：

```text
POST /api/restart
```

请求体：

```json
{
  "current_password": "..."
}
```

规则：

- 需要当前 Web 管理员密码、CSRF 头和同源 `Origin`；
- 只允许重启当前进程；
- 不在 systemd 下运行返回 `409 restart_unavailable`；
- 成功返回 `202`，随后进程退出；
- 该接口不接受 unit 名、命令、参数或文件路径。

页面保存成功后显示“已保存，重启后生效”，并在 `RestartRequired` 为 true
时持续显示提示和“重启应用”按钮。

### 5.5 安全

- Web 密码、CSRF、Origin、限流沿用现有设置页；
- 不允许 Web 直接执行 `mimic` 或 `systemctl`；
- 不允许 Web 修改 `mimic.enabled` 后立即生效；
- 重启只结束当前进程，由 systemd 重新拉起，不接受任意命令；
- 启动日志也必须记录模式 warning，不能只依赖 Web 页面。

## 6. 兼容性与切换

- **普通模式**：与 v1.2 完全一致，标准 HY2 客户端、端口跳跃都可用。
- **Mimic 模式**：服务端和客户端都必须开启；标准 HY2 客户端未开 Mimic
  会完全连不上。
- **端口跳跃**：Mimic 模式禁止；普通模式保留。
- **切换步骤**：
  1. 客户端 Web 保存 `mimic.enabled: true`，点击“重启应用”；
  2. 服务端 Web 保存 `mimic.enabled: true`，点击“重启应用”；
  3. 客户端会自动重连；验证控制握手和映射。
  也可以先服务端后客户端，但中间一定有一次连接中断；不能平滑切换。
  两个 Web 各自只重启自己，不能一键切换双端。
- **回滚**：两端都改回 `mimic.enabled: false` 并重启；普通客户端恢复。
- **标准 Hysteria 2 客户端**：如果它们也支持 Mimic，可以配置 Mimic 后连接；
  否则不能与 Mimic 服务端共存。
- **Salamander obfs**：Mimic 在 socket 之下工作，应与现有 `obfs.salamander`
  组合；验收矩阵必须包含 `obfs + mimic`，不能只测无 obfs。

## 7. 测试

### 7.1 自动测试

- 配置校验：
  - 默认 false；
  - `xdp_mode` 合法/非法；
  - Mimic + 端口集合/端口跳跃报错；
  - `extra_args` 非法字符。
- filter 生成：
  - 服务端通配 IPv4/IPv6；
  - 服务端指定 IP；
  - 客户端单 IP、多 IP、IPv6。
- 子进程管理：
  - 使用临时 fake `mimic` 脚本和临时 runtime dir；
  - 验证参数、启动、等待 lock、正常停止、意外退出回调；
  - 非 Linux 返回错误；
  - Mimic 意外退出后 `Start` 返回错误，客户端不会继续重连。
- 禁用模式：
  - `mimic.enabled: false` 时，旧的 `interface`/`path` 不参与校验；
  - 普通模式仍允许端口跳跃。
- 网卡选择：
  - 客户端按对端地址选网卡；
  - 服务端通配监听选默认路由网卡；
  - 服务端指定 IP 监听必须选拥有该 IP 的网卡。
- GSO：
  - 通过 helper 或配置构造测试，确认 Mimic 模式为 true、普通模式为 false。
- Web：
  - 服务端/客户端 Settings 包含 `MimicSettingsView`，字段值正确；
  - `Active`/`RestartRequired` 在保存前后正确；
  - `PUT /api/settings/mimic` 写文件但不改变运行时；
  - 非法配置（Mimic + 端口跳跃）返回 400，文件不变；
  - 服务端 Mimic 模式包含普通客户端不兼容警告；
  - 保存后页面提示重启；
  - `POST /api/restart` 需要当前密码和 CSRF；
  - 非 systemd 环境返回 409，不退出进程；
  - systemd 环境下成功返回 202，并通过可注入的退出函数验证调用；
  - 保存配置不会自动调用重启。

### 7.2 手工验收

在隔离测试端口（不碰生产 `24443`）：

1. 服务端和客户端安装 Mimic 及依赖；
2. 两端 `mimic.enabled: true`；
3. 验证控制握手、TCP 映射、UDP 映射；
4. 用 `tcpdump` 确认线上是 TCP 形状；
5. 用 iperf3 比较普通/Mimic 的吞吐、CPU、延迟；
6. 测试 MTU 1420/1408/1280 下的大包；
7. 测试 `obfs.type: salamander` + Mimic；
8. 同时检查云安全组和主机防火墙是否 TCP/UDP 都放行；
9. 记录内核、Mimic 版本、网卡、XDP 模式。

验收标准：

- 普通模式回归通过；
- Mimic 模式能握手并传输 TCP/UDP；
- Mimic 退出/缺失时明确失败，不回退；
- 端口跳跃配置在 Mimic 模式下被拒绝；
- 文档明确标准客户端会断开、切换需要两端同时进行。

## 8. 部署注意

- 当前服务端 Ubuntu 24.04 ARM64，内核 7.0，BPF/BTF 已开启；
- Mimic v0.7.1 有 Ubuntu 24.04 arm64 包；
- DKMS 需要匹配内核头文件，当前服务端未安装，需要先安装；
- 客户端 x86_64 Ubuntu 24.04，内核头文件已安装；
- Mimic 是 GPL-2.0-only，hy2tun 不打包它，只作为外部依赖；
- 生产切换前先在独立端口做完整实验。
