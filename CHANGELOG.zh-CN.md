# 更新日志

[English](CHANGELOG.md) | **简体中文**

本文件记录项目的重要变更。

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
