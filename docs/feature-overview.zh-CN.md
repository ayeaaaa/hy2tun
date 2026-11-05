# hy2tun v1.6.0 功能介绍

`hy2tun` 是一个基于 Hysteria 2 的反向端口转发工具：服务端在公网监听端口，客户端把内网服务映射出去，支持 TCP 和 UDP。适合远程 SSH、NAS、Web、DNS、游戏联机等场景。

最新版 **v1.6.0** 的核心变化是：**HY2 优先，UDP 不通时自动切 TCP，网络恢复后自动回 HY2。**

## 核心功能

- **反向 TCP/UDP 映射**：把内网端口映射到公网，支持固定端口、自动分配、允许/拒绝范围和离线端口保留。
- **HY2 传输**：基于 Hysteria 2，低延迟、抗丢包；标准 Hysteria 2 客户端可以使用 HY2 代理入口。
- **TCP 兜底（v1.6.0 新增）**：UDP 被封锁或严重丢包时，自动切换到 TLS/TCP；UDP 恢复后自动切回 HY2。
- **同一数字端口**：例如 `24443/UDP` 和 `24443/TCP` 同时提供，但使用两个独立 socket，防火墙需同时放行 TCP 和 UDP。
- **三种模式**：`hy2`、`tcp`、`hy2+tcp`。服务端全局设置，多服务端客户端可按服务器独立设置。
- **UDP over TCP**：TCP 模式下 UDP 映射不会消失，会封装成带帧的 UDP over TCP。
- **多服务端客户端**：一个客户端可以连接多个服务端，每个服务端有独立的认证、模式、映射、代理和端口跳跃配置。
- **本地代理**：客户端内置 SOCKS5 和 HTTP CONNECT，可配合浏览器或其他程序使用。
- **端口跳跃**：HY2 支持 UDP 端口范围跳跃，Linux 服务端可自动配置防火墙重定向。
- **两端 Web 控制台**：查看状态、映射、传输模式、实际传输、切换原因和探测错误。客户端控制台还内置最近日志视图；服务端日志通过 systemd journal 或配置的日志文件查看。
- **LuCI 与 CLI**：OpenWrt 可在 LuCI 中配置单服务端模式；命令行支持 `hy2tun config get|set`。
- **安全与运维**：TLS 1.3、CA/SNI/证书 pin、认证密码与 control token、凭据轮换、当前/上一代凭据、CSRF、登录限流、metrics Bearer token。
- **多平台**：Linux amd64/arm64/arm/mips/mipsle、Windows amd64 交叉构建、OpenWrt IPK、Docker、systemd。

## 适合用在哪

- 学校、公司、酒店网络封 UDP，但 TCP 能通；
- 运营商对 UDP 限速，晚高峰 HY2 丢包严重；
- 跨境线路 UDP 不稳定，TCP 正常；
- 移动网络、物联网卡只放行 TCP；
- 想保留 HY2 的低延迟，又希望 UDP 被封时有条后路。

## 怎么用

服务端和客户端配置都加一行：

```yaml
mode: "hy2+tcp"
```

服务端防火墙放行同一数字端口的 TCP 和 UDP，例如：

```text
24443/tcp
24443/udp
```

客户端会优先走 HY2；HY2 不可用时自动切 TCP，UDP 恢复后自动切回 HY2。

## 边界说明

- 切换瞬间已有 TCP/代理连接会断开，UDP 可能丢包；不做无缝迁移。
- TCP 兜底会增加延迟和队头阻塞，适合 DNS、SSH、小型 UDP 服务等“先保证能通”的场景；不适合低延迟游戏、语音或大流量 UDP。
- 标准 Hysteria 2 客户端可以使用 HY2 代理入口，但反向映射管理仍需要 hy2tun 客户端。
- 这是 hy2tun 自己的 TCP 传输，**不是 FRP 兼容协议**，也没有 P2P、UDP 打洞、Hysteria Realms。
- 认证、证书、协议错误不会触发 TCP 回退，这类问题需要修配置。
- Windows 目前只有交叉构建验证；OpenWrt 已完成 x86_64 真机验收，其他架构为交叉构建/QEMU 烟测。

## 下载

- Release：<https://github.com/ayeaaaa/hy2tun/releases/tag/v1.6.0>
- 项目：<https://github.com/ayeaaaa/hy2tun>
- 中文文档：<https://github.com/ayeaaaa/hy2tun/blob/main/README.zh-CN.md>
