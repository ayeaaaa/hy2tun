# hy2tun

[English](README.md) | **简体中文**

[![CI](https://github.com/ayeaaaa/hy2tun/actions/workflows/ci.yml/badge.svg)](https://github.com/ayeaaaa/hy2tun/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`hy2tun` 是一个基于官方 [Hysteria 2](https://github.com/apernet/hysteria)
核心实现的反向端口映射隧道。内网客户端主动连接到公网服务端，把选定的 TCP
和 UDP 服务暴露在服务端的公网端口上。同一个服务端仍然是普通的 HY2 代理服务端，
现有 Hysteria 2 客户端无需任何修改即可继续使用。

- `hy2tund` —— 公网服务端：HY2 代理入口、反向映射公网监听、控制端点和
  管理控制台。
- `hy2tun` —— 内网客户端：主动发起 HY2 连接、反向 TCP 流、反向 UDP 数据报、
  可选的本地 SOCKS5/HTTP 代理，以及管理控制台。

两个二进制都是静态链接的 Go 程序，Web 资源已嵌入其中。它们不需要数据库、
Node.js、CDN 或任何运行时依赖。

> 本文是英文 README 的简体中文版。命令、配置字段、API 和状态名保持原样；
> 如果中英文内容有差异，以英文版为准。

## 功能

- 标准 HY2 TCP/UDP 代理服务端，兼容官方 Hysteria 2 客户端。
- FRP 风格的反向 TCP 映射：服务端公网端口转发到客户端可达的 TCP 服务。
- 使用 HY2 原生数据报的反向 UDP 映射，不经过可靠流，因此实时 UDP 不会被
  队头阻塞。
- 客户端配置是权威配置。服务端控制台可以向在线客户端推送映射变更；客户端
  应用变更、原子写入自己的 YAML 文件，并确认新的 revision。
- 端口白名单支持单端口、范围、拒绝规则、自动分配和离线端口保留。
- 客户端可选 HY2 UDP 端口跳跃，支持端口集合；当服务端 `listen` 配置为范围时，
  Linux 服务端会自动配置 nftables/iptables 重定向；单端口监听仍支持手动重定向。
- 服务端和客户端都有 Web 控制台，支持登录、CSRF 防护、实时状态、映射管理
  和日志。
- 两个控制台都支持热配置编辑：Web 管理员、HY2 凭据、服务端端口策略、
  服务端 trust-proxy 和客户端名称会经过校验、原子写入，并在不重启进程的
  情况下生效。
- 基于代际（generation）的凭据轮换，保留上一代凭据；支持自动推送到具备能力
  的在线客户端，或从客户端控制台手动同步，并支持显式撤销。
- 经过认证的凭据显示：可以显示当前 HY2 凭据（服务端还可显示上一代）；默认
  掩码显示，响应带 `no-store`。
- 客户端可选本地 SOCKS5 和 HTTP CONNECT 代理，复用同一条 HY2 连接。
- 同一套源码可构建 Linux、OpenWrt 和 Windows 客户端。

## 不包含的功能（v1）

- 没有多用户账户、审批流程、租户隔离、配额或计费。每个服务端只有一套 HY2
  认证配置和一个控制令牌；密码认证在轮换期间会保留上一代凭据。
- 没有 P2P、Hysteria Realms 或 UDP 打洞。客户端始终主动连接服务端。
- 没有 TUN 模式、透明代理、基于域名的路由、ACL、负载均衡或 CDN 前置。
- 一个客户端只能连接一个服务端。一个客户端不能同时注册到多个服务端。
- 服务端控制台只能编辑
  [热配置编辑](#热配置编辑) 中列出的设置；监听地址、TLS、obfs、带宽、QUIC、
  Web 监听/TLS、日志、状态文件和认证模式/用户表仍然需要修改文件并重启。
- 客户端控制台不能编辑服务端地址、端口跳跃 transport、TLS、obfs、带宽、
  客户端 ID、代理监听或 Web 监听/TLS；这些仍然留在 `client.yaml`。客户端
  控制台可以重命名客户端，并通过手动同步轮换 HY2 凭据。
- 离线客户端的映射配置在服务端控制台中只读；详情页可以删除该客户端及其
  持久化映射和端口预约。仍可在客户端主机上本地更新其凭据。
- 没有自动更新、托盘程序或原生 Windows 服务。Windows 使用任务计划程序。
- 服务端范围监听仅支持 Linux，并且需要 root 或 `CAP_NET_ADMIN` 以及
  nftables 或 iptables。非 Linux 服务端只能使用单端口监听加外部重定向；见
  [端口跳跃](#端口跳跃)。
- 没有实现 SOCKS5 UDP ASSOCIATE；本地 SOCKS5 代理只支持 TCP CONNECT。
  HTTP CONNECT 也只支持 TCP。
- 没有 UPX 压缩。

## 架构

```text
标准 HY2 客户端 ──────── QUIC / HY2 ────────┐
                                            │
hy2tun 客户端 ────────── QUIC / HY2 ────────┤
  │ 控制流（注册 / 同步 / open）             │
  │ TCP 数据流                              ├── hy2tund（公网 Linux 服务端）
  │ 原生数据报（UDP）                       │     ├── HY2 代理出口（TCP/UDP）
  │                                         │     ├── 每条映射一个公网 TCP 监听
  ├── 本地 Web 控制台（局域网）              │     ├── 每条映射一个公网 UDP socket
  ├── 本地 SOCKS5 / HTTP 代理（可选）        │     └── 管理 Web + API
  └── 内网 TCP/UDP 服务                     │
```

### 反向 TCP 路径

1. 客户端向服务端打开一条长连接控制流。
2. 公网用户连接到服务端上已映射的端口。
3. 服务端生成一次性 128 位令牌，并在控制流上发送 `open` 消息。
4. 客户端向 `_hy2tun_data_<token>:0` 打开数据流，并连接本地目标。
5. 服务端的 `Outbound.TCP` 识别虚拟地址，接管等待中的公网连接，然后由
   HY2 核心双向复制字节。
6. 任意一端关闭都会关闭另一端。

### 反向 UDP 路径

1. 客户端为每条映射打开一个 HY2 UDP 会话，并向
   `_hy2tun_udp_<token>:0` 发送探测包。
2. 服务端的 `Outbound.UDP` 把令牌映射到该映射的公网 UDP socket。
3. 公网数据报带着公网对端地址投递给客户端。
4. 客户端为每个公网对端保留一个本地 UDP socket，写入目标，并通过同一个
   HY2 UDP 会话回送响应。
5. 20 秒 keepalive 保持 HY2 UDP 会话；空闲对端在 60 秒后回收。

虚拟的 `_hy2tun_*` 地址只由配套客户端使用。标准 HY2 客户端不会打开这些
地址，因此线协议保持兼容。

## 环境要求

- 构建需要 Go 1.26.10 或更高版本（包含标准库安全修复）。`go.mod` 锁定 HY2 核心为 v2.13.0。
- 一台有 TLS 证书的公网服务端（支持自签名证书：客户端配置对应 CA，可选
  叠加证书固定）。
- 到服务端 HY2 端口的 UDP 可达性；以及到每个公网映射端口的 TCP 可达性。
- 部署示例面向 Linux + systemd、OpenWrt + procd，或 Windows + 任务计划
  程序。

## 构建

```bash
make build      # 生成 bin/hy2tund 和 bin/hy2tun
make test       # go test ./...
make race       # go test -race ./...
make release    # ./scripts/release.sh，产物在 dist/
```

`make build` 和 release 脚本使用 `CGO_ENABLED=0`、
`-trimpath -ldflags "-s -w"`，因此二进制是静态且已裁剪的。发布矩阵：

| 系统 | 架构 | 额外参数 |
| --- | --- | --- |
| linux | amd64 | |
| linux | arm64 | |
| linux | arm | `GOARM=7` |
| linux | mipsle | `GOMIPS=softfloat` |
| linux | mips | `GOMIPS=softfloat` |
| windows | amd64 | `.exe` |

每个归档命名为 `hy2tun_<version>_<os>_<arch>.tar.gz`（Windows 为 `.zip`），
包含：

- `hy2tund` 和 `hy2tun`（Windows 带 `.exe`），可直接安装，无需改名；
- `examples/server.yaml` 和 `examples/client.yaml`；
- `README.md` 和 `README.zh-CN.md`；
- 完整的 `scripts/` 目录，包含 systemd、OpenWrt 和 Windows 部署说明；
- 覆盖两个二进制的 `SHA256SUMS`。

`dist/SHA256SUMS` 包含每个归档的 SHA-256。可以设置 `VERSION`、`COMMIT` 或
`DATE` 覆盖嵌入二进制的值：

```bash
VERSION=1.0.0 make release
```

## Docker

`Dockerfile` 提供 `server` 和 `client` 两个 target：

```bash
docker build --target server -t hy2tun-server .
docker build --target client -t hy2tun-client .
```

需要使用 host 网络，HY2 UDP 端口和公网映射端口才能正常工作：

```bash
docker run --network host --restart unless-stopped \
  -v "$PWD/server.yaml:/etc/hy2tun/server.yaml:ro" \
  -v "$PWD/certs:/etc/hy2tun/certs:ro" \
  -v hy2tund-state:/var/lib/hy2tun \
  hy2tun-server
```

完整双服务示例见 [`docker-compose.yml`](docker-compose.yml)。镜像以
`nonroot` 运行；如果需要绑定 1024 以下端口，请使用高权限端口或授予
`CAP_NET_BIND_SERVICE`。

## 从 Release 安装

在解压后的 release 目录执行：

```bash
sudo ./scripts/install.sh server
sudo ./scripts/install.sh client
```

脚本会安装二进制和 systemd unit，创建 `/etc/hy2tun` 与 `/var/lib/hy2tun`，
并且只在配置不存在时复制示例配置。启动服务前请先编辑配置。

## 配置检查

不绑定端口、不写状态文件，只校验配置：

```bash
hy2tund --check-config -c /etc/hy2tun/server.yaml
hy2tun --check-config -c /etc/hy2tun/client.yaml
```

会检查 YAML、必填字段、TLS 证书/私钥、Web TLS、bcrypt 哈希以及日志/状态目录。
成功时输出 `configuration OK`。

## 健康检查与指标

启用 Web 控制台后，两个二进制都提供：

- `GET /healthz` —— 进程正常时返回 `200` 和 `ok`。
- `GET /metrics` —— Prometheus 文本指标，包含聚合流量、映射数、连接状态、
  凭据代际和运行时间。

这两个端点有意不要求登录，方便本机监控。请把 Web 监听保持在回环地址、VPN
或可信反向代理后面，不要直接暴露到公网。

## 快速开始

完整示例文件在 [`examples/`](examples)。它们是合法配置，但包含占位符；
请替换所有 `change-me` 值和路径。

### 1. 服务端证书

公网部署建议使用正常证书（例如 Let's Encrypt），把 `fullchain.pem` 和
`privkey.pem` 复制到 `/etc/hy2tun/cert.pem` 和 `/etc/hy2tun/key.pem`。

自签名测试证书：

```bash
install -d -m 0755 /etc/hy2tun /var/lib/hy2tun
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes -days 3650 -subj "/CN=hy2tun.example.com" \
  -addext "subjectAltName=DNS:hy2tun.example.com" \
  -keyout /etc/hy2tun/key.pem -out /etc/hy2tun/cert.pem
chmod 600 /etc/hy2tun/key.pem
```

把 `/etc/hy2tun/cert.pem` 复制到客户端，并把 `tls.ca` 指向该文件。如果证书
由公共 CA 签发，`tls.ca` 留空，使用系统信任库。

### 2. 服务端配置

把 `examples/server.yaml` 复制到 `/etc/hy2tun/server.yaml` 并编辑：

```yaml
listen: ":443"

auth:
  type: password
  password: "a-long-random-password"

tls:
  cert: "/etc/hy2tun/cert.pem"
  key: "/etc/hy2tun/key.pem"

control:
  token: "a-different-long-random-token"   # openssl rand -hex 32

credentials:
  generation: 1   # 凭据轮换时自动递增

ports:
  allow: ["2222", "8000-8100", "51820"]
  auto_range: "20000-29999"

web:
  listen: "127.0.0.1:7500"
  username: "admin"
  password_hash: ""

state_file: "/var/lib/hy2tun/state.json"
```

启动：

```bash
sudo ./bin/hy2tund -c /etc/hy2tun/server.yaml   # release 包中为 ./hy2tund
```

如果 `web.password_hash` 为空，`hy2tund` 会生成一个 20 位密码，向 stderr
打印一次，并把 bcrypt 哈希写入 `server.yaml`。客户端控制台也一样。请保存
打印出的密码；哈希无法反推出明文。

### 3. 客户端配置

把 `examples/client.yaml` 复制到 `/etc/hy2tun/client.yaml` 并编辑：

```yaml
server: "hy2tun.example.com:443"
auth: "a-long-random-password"

tls:
  sni: "hy2tun.example.com"
  ca: "/etc/hy2tun/cert.pem"   # 公共 CA 签发时留空
  insecure: false
  pin_sha256: ""

control:
  token: "a-different-long-random-token"

credentials:
  generation: 1   # 由服务端推送或手动同步更新

client:
  id: ""
  name: "home-router"
  revision: 1

mappings:
  - id: "m-ssh"
    type: tcp
    remote_port: 2222
    local_addr: "192.168.1.10:22"
    note: "NAS SSH"
    enabled: true
  - id: "m-wireguard"
    type: udp
    remote_port: 51820
    local_addr: "192.168.1.10:51820"
    note: "WireGuard"
    enabled: true

web:
  listen: "0.0.0.0:7501"
  username: "admin"
  password_hash: ""
```

启动：

```bash
sudo ./bin/hy2tun -c /etc/hy2tun/client.yaml   # release 包中为 ./hy2tun
```

在客户端局域网打开 `http://<客户端局域网 IP>:7501/`，在服务端打开
`http://127.0.0.1:7500/`（或使用下面的 SSH 隧道）。首次登录使用启动时
打印的生成密码。

两个控制台都有 **Settings（设置）** 区域用于热更新。修改凭据或端口策略前，
请先阅读 [热配置编辑](#热配置编辑)。

### 4. 安装为服务

**systemd（Linux 服务端和客户端）：**

从源码构建：

```bash
sudo install -m 0755 bin/hy2tund bin/hy2tun /usr/local/bin/
```

从 release 包安装（在解压目录执行）：

```bash
sudo install -m 0755 hy2tund hy2tun /usr/local/bin/
```

然后安装 unit 并启用服务：

```bash
sudo install -d -m 0755 /etc/hy2tun /var/lib/hy2tun
sudo install -m 0644 scripts/systemd/hy2tund.service /etc/systemd/system/
sudo install -m 0644 scripts/systemd/hy2tun.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hy2tund    # 在服务端执行
sudo systemctl enable --now hy2tun     # 在客户端执行
sudo journalctl -u hy2tund -f
```

**OpenWrt（procd）：**

在解压后的 Linux 归档根目录执行：

```bash
opkg update && opkg install ca-bundle
cp hy2tun /usr/bin/hy2tun
chmod +x /usr/bin/hy2tun
install -d -m 0755 /etc/hy2tun
cp examples/client.yaml /etc/hy2tun/client.yaml
chmod 600 /etc/hy2tun/client.yaml
cp scripts/openwrt/hy2tun.init /etc/init.d/hy2tun
chmod +x /etc/init.d/hy2tun
/etc/init.d/hy2tun enable
/etc/init.d/hy2tun start
```

在外部编辑 `/etc/hy2tun/client.yaml` 后，用 `service hy2tun restart` 应用。
不要让 procd 监视该文件：客户端在启动、回写分配端口、服务端推送和 Web 编辑
时都会重写它，文件监视会导致每次写入都重启隧道。

MIPS 路由器使用 `linux/mipsle` 或 `linux/mips` 构建；ARM 路由器使用
`linux/arm` 或 `linux/arm64`。MIPS 构建已经使用 `GOMIPS=softfloat`。如果不
确定，在路由器上执行 `uname -m`。如果闪存空间紧张，只部署客户端二进制，
或使用 extroot。

**Windows：** 任务计划程序和防火墙说明见
[`scripts/windows/README.md`](scripts/windows/README.md)。

## 配置参考

### 服务端（`server.yaml`）

| 字段 | 含义 |
| --- | --- |
| `listen` | HY2 UDP 监听地址，例如 `:443`。端口跳跃时仍保持单端口；见 [端口跳跃](#端口跳跃)。 |
| `listen_host` | 公网映射监听的可选地址；留空绑定所有接口。 |
| `auth.type` | `password` 或 `userpass`。 |
| `auth.password` | `password` 认证使用的 HY2 密码。 |
| `auth.users` | `userpass` 认证的用户名到密码映射。 |
| `tls.cert`、`tls.key` | 服务端证书和私钥，两者都必填。 |
| `obfs.type` | 空或 `salamander`。 |
| `obfs.salamander.password` | 共享混淆密码。 |
| `bandwidth.up`、`bandwidth.down` | HY2 带宽限制；`0` 表示不限制。 |
| `bandwidth.ignore_client` | 忽略客户端的带宽设置。 |
| `quic.*` | QUIC 流/连接窗口、空闲超时和入站流上限。 |
| `control.token` | 共享控制密钥，至少 16 字节。建议使用 32 个随机字节。 |
| `credentials.generation` | 当前凭据代际。缺失或 `0` 表示 `1`；轮换会写入。 |
| `credentials.previous_generation` | 轮换期间保留的上一代；`0` 表示没有。 |
| `credentials.previous_auth` | 在撤销或下一次轮换替换前有效的上一代 HY2 密码。与 `previous_generation` 一起必填。 |
| `credentials.previous_control_token` | 在撤销或下一次轮换替换前有效的上一代控制令牌。与 `previous_generation` 一起必填。 |
| `ports.allow` | 客户端可以请求的端口或范围。为空表示所有空闲端口。 |
| `ports.deny` | 始终拒绝的端口或范围。拒绝优先于允许。 |
| `ports.auto_range` | 当 `allow` 为空时，`remote_port: 0` 使用的范围。默认 `20000-29999`。 |
| `ports.offline_reserve` | 离线客户端保留软预约的时长。默认 `24h`。热更新要求至少 `1s`；`0` 会被拒绝。 |
| `web.listen` | 管理地址。示例使用 `127.0.0.1:7500`。 |
| `web.username` | 控制台用户名。设置 `web.listen` 时默认 `admin`。 |
| `web.password_hash` | bcrypt 哈希。为空时首次启动生成密码。 |
| `web.trust_proxy` | 登录限流是否信任 `X-Forwarded-For`/`X-Real-IP`。仅当代理会用直连客户端地址覆盖这两个头时启用。默认 `false`。 |
| `web.tls.cert`、`web.tls.key` | 控制台可选 HTTPS 证书对。 |
| `state_file` | 持久化状态 JSON。为空时状态只保存在内存。 |
| `log.level` | `debug`、`info`、`warn` 或 `error`。 |
| `log.file` | 可选日志文件。为空时输出到 stderr/journal。 |

### 客户端（`client.yaml`）

| 字段 | 含义 |
| --- | --- |
| `server` | 服务端 HY2 监听地址，`host:port`；端口跳跃时可使用端口集合，例如 `host:443,8443` 或 `host:30000-40000`。 |
| `auth` | HY2 密码；`userpass` 认证时为 `user:password`。 |
| `tls.sni` | TLS 服务器名称。为空时使用 `server` 的主机部分。 |
| `tls.insecure` | 跳过证书验证。不要在不可信网络使用。 |
| `tls.pin_sha256` | 服务端证书 DER 字节的 SHA-256。 |
| `tls.ca` | 私有/自签名服务端证书的 CA 证书包。 |
| `obfs.type` | 必须与服务端一致。空或 `salamander`。 |
| `bandwidth.up`、`bandwidth.down` | 可选的客户端 HY2 限速。 |
| `transport.udp.hop_interval` | 多端口 `server` 的固定跳跃间隔。默认 30s，最小 5s。 |
| `transport.udp.min_hop_interval`、`max_hop_interval` | 随机跳跃间隔；两者必须同时配置，最小 5s，且与 `hop_interval` 互斥。 |
| `control.token` | 必须与服务端的 `control.token` 一致。 |
| `credentials.generation` | 客户端凭据代际。缺失或 `0` 表示 `1`；服务端推送或手动同步会写入。客户端忽略 `previous_*` 字段；这些字段保留。 |
| `client.id` | 稳定的客户端 ID。为空时首次启动生成。 |
| `client.name` | 服务端控制台显示的名称。 |
| `client.revision` | 配置 revision。客户端自动更新。 |
| `mappings` | 反向映射列表（见下文）。 |
| `web.listen` | 客户端控制台地址。示例使用 `0.0.0.0:7501`。 |
| `web.username`、`web.password_hash` | 控制台登录。哈希为空时生成密码。 |
| `web.trust_proxy` | 登录限流是否信任 `X-Forwarded-For`/`X-Real-IP`。仅当代理会用直连客户端地址覆盖这两个头时启用。默认 `false`。 |
| `web.tls.cert`、`web.tls.key` | 客户端控制台可选 HTTPS。 |
| `proxy.socks5` | 可选的本地 SOCKS5 监听地址，例如 `127.0.0.1:1080`。 |
| `proxy.socks5_auth` | 可选的 SOCKS5 `user:password`。 |
| `proxy.http` | 可选的本地 HTTP CONNECT 监听地址。 |
| `proxy.http_auth` | 可选的 HTTP CONNECT `user:password`。 |
| `log.level`、`log.file` | 日志配置。 |

`credentials.generation` 是状态标签，不是认证因素；不要递减。服务端的
`credentials.previous_*` 字段由轮换和撤销维护。也可以手工预置一对凭据，
但服务端要求 `previous_generation` 为 `0` 或小于 `generation` 的正数，
并且非零时必须同时提供两个上一代密钥。与当前密钥一样，它们保存在
`server.yaml` 及其 `.bak` 中，权限均为 `0600`。

### 映射字段

```yaml
- id: "m-ssh"                 # 在客户端内唯一；为空时自动生成
  type: tcp                   # tcp 或 udp
  remote_port: 2222           # 0 = 服务端从允许范围自动分配
  local_addr: "192.168.1.10:22"
  note: "NAS SSH"             # 可选，两个控制台都会显示
  enabled: true               # false 保留在文件中但不注册
```

修改映射的类型、公网端口或本地目标会关闭该映射的活跃公网连接。只修改
note 会保持连接。删除映射会关闭其监听/socket 以及所有活跃连接。

### 端口白名单语法

`ports.allow` 和 `ports.deny` 是 YAML 字符串列表。每一项是：

- 单个端口：`"2222"`
- 包含端点的范围：`"8000-8100"`

规则：

- `allow` 为空表示允许所有空闲端口。
- `allow` 非空时，显式请求和自动分配都限制在这些范围内。
- `deny` 始终优先，即使端口也在 `allow` 中。
- `remote_port: 0` 在 `allow` 非空时从 `allow` 分配，否则从
  `ports.auto_range` 分配。
- 服务端保留自己的 HY2 监听端口和 Web 端口，并跳过已被其他映射占用的端口。
- TCP 和 UDP 独立检查：TCP 2222 和 UDP 2222 可以共存。
- 离线保留是软预约：自动分配在 `offline_reserve` 期间跳过它，但其他客户端
  仍可显式请求该端口。

### 生成 Web 密码哈希

最简单的方式是留空 `web.password_hash: ""`。首次启动时程序生成一个 20 位
密码，向 stderr 打印一次，只在 YAML 文件中保存 bcrypt 哈希。重启不会再次
打印。

如果要自己选择密码，可以用 `htpasswd` 生成 bcrypt 哈希：

```bash
htpasswd -bnBC 12 "" 'your-password' | tr -d ':\n'
# $2y$12$...
```

或者使用 Python 的 `bcrypt` 包：

```bash
python3 -c 'import bcrypt; print(bcrypt.hashpw(b"your-password", bcrypt.gensalt()).decode())'
```

把输出粘贴到 `web.password_hash`。明文永远不会写入磁盘或日志。修改哈希后
重启进程。

### TLS 证书固定

`tls.pin_sha256` 固定服务端证书本身，与 CA 链无关：

```bash
openssl x509 -in cert.pem -outform der | sha256sum
```

值是 64 个字符的十六进制摘要（可接受可选的 `sha256:` 前缀和冒号）。证书
固定不会替代正常的证书验证：保留 `tls.ca` 配置，或者只在理解风险的情况下
设置 `tls.insecure: true`。同时配置 CA 和 pin 时，两者都必须匹配。

## 热配置编辑

两个 Web 控制台都有 **Settings（设置）** 区域。修改会先校验，以 `0600`
权限原子写入 YAML 文件（旧文件保留为 `.bak`），然后才应用到运行中的进程。
如果写入失败，运行配置保持不变，API 返回错误。下面标记为在线可编辑的设置
不需要重启。

修改 Web 管理员凭据、trust-proxy 设置、显示/轮换/撤销 HY2 凭据等安全关键
操作需要当前 Web 管理员密码。日常的端口策略修改只需要有效会话和 CSRF
头/Origin，不会再次要求输入密码。从客户端详情页推送或删除映射同样不需要
重新输入密码。密码确认失败按客户端 IP 限制为每分钟五次。所有修改仍然需要
[安全模型](#安全模型) 中描述的 CSRF 头和同源 `Origin`。

### 哪些设置可以在线编辑

**服务端（`hy2tund`）**

| 设置 | 在线编辑 |
| --- | --- |
| `web.username`、`web.password_hash` | 可以。密码只能重置，不能显示。保存会撤销所有 Web 会话。 |
| `auth.password`、`control.token` | 在 `auth.type: password` 时可以。仅限轮换；见下文。 |
| `ports.allow`、`ports.deny`、`ports.auto_range`、`ports.offline_reserve` | 可以。新的允许列表不再覆盖已经运行的映射时，不会关闭它们。`offline_reserve` 至少为 `1s`。 |
| `web.trust_proxy` | 可以。立即应用于登录和设置确认限流。 |
| 在线客户端的映射 | 可以。推送权威列表或删除映射。离线客户端保持只读。 |
| 离线客户端 | 可以。在客户端详情页删除；会移除持久化客户端、映射和软端口预约。在线客户端必须先断开。 |
| `credentials.generation`、`credentials.previous_*` | 由轮换和撤销管理，不能在控制台直接编辑。 |
| `listen`、`listen_host` | 不可以。修改文件并重启。 |
| `tls.*`、`obfs.*`、`bandwidth.*`、`quic.*` | 不可以。修改文件并重启。 |
| `web.listen`、`web.tls.*` | 不可以。修改文件并重启。 |
| `auth.type`、`auth.users` | 不可以。`userpass` 不能在线轮换凭据；修改文件并重启。 |
| `state_file`、`log.*` | 不可以。修改文件并重启。 |

**客户端（`hy2tun`）**

| 设置 | 在线编辑 |
| --- | --- |
| `web.username`、`web.password_hash` | 可以。只能重置密码；保存会撤销所有 Web 会话。 |
| `client.name` | 可以。写入 `client.yaml`，递增 revision，并立即发送到服务端。 |
| `auth`、`control.token` | 可以，只能通过手动凭据同步。新代际必须大于本地代际；客户端会重连。 |
| `mappings` | 可以。添加、更新、删除；服务端也可以推送列表。 |
| `client.revision` | 映射、分配端口或客户端名称变化时自动更新。 |
| `credentials.generation` | 由服务端推送或手动同步管理。 |
| `server`、`transport.*`、`tls.*`、`obfs.*`、`bandwidth.*`、`client.id` | 不可以。修改文件并重启。 |
| `web.listen`、`web.trust_proxy`、`web.tls.*` | 不可以。修改文件并重启。 |
| `proxy.*`、`log.*` | 不可以。修改文件并重启。 |

客户端 `web.trust_proxy` 在当前版本只能通过文件配置；只有服务端控制台可以在
运行时切换 trust-proxy。

### Web 管理员密码

Web 管理员用户名在控制台中显示。密码只保存为 bcrypt 哈希，不能显示；控制台
只能重置。重置需要当前密码、至少 8 个字符的新密码和确认。保存会更新
`web.password_hash`，保持 YAML 文件和在线登录同步，并撤销所有现有 Web
会话，因此需要用新凭据重新登录。修改用户名也有相同的会话效果。

### 凭据代际

对于密码认证，服务端当前 HY2 凭据对是 `auth.password` 加 `control.token`，
客户端当前凭据对是 `auth` 加 `control.token`。`credentials.generation`
标记这对凭据。缺失或 `0` 的代际按 `1` 处理；这保证旧配置文件仍可用。

服务端轮换凭据时，把 `generation` 加一，并把旧的当前凭据对移到
`credentials.previous_generation`、`credentials.previous_auth` 和
`credentials.previous_control_token`。在上一代被撤销或被下一次轮换替换之前，
服务端同时接受当前和上一代 HY2 密码及控制令牌。在服务端，代际只用于状态
报告和关闭过期的控制会话；它不是认证因素。报告上一代代际的具备能力客户端
会显示为 `pending`。

客户端只跟踪 `credentials.generation`。
`previous_generation`、`previous_auth` 和 `previous_control_token` 在
`client.yaml` 中保留但被忽略；客户端从不保留或使用上一代凭据。推送或手动
应用的轮换会替换当前凭据对。服务端的上一代凭据只用于认证，绝不会推送给
客户端。

### 从服务端控制台轮换凭据

1. 打开 **Settings → HY2 凭据**。
2. 输入新的 HY2 密码、新的控制令牌，或两者都输入。留空表示保持当前值。
   HY2 密码至少 8 字节；控制令牌至少 16 字节。
3. 选择同步模式：
   - **自动推送 / auto** 向每个声明 `credential_update_v1` 的在线客户端发送
     `credential_update`，每个客户端最多等待五秒确认，并返回每个客户端的
     结果。
   - **手动同步 / manual** 不发送推送。每个在线客户端保持 `pending`，直到
     从它自己的控制台更新。
4. 用当前 Web 管理员密码确认。

服务端在切换在线凭据之前先把新代际写入 `server.yaml`，因此写入失败会保持
旧凭据对有效。两个字段都留空，或值与当前凭据对相同，会以 `no_change` 拒绝。
允许只轮换一个字段；保留的上一代凭据对就是旧的当前凭据对。

自动推送结果：

- `synced` —— 客户端写入新凭据对并确认；服务端随后关闭旧控制流，让客户端
  重连。
- `pending` —— 客户端尚未确认新代际（手动模式、未就绪，或写入/确认失败）。
- `unsupported` —— 客户端没有声明 `credential_update_v1`；没有向它发送消息，
  必须手动更新。
- `timeout` —— 五秒内没有收到匹配的确认。

自动推送不会自动重试。设置页面轮询 `GET /api/settings`，显示每个在线客户端
的当前状态；离线客户端显示为未知代际。离线客户端不会被推送；仍在上一代
的客户端可以继续工作，直到上一代被撤销或被后续轮换替换。

如果已经存在上一代凭据对，第二次轮换会替换它。代际早于新的上一代代际的
控制会话会立即关闭，因为这些客户端重连后无法再认证；请手动更新它们。

### 客户端手动同步

如果客户端离线、版本旧或没有被自动推送，从它自己的控制台更新：

1. 在服务端 **Settings → HY2 凭据** 记录当前代际（或显示当前凭据对）。
2. 在客户端 **Settings → HY2 凭据** 输入大于本地代际的新代际，以及新的
   HY2 密码和控制令牌。
3. 用客户端当前 Web 管理员密码确认。客户端原子写入 `client.yaml`，关闭
   当前连接，并用新凭据对重连。

客户端页面会把最后一次 `hello_ack` 中的服务端代际显示为提示。值不匹配也
允许提交，但同步徽章可能会保持 `pending`，直到下一次握手报告服务端当前
代际。代际只用于显示；认证仍然取决于密码和令牌是否匹配。

也可以直接编辑 `client.yaml` 中的 `auth`、`control.token` 和
`credentials.generation`，然后重启客户端。

### 撤销上一代凭据

**撤销旧凭据 / Revoke previous credentials** 会从 `server.yaml` 清除
`credentials.previous_generation`、`credentials.previous_auth` 和
`credentials.previous_control_token`，只让当前凭据对有效。服务端会关闭
所有不在当前代际的控制会话。那些客户端会继续用旧凭据对重连并失败，直到
手动更新。

对于标准 Hysteria 2 代理路径，撤销后使用旧密码的新连接会立即失败。已经
认证的连接可以继续，直到自然关闭，因为 Hysteria 是按连接认证的。这是预期
行为。

### 显示凭据

设置页面只显示固定掩码（服务端 `********`，客户端 `••••••••`）；掩码不会
泄露密钥长度。`GET /api/settings` 从不包含明文密码或令牌。

显示明文：

1. 在显示字段中输入当前 Web 管理员密码。
2. 点击 **显示凭据 / Show credentials**。

服务端返回当前凭据对，以及存在时的上一代凭据对；客户端只返回当前凭据对。
显示需要有效 Web 会话和当前密码，响应带 `Cache-Control: no-store`，不会
写入日志或 URL，并且在五次密码确认失败后按客户端 IP 限制为每分钟一次。
页面在 30 秒后自动重新掩码，刷新后不保留明文。

Web 管理员密码不属于此端点，永远不会显示。

### 兼容性与旧客户端

新客户端在控制 `hello` 中发送 `credential_generation` 和
`capabilities: ["credential_update_v1"]`。服务端用能力字段控制自动推送，
并把不具备能力的客户端标记为 `unsupported`；绝不会向它们发送
`credential_update` 消息：

- **旧客户端，新服务端** —— 轮换后，省略 `credential_update_v1` 的客户端
  不会收到 `credential_update` 消息，因此不会因未知消息断开。它会被标记为
  `unsupported`，并继续使用上一代凭据，直到凭据被撤销或被后续轮换替换。
  更新客户端二进制，或手动编辑 `client.yaml` 并重启。
- **旧客户端，缺少代际** —— 没有 `credential_generation` 的 `hello` 按代际
  `1` 处理。如果服务端仍在代际 `1`，客户端显示为 `synced`。
- **新客户端，旧服务端** —— 旧服务端忽略新的 `hello` 字段，其 `hello_ack`
  没有代际。客户端显示服务端代际未知并保持 `pending`；隧道仍然工作，但
  服务端驱动的轮换不可用。手动编辑两个文件。
- **旧配置文件** —— 缺少 `credentials` 段表示代际 `1` 且没有上一代凭据，
  因此现有部署可以原地升级。
- **标准 Hysteria 2 客户端** —— 它们不使用 hy2tun 控制协议，不受影响；只是
  服务端在撤销或后续轮换替换上一代之前，会同时接受当前和上一代 HY2 密码，
  如上所述。

### 写入顺序与失败行为

每次热更新都遵循相同顺序：校验输入、构建新配置、原子写入（临时文件、fsync、
rename 和 `.bak`），然后切换运行时状态。如果校验或文件写入失败，运行配置、
凭据集、端口策略和 Web 登录保持不变。客户端凭据更新会在发送
`credential_ack` 之前写入 `client.yaml`，因此已确认的轮换是持久的。

## 客户端管理

服务端会把离线客户端保留在状态文件中，使其映射和端口预约在重启后仍然存在。
如果要忘记一个不会再回来的客户端，打开它的详情页并点击 **删除客户端**。
这会移除持久化的客户端、它的所有映射和软端口预约，且无法撤销。

按钮只在客户端离线时显示。如果客户端在线，请先停止客户端或其控制流，等它
变为离线后再删除。删除客户端不会撤销它的 HY2 凭据；同一个客户端重新连接后
仍会重新注册。如果它不应该再回来，请轮换凭据或删除它的配置。

等价的 API 是 `DELETE /api/clients/{id}`，需要有效会话、
`X-Requested-With: hy2tun` 和同源 `Origin`。成功返回 `204`；在线客户端返回
`409 client_online`；未知或已删除的客户端返回 `404 client_not_found`。

## 端口跳跃

Hysteria 2 支持 UDP 端口跳跃：客户端把 HY2 连接发送到一组 UDP 端口，并按
固定或随机间隔切换到另一个端口。它主要用于绕过针对单个 UDP 端口的限速或
阻断，不能隐藏服务端、突破对整个 UDP 的封锁，也不能替代 TLS。

端口跳跃只影响客户端到服务端的 HY2 UDP 连接。服务端的公网反向映射端口
（例如 `2222/tcp`、`51820/udp`）不会跳跃，仍使用配置中的固定端口。

### 客户端配置

`server` 支持与 Hysteria 2 相同的端口集合语法，以下写法都有效：

- `hy2tun.example.com:443` — 单端口，不启用跳跃
- `hy2tun.example.com:443,8443` — 两个端口
- `hy2tun.example.com:30000-40000` — 包含端点的范围
- `hy2tun.example.com:443,30000-30100,8443` — 混合列表

在 `transport.udp` 中配置跳跃间隔：

```yaml
server: "hy2tun.example.com:30000-40000"

transport:
  udp:
    hop_interval: "30s"        # 固定间隔，最小 5s

# 或者使用随机间隔；两个字段必须同时出现：
# transport:
#   udp:
#     min_hop_interval: "15s"
#     max_hop_interval: "45s"
```

- 多端口 `server` 未配置间隔时默认 30s。默认值只在运行时使用，不会写回
  `client.yaml`。
- `hop_interval`、`min_hop_interval`、`max_hop_interval` 的最小值都是 5s，
  使用 Go duration 语法（`30s`、`1m`、`1m30s`）。
- `hop_interval` 与 `min_hop_interval`/`max_hop_interval` 互斥；随机间隔
  必须同时配置后两者，且 `min <= max`。
- 单端口 `server` 配置任何间隔字段都会被拒绝。规范化后只剩一个唯一端口的
  集合（例如 `443-443`、`443,443`）按单端口处理。
- `transport` 只能通过配置文件修改，Web 控制台不支持热编辑。修改 `server`
  或 `transport` 后重启客户端。
- Salamander 混淆会包装每个跳跃 socket，obfs 配置无需改变。

启用跳跃时客户端会在启动日志中记录一条 warning：

```text
port hopping enabled; server must listen on the range or redirect it to the HY2 listen port
```

这条 warning 以 `Warn` 级别记录，因此 `log.level: error` 会将其抑制。出现时
属于预期行为：客户端无法验证服务端防火墙规则。

### 服务端监听与重定向

把 `listen` 配置为单端口或范围：

```yaml
listen: ":30000-40000"
```

服务端会绑定首端口（`30000`），并在启动时把其余端口（`30001-40000`）
重定向到它。Linux 上优先使用 nftables，找不到时回退到 iptables/ip6tables。
可以用 `HY2TUN_FIREWALL_BACKEND=nft` 或
`HY2TUN_FIREWALL_BACKEND=iptables` 强制指定后端。服务关闭时会删除规则。
安装规则需要 root 或 `CAP_NET_ADMIN`；安装失败会导致启动失败。

客户端指向同一个范围：

```yaml
server: "hy2tun.example.com:30000-40000"
```

首端口是实际的 HY2 监听端口，其余端口是重定向。单端口
`listen: ":24443"` 不会安装任何重定向；如果仍想使用客户端跳跃，请使用
下面的手动回退方案。

非 Linux 系统请使用单端口监听加外部重定向。

#### 手动重定向回退

如果保留 `listen: ":24443"`，并希望客户端在 `30000-40000` 上跳跃，可以
自行安装重定向。nftables 和 iptables 只选一种。

nftables：

```nft
table inet hy2tun_porthop {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    udp dport 30000-40000 counter redirect to :24443
  }
}
```

把规则保存到文件后用 `nft -f` 加载，或用增量命令创建表、链和规则。

添加：

```bash
nft add table inet hy2tun_porthop
nft add chain inet hy2tun_porthop prerouting \
  '{ type nat hook prerouting priority dstnat; policy accept; }'
nft add rule inet hy2tun_porthop prerouting \
  udp dport 30000-40000 counter redirect to :24443
```

验证：

```bash
nft list table inet hy2tun_porthop
```

删除：

```bash
nft delete table inet hy2tun_porthop
```

iptables 和 ip6tables。上面的 `table inet` nftables 示例用一条规则同时覆盖
IPv4 和 IPv6；iptables 方式需要分别执行两条命令。

添加：

```bash
iptables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
ip6tables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
```

验证：

```bash
iptables -t nat -L PREROUTING -n -v
ip6tables -t nat -L PREROUTING -n -v
```

删除：

```bash
iptables -t nat -D PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
ip6tables -t nat -D PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
```

要求：

- Linux 服务端，且安装了 nftables 或 iptables/ip6tables。
- 具有 root 或 `CAP_NET_ADMIN` 才能安装规则。
- 云厂商安全组必须放行整个 UDP 范围，因为它在实例的 NAT 之前过滤。主机
  防火墙的 INPUT 链只需要放行首个监听端口，因为 `REDIRECT` 在 PREROUTING
  阶段完成。
- 如果客户端可能使用 IPv4 和 IPv6，两个协议都要放行该范围和监听端口。
- 自动重定向是运行时规则，不需要持久化；手动规则必须在重启后仍然生效
  （发行版防火墙配置、`nftables.service`、`iptables-persistent` 或 systemd
  单元）。

### 端口冲突

跳跃范围必须避开服务端可能用于反向映射的每一个 UDP 端口：

- 每一个显式 UDP 映射（`type: udp`，包括 `remote_port: 0` 自动分配的端口）；
- `ports.allow` 中覆盖 UDP 端口的每一条条目。`ports.allow` 非空时它也是
  `remote_port: 0` 自动分配端口的来源，因此整个列表都要避开；
- `ports.auto_range`：`ports.allow` 为空时用于自动分配，默认值为
  `20000-29999`。

使用默认 `auto_range` 时，应使用 `30000-40000`，不要使用 `20000-30000`。
服务端的 TCP 映射端口可以与 UDP 跳跃范围重叠，因为协议相互独立；但分开
更便于审计。如果 UDP 映射落在跳跃范围内，它的流量会被重定向到 HY2 监听
端口，映射将无法工作。

服务端会保留 `listen` 中的所有端口（包括范围），并拒绝使用这些端口的映射。
仍建议让跳跃范围避开 `ports.allow` 和 `ports.auto_range`：自动分配会跳过
保留端口，但明确分开更便于排查和审计。

### 验证重定向

1. 确认规则存在且数据包计数在增长：自动重定向可查看
   `nft list tables | grep hy2tun_porthop_`；手动规则可查看
   `nft list ruleset` 或 `iptables -t nat -L PREROUTING -n -v`。
2. 客户端运行时在服务端查看 conntrack：

   ```bash
   conntrack -L -p udp | grep 'sport=30000'
   ```

   reply 元组的源端口是首个监听端口（示例中的 30000），原始目的端口来自
   跳跃范围。观察多个间隔后，应能看到不止一个原始目的端口。
3. 或者抓取到达的报文：

   ```bash
   tcpdump -ni any 'udp and dst portrange 30000-40000'
   ```

   `tcpdump` 在 NAT 之前抓包，因此目的端口是跳跃端口。
4. 在跳跃期间持续使用一个 TCP 映射和一个 UDP 映射，两者都应保持正常；
   只有 HY2 连接会切换端口。UDP 不会重传，客户端切换 socket 时在途的数据报
   可能丢失，因此 UDP 映射在跳跃瞬间可能出现短暂中断。TCP 流和 HY2 连接会
   自行恢复。
5. 删除重定向规则，确认客户端跳到未重定向端口后最终失败；恢复规则后恢复
   正常。

`udphop` 每个间隔随机选择端口，可能再次选中当前端口，因此某个间隔看不到
变化是正常的。应在多个间隔内确认至少变化一次。

如果跳跃不工作，按以下顺序检查：

- 客户端启动日志中应有端口跳跃 warning。没有 warning 时，可能是 `server`
  仍是单端口，也可能是 `log.level` 或 `log.file` 把这条日志隐藏了；请同时
  检查 `server` 和 `log.level`。
- 云安全组放行整个 UDP 范围，主机防火墙放行监听端口。
- 重定向目标与 `listen` 的首个端口一致，且规则位于 `nat` 表的 PREROUTING 钩子。
- `conntrack` 中 reply 元组的源端口是 `listen` 的首个端口。
- IPv6 客户端没有被只处理 IPv4 的规则遗漏。

### 兼容性

- 单端口客户端的行为与之前完全一致，旧配置文件无需修改。
- 标准 Hysteria 2 客户端在服务端有重定向或范围监听时，也可以使用相同的
  `host:port,端口范围` 语法。hy2tun 的间隔字段是 snake_case；标准
  Hysteria 2 配置使用 `hopInterval`、`minHopInterval`、`maxHopInterval`，
  不要直接复制。
- 标准 Hysteria 2 服务端 + 手动重定向可以与 hy2tun 客户端配合；标准
  Hysteria 2 客户端也可以连接配置了范围监听或手动重定向的 hy2tun 服务端。
- Windows 和 OpenWrt 客户端只发起出站 UDP，因此也可以跳跃；自动服务端
  重定向要求 Linux，手动重定向可以由任意 Linux NAT 主机提供。
- 启用 Salamander obfs 时，每个跳跃 socket 都会被包装，两端必须使用相同
  的 obfs 密码。

## 部署注意

### 防火墙与 NAT

服务端需要允许：

- 从互联网到 `listen` 端口（通常是 443）的 UDP；
- 到每个显式公网映射端口（例如 2222）的 TCP；
- 到每个显式公网 UDP 映射端口（例如 51820）的 UDP；
- 仅从管理网络、VPN 或反向代理到 Web 控制台的 TCP。示例
  `127.0.0.1:7500` 从主机外部不可达。

自动端口（来自 `allow`，或 `allow` 为空时来自 `auto_range`）也需要可达。
可以开放整个范围，或者把映射固定到显式端口并只开放这些端口。客户端只发起
出站连接，因此不需要在路由器上做端口转发。

使用端口跳跃时，还需要在云安全组放行整个 UDP 跳跃范围。可以让服务端通过
范围 `listen` 自动安装重定向，或按 [端口跳跃](#端口跳跃) 手动安装。主机
防火墙的 INPUT 链只需要放行 `listen` 的首个端口，因为重定向在 PREROUTING
阶段完成。

客户端主机需要允许到服务端 HY2 端口的出站 UDP。Windows 可能会为客户端
Web 控制台弹出防火墙提示；只在专用网络允许。

### 安全暴露服务端 Web 控制台

示例服务端控制台绑定 `127.0.0.1:7500`。优先选择以下方式：

1. **从工作站建立 SSH 隧道：**

   ```bash
   ssh -L 7500:127.0.0.1:7500 root@hy2tun.example.com
   # 然后打开 http://127.0.0.1:7500/
   ```

2. **VPN / Tailscale / WireGuard**：把 `web.listen` 绑定到 VPN 地址，或保持
   回环并通过 VPN 访问。

3. **TLS 反向代理**，例如 nginx 或 Caddy。代理必须保留 `Host` 头并设置
   `X-Forwarded-Proto`，因为 CSRF 检查会比较 `Origin` 头与请求主机和协议。
   为 SSE 端点关闭代理缓冲。

   ```nginx
   server {
       listen 443 ssl;
       server_name hy2tun.example.com;

       ssl_certificate     /etc/letsencrypt/live/hy2tun.example.com/fullchain.pem;
       ssl_certificate_key /etc/letsencrypt/live/hy2tun.example.com/privkey.pem;

       location / {
           proxy_pass http://127.0.0.1:7500;
           proxy_set_header Host $host;
           proxy_set_header X-Forwarded-Proto $scheme;
           proxy_set_header X-Forwarded-For $remote_addr;
           proxy_set_header X-Real-IP $remote_addr;
           proxy_buffering off;
           proxy_read_timeout 1h;
       }
   }
   ```

   控制台使用 `X-Forwarded-Proto` 把会话 cookie 标记为 `Secure` 并发送 HSTS，
   因此代理 HTTPS 部署会同时获得两者。只有当代理会用直连客户端地址覆盖
   **两个** `X-Forwarded-For` 和 `X-Real-IP` 头（示例把两者都设为
   `$remote_addr`）时，才设置 `web.trust_proxy: true`。优先覆盖而不是
   `$proxy_add_x_forwarded_for`：覆盖会完全移除客户端提供的条目，而追加式
   代理会让结果依赖代理行为和链长度。受信任时，限流器优先使用 `X-Real-IP`，
   否则使用 `X-Forwarded-For` 的最后一项，因此最近代理的值生效。客户端可以
   直接访问控制台时，保持 `trust_proxy` 为 `false`，否则转发头可被伪造，
   攻击者可以绕过限流。

不要通过明文 HTTP 直接把控制台暴露在公网接口。控制台有自己的登录、限流和
CSRF 防护，但 TLS 保护传输中的密码和会话 cookie。

### 安全模型

- HY2 认证和控制令牌是独立的密钥。泄露 HY2 密码不会让人注册反向映射，
  泄露控制令牌也不会让人使用代理。
- 控制令牌使用常量时间比较。
- 数据流和 UDP 令牌是 128 位随机值，不派生自映射 ID，也从不记录日志。
- 服务端强制执行端口白名单；客户端无法绕过。
- Web 密码保存为 bcrypt 哈希。会话 cookie 使用 HMAC 签名、`HttpOnly` 和
  `SameSite=Lax`；当请求直接是 HTTPS 或通过 TLS 反向代理时，会标记
  `Secure`（并发送 HSTS）；修改类 API 要求
  `X-Requested-With: hy2tun` 和匹配的 `Origin`。
- Web 管理员密码只能重置，任何 API 都不会返回。重置需要当前密码，并撤销
  所有 Web 会话。
- HY2 凭据在 `GET /api/settings` 中掩码显示。显示端点需要当前 Web 管理员
  密码，返回 `Cache-Control: no-store`，从不把明文写入日志或 URL，并对失败
  确认限流。
- 热设置先持久化再生效。校验或文件写入失败会保持运行中的凭据集、端口策略
  和 Web 登录不变。
- 轮换会保持上一代 HY2 凭据对有效，直到显式撤销或后续轮换替换它；撤销会
  关闭不在当前代际的控制会话。已经认证的标准 Hysteria 2 代理连接可以继续，
  直到自然关闭，因为 HY2 是按连接认证的。
- 登录限流默认按直连对端 IP 计数。仅当代理会用直连客户端地址覆盖
  `X-Forwarded-For` 和 `X-Real-IP` 时，才设置 `web.trust_proxy: true`；
  否则忽略转发头，防止客户端伪造新 IP 绕过限制。受信任时优先
  `X-Real-IP`，回退到 `X-Forwarded-For` 的最后一项。
- 配置和状态文件以 `0600` 权限原子 rename 写入。旧内容保留为 `.bak`。
- 密码、令牌和私钥从不记录日志。
- 客户端控制台默认使用 HTTP，除非配置了 `web.tls`。在局域网中，启用
  `web.tls` 或使用可信 VPN；否则局域网攻击者可以嗅探管理会话。

## 故障排查

### `control handshake rejected: unauthorized`

客户端的 `control.token` 与服务端不匹配。把 `server.yaml` 中的确切值复制到
`client.yaml`（至少 16 字节；建议 32 个随机字节）。两个进程都不会打印该
令牌。

### TLS 错误

- `x509: certificate is valid for ...` —— `server` 地址或 `tls.sni` 与证书
  SAN 不匹配。把 `tls.sni` 设为证书中的名称，或为实际使用的地址签发证书。
- `x509: certificate signed by unknown authority` —— 对于自签名服务端，把
  其证书复制到客户端并设置 `tls.ca`；不要在不可信网络使用 `tls.insecure`。
- `server certificate pin mismatch` —— `tls.pin_sha256` 不是当前服务端证书的
  SHA-256。续期证书后重新生成。
- 控制台前面的反向代理必须保留 `Host`；否则修改会以 `403 forbidden` 失败。

### 防火墙或连接问题

- 客户端日志应显示控制握手完成。如果反复记录 `connect HY2`，说明到服务端
  端口的 UDP 被阻断、地址错误，或服务端没有监听。
- 从客户端网络测试 UDP 可达性：

  ```bash
  nc -vzu hy2tun.example.com 443
  ```

  UDP 的“成功”只表示包已发送；检查服务端日志中的 HY2 监听和客户端日志中的
  握手。
- 公网映射端口在服务端打开，不在客户端。如果两个控制台都显示映射为
  `running` 但无法访问，检查服务端的云防火墙/安全组以及服务端前面的 NAT。
- 处于 `failed` 状态的映射会显示稳定的错误码，例如 `port_in_use`、
  `port_not_allowed`、`port_reserved` 或 `no_free_port`。
  `port_not_allowed` 表示请求端口不在 `ports.allow` 中，或匹配了
  `ports.deny`。
- 启用端口跳跃时，如果客户端能在第一个端口连接、但第一次跳跃后停滞，通常
  是服务端重定向缺失或不完整。检查 [端口跳跃](#端口跳跃) 中的
  `conntrack`/`tcpdump` 步骤以及云安全组范围。

### UDP MTU 与大报文

反向 UDP 使用 HY2 数据报。每条 HY2 UDP 消息有固定的 4096 字节内部缓冲区
（包含地址头），因此大于约 4 KB 的载荷会被丢弃。QUIC 还会执行路径 MTU
发现，MTU 较小的网络可能在报文到达前丢弃大数据报。

- WireGuard 默认 1420 字节包通常可以容纳。如果握手正常但大流量停滞，把
  两端的隧道 MTU 调低（例如 1280）。
- 用递增的载荷大小测试并观察丢包：

  ```bash
  iperf3 -c <server-ip> -p <mapped-udp-port> -u -b 100M -t 30 -l 1200
  iperf3 -c <server-ip> -p <mapped-udp-port> -u -b 100M -t 30 -l 1400
  ```

- UDP 有意不重传。丢失的数据报就是丢失；需要可靠性的协议请使用 TCP 映射。

### Web 控制台登录

- 如果没有打印密码，说明 `web.password_hash` 已经设置。使用该密码，或替换
  为新哈希并重启。
- 同一 IP 五次登录失败会触发一分钟锁定。等待后重试，或重启进程（限流器在
  内存中）。在 TLS 反向代理后面，所有用户否则共享代理 IP；当代理会用直连
  客户端地址覆盖 `X-Forwarded-For` 和 `X-Real-IP` 时，设置
  `web.trust_proxy: true`，以便分别限制每个客户端。
- 如果控制台可达但页面重定向到 `/login`，说明会话 cookie 缺失或过期。重新
  登录；重启进程会使会话失效，但不会使持久化签名密钥失效。
- 如果 `web_secret` 存在但不可读或损坏，进程无法启动。删除它并重启以生成
  新密钥；这会使所有现有 Web 会话失效。检查配置目录对进程用户可写。

### 凭据轮换与同步

- 服务端同步表中的 `unsupported` 表示客户端早于 `credential_update_v1`。
  没有向它发送新凭据对，它会继续用上一代凭据保持连接，直到凭据被撤销或被
  后续轮换替换。更新客户端，或编辑 `client.yaml` 并重启。
- 自动推送后的 `pending` 或 `timeout` 表示客户端没有确认新代际（未就绪、
  配置写入/确认失败，或五秒内没有确认）。上一代凭据仍然有效，因此隧道继续
  工作。服务端已经持久化新凭据对：再次提交相同值会返回 `no_change`，没有
  重新推送端点。使用客户端手动同步，或轮换到另一对不同的凭据。第二次轮换
  会替换上一代凭据对，因此更早代际的客户端会立即断开。
- `no_change` 表示两个字段都为空或与当前凭据对相同；没有创建新代际。
- `unsupported_auth_type` 表示服务端使用 `auth.type: userpass`，不能在线
  轮换。编辑 `server.yaml` 并重启。
- 客户端的 `credential generation ... must be greater than current generation`
  表示手动同步表单没有推进代际。输入服务端当前代际（或任何更大的数字）。
- 离线客户端显示为 `pending` 且代际未知，因为状态文件不持久化最后一次凭据
  代际。重新连接以刷新状态。
- 撤销后，旧客户端无法重连。这是预期行为；更新它的 `auth`、
  `control.token` 和 `credentials.generation` 并重启。

### 端口与重启

- 删除映射会立即关闭其公网监听。端口可能短暂处于 `TIME_WAIT`；
  `SO_REUSEADDR` 允许新监听绑定它。
- 客户端断开后，其端口会在 `ports.offline_reserve` 期间保持软预约。其他
  客户端仍然可以显式请求该端口。
- 状态文件保留最后已知的客户端和映射。要忘记单个离线客户端及其预约，请使用
  客户端详情页的 **删除客户端**；只有在需要完全重置时才删除状态文件。

## 使用 iperf3 做性能测试

每次对比都使用相同的服务端、客户端和网络路径，并尽量连续运行测试，使线路
条件可比。

1. 在内网目标上启动 `iperf3`：

   ```bash
   iperf3 -s -p 5201
   ```

2. 映射一个公网 TCP 端口到它，例如 `remote_port: 2222`、
   `local_addr: "192.168.1.10:5201"`。

3. 从服务端侧的主机测量反向映射：

   ```bash
   iperf3 -c <server-public-ip> -p 2222 -t 30
   iperf3 -c <server-public-ip> -p 2222 -t 30 -P 4
   ```

4. 对于 UDP，把 UDP 端口映射到同一目标并测量：

   ```bash
   iperf3 -c <server-public-ip> -p 51820 -u -b 100M -t 60
   iperf3 -c <server-public-ip> -p 51820 -u -b 100M -t 60 -l 1400
   ```

   记录报告中的丢包、抖动和乱序计数。比较 1200、1400 和 4000 字节载荷，
   找出路径可用的 MTU。

5. 与原生 HY2 代理路径对比。在服务端主机运行 `iperf3 -s`，启用客户端的
   SOCKS5 监听（`proxy.socks5: "127.0.0.1:1080"`），从客户端局域网使用支持
   SOCKS 的包装器：

   ```bash
   proxychains4 -q iperf3 -c 127.0.0.1 -p 5201 -t 30
   ```

   或者通过 `proxy.http` 使用支持 HTTP CONNECT 的客户端。

6. 如果同一路径上有 FRP 部署，通过它运行相同的 iperf3 命令并记录结果。

验收是对比性的，不是固定数字：TCP 反向映射吞吐量应接近同一路径上的原生
HY2 代理，100 Mbps 的 UDP 不应出现持续丢包或明显抖动。记录客户端、服务端、
内核、HY2 版本、MTU 和测试时间。

### 调优清单

- 从示例 QUIC 窗口开始（`initial_stream_receive_window` 和
  `max_stream_receive_window` 约 8 MB，`max_connection_receive_window`
  约 20 MB）。只有在路径带宽延迟积很大且两端内存充足时才调高；过大的窗口
  会增加 bufferbloat 和内存占用。
- `quic.max_incoming_streams` 限制并发 TCP 映射和控制流。没有实测需求时
  保持默认值；每个流都会占用内存。
- `bandwidth.up` 和 `bandwidth.down` 是服务端限速。线路速率测试时留空；
  生产环境按公平性需求显式设置。
- `bandwidth.ignore_client` 决定是否信任客户端上报的带宽。对比吞吐量时
  两种设置都测试。
- UDP 反向映射的数据报上限约为 4 KB，更大的载荷会被丢弃。大报文停滞时
  降低隧道 MTU（例如 WireGuard 调到 1280）。
- 数据面改动前后运行 `make bench`。控制帧和端口策略基准用于本机回归；
  端到端吞吐请使用 iperf3。

## 手动验收清单（OpenWrt 和 Windows）

Go 测试只在 Linux 上运行。OpenWrt 和 Windows 真机执行属于外部验收，
**不会**由 CI 执行，测试套件也不声称已完成。在宣布支持某个平台之前，请在
真机上运行此清单，并记录设备、固件/系统版本、架构、二进制版本和日期。

### OpenWrt

1. 安装与 `uname -m` 匹配的归档（`mipsle`/`mips` 是软浮点；`arm` 是
   GOARM=7）。确认 `hy2tun` 无需额外共享库即可启动。
2. 把 `examples/client.yaml` 复制到 `/etc/hy2tun/client.yaml`，设置服务端、
   凭据以及至少一条 TCP 和一条 UDP 映射，然后 `chmod 600`。
3. 安装 `scripts/openwrt/hy2tun.init`，启用并启动服务。确认 `logread` 显示
   控制握手完成且没有崩溃循环。
4. 从局域网登录客户端控制台，添加一条映射，并确认它出现在服务端控制台。
5. 从局域网外部通过映射的 TCP 端口连接，并向映射的 UDP 端口发送数据报；
   确认两者都有响应。
6. 重启服务和路由器。确认客户端重连、映射恢复，配置文件（包括服务端分配
   的端口）保留。
7. 从控制台删除并禁用一条映射。确认公网监听关闭，活跃连接断开。

### Windows

1. 解压 `windows/amd64` 归档，在 PowerShell 中运行
   `hy2tun.exe -c client.yaml`。确认出现防火墙提示，并且只在专用网络允许。
2. 配置一条 TCP 和一条 UDP 映射，然后从客户端网络外部验证两个公网端口。
3. 在 `http://<lan-ip>:7501/` 登录客户端控制台，添加/更新/删除映射，并确认
   YAML 文件发生变化。
4. 用任务计划程序注册客户端（见
   [`scripts/windows/README.md`](scripts/windows/README.md)），重启 Windows，
   确认无需交互登录即可重连。
5. 停止并重启任务。确认映射恢复，配置文件除预期的分配端口/revision 外
   保持不变。
6. 确认密码、控制令牌、UDP 令牌和会话密钥不会出现在控制台日志文件或
   Windows 事件输出中。

## 开发

运行基准测试和 fuzz：

```bash
make bench
go test ./internal/control -run '^$' -fuzz=FuzzReadMessage -fuzztime=30s
go test ./internal/ports -run '^$' -fuzz=FuzzParseRanges -fuzztime=30s
go test ./internal/config -run '^$' -fuzz=FuzzParseListen -fuzztime=30s
```

基准测试覆盖控制帧编解码和端口策略解析/分配；用于本机回归对比，不作为跨机器
性能 SLA。

```bash
go test ./...                 # 单元 + 集成 + 端到端测试
go test -race ./...           # 竞态检测
go test ./internal/e2e -v     # 只运行验收测试
```

端到端测试会用自签名证书启动真实的服务端和客户端，验证反向 TCP 和 UDP，
连接标准 HY2 核心客户端，然后登录服务端控制台推送和删除映射、自动推送轮换
两套凭据、确认客户端持久化新凭据对并重连，以及撤销上一代凭据。它还会确认
使用已撤销凭据的新客户端会被拒绝。测试只使用回环地址和临时端口。

仓库有意不使用 Node/CDN 构建。Web 模板、CSS 和 JavaScript 通过 `go:embed`
嵌入。

## 许可证

MIT，见 [`LICENSE`](LICENSE)。安全报告方式见 [`SECURITY.md`](SECURITY.md)。
