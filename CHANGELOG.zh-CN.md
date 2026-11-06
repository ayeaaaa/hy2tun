# 更新日志

[English](CHANGELOG.md) | **简体中文**

本文件记录项目的重要变更。

## v1.6.4 - 2026-11-06

### 变更

- 客户端和服务端的流量统计改用 **客户端 → 服务端**、**服务端 → 客户端**，
  不再使用容易混淆的“上行/下行”。映射表保持原有列数，用两行显示两个方向。
- 服务端 metrics 的 HELP 文案改为实际转发方向；指标名称和数值不变。
- `tx_bytes` / `rx_bytes` JSON 字段名称和数值不变：`tx_bytes` 表示客户端到
  服务端的载荷字节，`rx_bytes` 表示服务端到客户端的载荷字节。
- 升级 `github.com/andybalholm/brotli` 到 v1.2.7、
  `github.com/klauspost/compress` 到 v1.20.2，修复 GO-2026-6888 和
  GO-2026-6865。GO-2026-5288 仍按“未启用 Hysteria sniff 且上游无修复版本”
  允许。

## v1.6.3 - 2026-11-05

### 新增

- 客户端 Web 控制台增加按服务端的“回退时序”表单，可调整
  `connect_timeout`、`heartbeat_interval`、`heartbeat_timeout`、
  `probe_interval`、`min_tcp_hold` 和 `recovery_successes`。保存需要
  CSRF 和当前 Web 密码，原子写入 YAML，并立即热更新运行中的策略，无需
  重启客户端。
- 未实现该可选接口的 API 返回 `501`，页面不显示表单。

### 变更

- 文档和示例补充 Web 控制台与 YAML 两种设置方式；LuCI 基础表单仍把
  `failover` 作为高级字段保留。

## v1.6.2 - 2026-11-05

### 新增

- `hy2+tcp` 支持按服务端调整 `failover`：`connect_timeout`、
  `heartbeat_interval`、`heartbeat_timeout`、`probe_interval`、
  `min_tcp_hold` 和 `recovery_successes`。单服务端写在顶层；多服务端写在
  每个 `servers[]` 条目里，互不影响。省略的字段使用保守默认值。
- 文档增加更快的中间档：连接 `8s`、心跳 `5s`、判死 `20s`、探测 `15s`、
  TCP 保持 `30s`、连续成功 3 次。

### 变更

- 基础配置编辑器把 `failover` 识别为已知高级字段并原样保留，不在基础表单
  中展示。
- 示例和 README 增加按服务端调整及中间档说明。

## v1.6.1 - 2026-11-05

### 修复

- 强制重连现在会在关闭控制流之前同步清空当前 owner。旧 reader 已经排队的
  Push 不会再在失效后被应用，从而避免错误修改客户端 revision 或映射。

### 变更

- 文档明确说明：客户端控制台内置最近日志视图；服务端日志写入 systemd
  journal 或配置的日志文件。
- 新增论坛用中文功能介绍：`docs/feature-overview.zh-CN.md`。
- Release notes 改为在构建目录之外生成，后续发布二进制不再带有
  `vcs.modified=true` / `+dirty` 元数据。
- 加固 E2E 轮询和 flood 清理（仅测试）。

## v1.6.0 - 2026-11-05

### 新增

- 为 hy2tun 控制流、反向 TCP 流、反向 UDP 映射和正向代理流增加 TLS 1.3
  TCP 传输。它复用现有的 HY2 密码/control token，以及现有的 `tls.ca`、
  `tls.sni`、`tls.insecure` 和 `tls.pin_sha256` 策略。TCP 传输要求
  TLS 1.3；即使启用不安全验证，证书 pin 仍会校验。
- 三种传输模式：`hy2`（默认）、`tcp` 和 `hy2+tcp`。服务端模式是全局的；
  多服务端客户端的每个服务端条目可独立设置模式。纯模式互斥：`hy2` 不启动
  TCP 监听或回退，`tcp` 不启动 UDP HY2 监听，并拒绝范围、端口列表和 UDP
  跳跃间隔。
- `hy2+tcp` 的同端口双监听：UDP HY2 监听和 TCP 监听使用同一数字端口，但
  是独立 socket。TCP 端点取第一个端口项，在使用范围或端口跳跃时保持输入
  顺序。防火墙必须同时放行该端口的 TCP 和 UDP。
- `hy2+tcp` 的自动回退与恢复：10 秒连接/握手预算、10 秒心跳、30 秒会话
  死亡超时、每 30 秒探测一次并带 ±20% 抖动；只有在连续三次成功探测且在
  TCP 上保持至少 60 秒后才回切。
- TCP 活跃时，UDP 映射使用 UDP over TCP 帧，载荷上限 4096 字节，每条映射
  有固定的 32 包发送队列。可靠有序会增加延迟和队头阻塞代价，队列压力下
  数据报可能被丢弃。
- 本机防火墙或沙箱以 `EPERM`/`EACCES` 拒绝 UDP I/O 时也会触发回退；文件
  系统权限错误以及认证、TLS、证书 pin、协议错误仍不会触发回退。
- 服务端和客户端 Web 模式控制、多服务端客户端的按服务端控制、LuCI 模式
  字段，以及 `hy2tun config get|set` 的 `mode` 支持。
- 诊断信息：配置模式、实际传输、切换原因和次数、最近切换、最近探测、探测
  错误、服务端记录的最后实际传输，以及客户端指标
  `hy2tun_client_transport_active` 和
  `hy2tun_client_transport_switches_total`。

### 变更

- `luci-app-hy2tun` 现在依赖 `hy2tun (>= 1.6.0)`。
- 没有 `mode` 的配置继续默认 `hy2`；加载或升级不要求添加该字段。默认的
  `server.yaml`、`client.yaml` 和 `client-multi.yaml` 示例仍然省略 `mode`；
  LuCI 包的基础示例保留显式 mode 字段。
- 改变活跃传输的模式切换会关闭当前会话：现有 TCP 和代理连接断开，UDP 映射
  状态关闭，排队的数据报可能丢失。不提供无缝迁移、恰好一次投递或重放。
- 只有可识别的网络故障会触发回退。认证、TLS、证书 pin、协议、配置、队列
  已满和取消错误会直接上报，绝不会降级到 TCP。

### 兼容性

- 现有 `hy2` 部署和标准 Hysteria 2 客户端不受影响。`hy2+tcp` 保留 HY2；
  纯 `tcp` 服务端不接受标准 Hysteria 2 客户端。
- 不支持模式的旧客户端继续使用 HY2，无法回退。新的 `hy2+tcp` 客户端连接
  旧的纯 HY2 服务端时仍使用 HY2；如果 UDP 被阻断，需要先升级服务端并开放
  TCP 才能回退。
- TCP 传输是 hy2tun 自有协议，不与 FRP 线协议兼容；本版本没有增加 P2P、
  Hysteria Realms 或 UDP 打洞。

### 验证

- 本地门槛：`go test ./... -count=1`、`go test -race ./... -count=1`、
  `go vet ./...`、`staticcheck ./...`、`make build`、
  `scripts/openwrt/test-ipk.sh` 和
  `scripts/openwrt/test-luci-controller.sh`。
- Linux amd64/arm64/arm/mips/mipsle 和 Windows amd64 交叉构建。
- Linux arm64/arm/mips/mipsle 的本地 QEMU user-mode 检查：`-version`、
  `--check-config` 和真实 TLS/TCP 帧往返测试。
- OpenWrt 24.10.2 x86_64 使用 v1.6.0 包完成真机验收：HY2 正常；双向 UDP
  DROP 后 9 秒回退到 TCP；99 秒后恢复 HY2；TCP 和 UDP 映射均可在 TCP 回退
  上工作；错误认证被拒绝且不回退；TCP 端口冲突回滚；LuCI 页面/模式与启停
  检查通过；回滚到 v1.5.4 后无遗留映射。Windows 真机验收仍属外部检查项，
  当前只验证了 Windows amd64 交叉构建。

## v1.5.4 - 2026-10-31

### 新增

- 客户端/服务端 Web 控制台和 OpenWrt LuCI 页面增加 GitHub 项目链接。

## v1.5.3 - 2026-10-30

### 新增

- `/metrics` 支持可选的 `web.metrics_token` Bearer 认证；为空时保持仅适合
  回环环境的历史无认证行为。
- CI 增加 `staticcheck`。
- OpenWrt 包 post-install 会验证安装的二进制能在目标 CPU 上实际运行。

### 变更

- systemd 单元增加更严格的文件系统、设备、内核和地址族沙箱限制，同时保留
  `/etc/hy2tun` 和 `/var/lib/hy2tun` 可写。

## v1.5.2 - 2026-10-30

### 变更

- Docker 示例改用可写配置目录和可写状态目录；原来的只读 YAML 挂载会导致
  生成密码、客户端 ID、分配端口和热配置写入失败。

### 修复

- 加载配置时自动把 group/other 权限收紧为 0600。
- 服务端增加控制客户端、每客户端映射、每映射并发公网 TCP 连接和待处理
  data-stream token 的资源上限。
- 反向 UDP peer 的读缓冲从 64 KiB 降到 4097 字节，避免在 peer 上限时每个
  映射占用最多 64 MiB。
- 防火墙命令增加 10 秒超时，nft/iptables 卡住时不会无限阻塞启动。
- Web 控制台密码超过 bcrypt 72 字节限制时返回校验错误，而不是内部错误。
- `log.file` 达到 10 MiB 时轮转并保留 5 份备份，不再无限增长。
- 服务端启动时自动清理最后在线时间超过七天的离线客户端。

## v1.5.1 - 2026-10-30

### 新增

- 新增已提交的 LuCI controller 集成测试，使用假 CLI/init 脚本覆盖路由/CSRF
  注册、保存/保存并启动、服务操作、日志和配置读取失败，无需 root 或路由器。

### 修复

- LuCI 基础配置保存路径在 Web 控制台密码超过 bcrypt 72 字节限制时返回
  校验错误，而不是内部错误。
- LuCI 页面在发送请求前拒绝少于 8 字节的 HY2 认证和少于 16 字节的 control
  token。
- 服务状态轮询严格限制在请求的 5 秒内。
- controller 的 payload 临时文件直接以 600 权限创建，不再先打开再 chmod。
- `hy2tun config` 的 init 状态检查增加超时；`config set` 拒绝 JSON 对象后的
  尾随数据。

## v1.5.0 - 2026-10-30

### 新增

- LuCI **服务 → hy2tun** 页面：单服务端基础配置表单、保存/保存并启动、
  启动/停止/重启、启用/停用、最近 50 行日志，以及客户端 Web 控制台入口。
- `hy2tun config get|set` 辅助命令：SHA-256 revision 校验、配置排他锁、
  原子写入、Web 密码 bcrypt 哈希，以及稳定 JSON 错误和固定退出码。
- 保存前校验配置：基础字段无效时返回字段错误，高级字段校验失败时不会覆盖
  现有文件。
- 服务运行期间 LuCI 表单只读；保存要求服务已停止，过期 revision 会以冲突
  拒绝。
- 基础表单的安全服务端迁移：更换服务端时必须重新填写 HY2 认证和 control
  token，并清除已保存的 client ID 和 revision。
- 保存基础表单时保留单服务端高级字段。多服务端配置和未知顶层字段在 LuCI 中
  保持只读或受保护；已轮换凭据请在客户端控制台管理。

### 变更

- OpenWrt 安装包不再在安装时创建 `/etc/hy2tun/client.yaml` 或启用服务。
  随包提供的 `client.yaml.example` 只包含单服务端基础字段，首次配置在
  LuCI 中完成（安装 → 填写表单 → 保存 → 启动 → 打开控制台）。
- `luci-app-hy2tun` 现在依赖 `hy2tun (>= 1.5.0)`。

## v1.4.4 - 2026-10-29

### 修复

- LuCI 状态页不再返回 `502 Bad Gateway`：控制器在使用前加载
  `nixio.fs`，服务状态命令不再把 `inactive` 写入 CGI 响应。LuCI 菜单缓存
  刷新后可在“服务”中看到 hy2tun 入口。

## v1.4.3 - 2026-10-29

### 修复

- OpenWrt `.ipk` 改用 `opkg` 期望的 gzip-tar 容器格式；之前的 ar 格式会被
  判定为 malformed。已在 ImmortalWrt 24.10.2 x86_64 上通过 `opkg install`
  验证，包括 LuCI 包和真实反向 TCP 映射。

## v1.4.2 - 2026-10-29

### 新增

- 从 `CHANGELOG.md` 和 `CHANGELOG.zh-CN.md` 生成中英文 GitHub Release
  说明；缺少任一语言时发布流程会失败。

## v1.4.1 - 2026-10-29

### 新增

- OpenWrt 客户端 `.ipk` 安装包，包含 procd 启动脚本和示例配置。
- `luci-app-hy2tun` 包，在 LuCI 中提供服务状态页和客户端 Web 控制台入口。
- Release 和 CI 增加 OpenWrt 包构建与校验。

## v1.4.0 - 2026-10-29

### 新增

- 多服务端客户端配置：一个进程连接多个独立服务端，每个服务端有独立的
  client ID、映射、凭据、重连循环、端口跳跃和代理。
- 每个服务端独立的 SOCKS5/HTTP 代理监听，默认关闭，可在客户端 Web 控制台
  修改，并进行端口冲突校验。
- 代理关闭或修改地址时会取消进行中的 dial；配置写入区分 rename 已提交和
  pre-commit 失败。
- 客户端服务端列表和按服务端作用域的 Web API；旧单服务端 API 继续映射到
  `default`。

### 变更

- 继续兼容旧单服务端 YAML，不自动转换格式。
- 多服务端配置只使用顶层 `web` 和 `log`；存在 `servers` 时拒绝旧的单服务端
  顶层字段。

## v1.3.3 - 2026-10-29

### 安全

- 恢复修改 trust-proxy 时的当前密码确认。审查发现免密修改可绕过基于 IP 的
  限流；端口策略修改仍然免密。
- 静态资源使用 `Cache-Control: no-cache`，避免浏览器在升级后继续运行旧
  JavaScript 操作新的设置表单。
- 离线客户端删除改为事务式：rename 前的保存失败会恢复内存状态；rename 已
  提交但目录 sync 失败时按已提交处理并记录日志，不回滚内存。

## v1.3.2 - 2026-10-28

### 变更

- 服务端设置中的日常端口策略和 trust-proxy 修改不再要求当前 Web 密码。
  Web 管理员和 HY2 凭据操作仍然需要确认。

## v1.3.1 - 2026-10-28

### 新增

- 服务端 Web 控制台可以删除离线客户端，同时删除其持久化映射和软端口预约。
  在线客户端必须先断开。

## v1.3.0 - 2026-10-28

### 新增

- 两个二进制都支持 `--check-config`，检查 TLS、Web TLS、bcrypt 和运行目录。
- 两个 Web 控制台增加 `/healthz` 和 Prometheus `/metrics`。
- Linux 服务端 `listen` 支持 UDP 端口范围时，自动安装 nftables/iptables
  重定向，并在关闭时删除规则。
- Dockerfile target、Docker Compose 示例和 Release 安装脚本。
- CI、Dependabot、安全策略、Issue/PR 模板、贡献指南和 Tag 自动发布。
- 控制帧、端口策略基准测试，以及帧、端口范围、listen 解析 fuzz 测试。

### 变更

- 服务端 `listen` 接受 `host:start-end` 和逗号分隔范围；第一个端口是实际
  绑定端口，范围端口全部保留。
- 升级 `golang.org/x/crypto`、`golang.org/x/net`、`golang.org/x/sys` 和
  `golang.org/x/text`，修复已知模块漏洞。
- 客户端端口跳跃 warning 同时提示范围监听和重定向。
- 拆分过大的服务端 mapping 和 Web handler 文件，行为不变。

## v1.2.0 - 2026-10-28

### 新增

- 客户端 Hysteria 2 UDP 端口跳跃。
- `server` 接受单端口、逗号分隔端口、范围和混合列表。
- `transport.udp.hop_interval`、`min_hop_interval` 和 `max_hop_interval`。
- 每个跳跃 socket 都支持 Salamander obfs 组合。
- 端口跳跃部署指南，包含 nftables/iptables 示例。
- 简体中文 README（`README.zh-CN.md`）。

### 变更

- 单端口配置保持原有 YAML 结构和行为。
- `443,443` 这类只有一个唯一端口的集合规范化为单端口。
- Release 归档同时包含中英文 README。

## v1.1.0 - 2026-10-28

### 新增

- Web 热配置：Web 管理员、HY2 凭据、端口策略、trust-proxy 和客户端名称。
- 基于代际的凭据轮换，并保留上一代凭据。
- 自动推送到具备能力的在线客户端，以及客户端手动同步。
- 显式撤销上一代凭据。
- 经过认证的凭据显示，默认掩码，响应带 `Cache-Control: no-store`。

## v1.0.0 - 2026-10-27

### 新增

- HY2 代理服务端和反向隧道客户端。
- 基于 HY2 流的反向 TCP 映射。
- 基于 HY2 原生数据报的反向 UDP 映射。
- 服务端和客户端 Web 控制台。
- 映射管理、实时状态、日志和凭据处理。
- Linux、OpenWrt 和 Windows 客户端构建。
