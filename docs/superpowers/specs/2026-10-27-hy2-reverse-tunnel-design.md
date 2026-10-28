# hy2tun 设计文档

- 状态：待批准
- 日期：2026-10-27
- 工作名：`hy2tun`（可改）
- 目标读者：实现这套程序的工程师

## 1. 背景

现有做法是 FRP 做内网穿透、Hysteria 2（下称 HY2）做代理。在甲骨文的服务器上，FRP 的传输效果不理想，而 HY2 的传输效果很好。

用户想要一套程序，同时具备：

- FRP 式的反向端口映射：客户端主动连接公网服务端，把内网 TCP/UDP 服务暴露到服务端端口。
- HY2 式的代理：服务端可以直接作为 HY2 代理服务器使用。
- 客户端与服务端之间的传输使用 HY2 的传输机制，而不是 FRP 自己的传输层。
- 现有 HY2 客户端可以继续连接服务端做代理，不需要修改。
- 服务端和客户端都有 Web 管理界面；服务端 Web 可以统一管理各个客户端。
- 客户端支持 Windows、Linux、OpenWrt。

## 2. 目标与非目标

### 2.1 目标

1. 服务端是一个可用的 HY2 代理服务器，标准 HY2 客户端能直接连接并代理 TCP/UDP。
2. 我们的客户端通过同一条 HY2 连接注册反向映射；映射数据走 HY2 的 QUIC 流与原生 Datagram。
3. 客户端在配置文件中声明映射，也可以在本地 Web 页面增删改映射。
4. 服务端 Web 能查看在线客户端、每个客户端的映射、端口占用和流量，并能修改在线客户端的映射。
5. 客户端配置是唯一事实来源；服务端 Web 的修改会推送给客户端，由客户端立即应用并原子写入本地配置文件。
6. 服务端支持端口白名单：多个单端口和多段端口范围；默认允许任意空闲端口。
7. 支持 TCP 和 UDP 映射，UDP 使用 HY2 原生 Datagram，不把报文塞进可靠流。
8. 交付 Windows、Linux、OpenWrt 可用的客户端二进制。

### 2.2 非目标（v1 不做）

- 多用户注册、审批、租户隔离、计费、配额。
- P2P / Hysteria Realms / UDP 打洞。
- CDN、TUN 模式、透明代理、按域名分流、ACL、负载均衡。
- 一个客户端同时连接多个服务端。
- 服务端 Web 编辑服务端自身的完整配置（改 YAML 后重启）。
- 客户端 Web 编辑服务端地址、TLS、obfs、控制令牌（这些留在配置文件）。
- 离线客户端的配置编辑（v1 只读展示最后已知配置）。
- 自动更新、托盘程序、图形安装器。

### 2.3 反目标（即使能跑也算失败）

- 反向 UDP 实际走可靠流，导致实时 UDP 服务出现队头阻塞或明显抖动。
- 为了反向映射修改 HY2 线协议，导致标准 HY2 客户端无法连接。
- 服务端 Web 的修改覆盖客户端本地修改，造成配置丢失。
- 客户端断开后服务端仍长期保留公网监听端口。
- OpenWrt 上二进制大到无法安装，或需要额外动态库。
- 端口白名单可以被客户端绕过。

## 3. 术语

| 术语 | 含义 |
|---|---|
| 服务端 | 部署在公网服务器上的 `hy2tund`，同时是 HY2 代理服务器和映射入口 |
| 客户端 | 部署在内网的 `hy2tun`，主动连接服务端并申请映射 |
| 映射 | 一条“服务端公网端口 → 客户端内网目标”的转发规则 |
| 控制流 | 客户端主动打开的 HY2 TCP 流，承载注册、配置同步、开流指令 |
| 数据流 | 每条公网 TCP 连接对应的一条 HY2 TCP 流 |
| UDP 会话 | 每条 UDP 映射对应的一条 HY2 UDP 会话 |
| 标准 HY2 客户端 | 官方 Hysteria 2 或其他兼容实现，只做代理，不申请映射 |

## 4. 总体架构

```
标准 HY2 客户端 ────── QUIC / HY2 ──────┐
                                        │
我们的客户端 ───────── QUIC / HY2 ──────┤
  │ 控制流（注册/同步/开流）             │
  │ 数据流（TCP）                        ├── hy2tund（甲骨文 Linux）
  │ 原生 Datagram（UDP）                 │     ├── HY2 代理出口（TCP/UDP）
  │                                     │     ├── 公网 TCP 监听（每条映射）
  ├── 本地 Web（LAN）                    │     ├── 公网 UDP 监听（每条映射）
  ├── 本地 SOCKS5/HTTP（可选）            │     └── 管理 Web + API
  └── 内网服务（TCP/UDP）                │
```

### 4.1 角色与进程

- `hy2tund`：服务端二进制。包含 HY2 服务端核心、映射管理、公网监听、管理 Web。
- `hy2tun`：客户端二进制。包含 HY2 客户端核心、控制流、本地 Web、可选的本地代理。
- 两个二进制来自同一个代码库，共享配置、协议、Web 组件；分开构建以减小 OpenWrt 上的体积。
- 一个服务端可以接多个客户端；一个客户端只连一个服务端。

### 4.2 数据路径

**代理路径（标准 HY2）**

标准 HY2 客户端 → HY2 认证 → 打开 TCP 流或 UDP 会话 → 服务端 `Outbound` 连接目标 → 转发。这条路径完全由官方核心处理。

**反向 TCP 路径**

1. 客户端打开控制流，注册映射。
2. 公网用户连接服务端映射端口。
3. 服务端接受连接，生成一次性 token，通过控制流发 `open`。
4. 客户端收到 `open`，打开数据流 `_hy2tun_data_<token>:0`，同时连接内网目标。
5. 服务端的 `Outbound.TCP` 识别该虚拟地址，取出等待中的公网连接，交给 HY2 核心复制数据。
6. 任一侧关闭，另一侧随之关闭。

**反向 UDP 路径**

1. 客户端为每条 UDP 映射创建一条 HY2 UDP 会话，向虚拟地址 `_hy2tun_udp_<token>:0` 发送一个 1 字节探针。
2. 服务端 `Outbound.UDP` 识别虚拟地址，返回绑定到该映射公网 UDP 端口的 `UDPConn` 包装。
3. 公网报文到达：包装的 `ReadFrom` 返回 `(data, 公网对端地址)`；HY2 核心把公网对端地址放进 `UDPMessage.Addr` 发给客户端。
4. 客户端 `Receive()` 得到 `(data, 公网对端地址)`；按对端地址维护一个连接内网目标的本地 UDP socket，写入数据。
5. 内网目标回复：客户端从对应 socket 读出，调用 `Send(reply, 公网对端地址)`。
6. 服务端会话把 `UDPMessage.Addr` 作为目标，调用包装的 `WriteTo`，发回公网对端。
7. 客户端每 20 秒向虚拟地址发一个 1 字节保活，服务端 `WriteTo` 识别虚拟地址后丢弃，只刷新会话活跃时间。

这条路径完全使用 HY2 的 `UDPMessage` / QUIC Datagram，不需要改 HY2 线协议。依据见 17 节。

### 4.3 与标准 HY2 的关系

- 服务端使用官方 `core/server`，认证、TLS、QUIC、拥塞控制、TCP/UDP 代理行为与官方一致。
- 客户端使用官方 `core/client`，所以它对服务端就是一个标准 HY2 客户端。
- 反向映射只使用官方公开的 `Outbound`、`Client.TCP`、`Client.UDP` 扩展点，不 fork 核心，不改线协议。
- 标准 HY2 客户端不会打开 `_hy2tun_*` 虚拟地址，因此不受影响。

## 5. 关键设计决策

### 5.1 复用官方 HY2 核心

依赖：`github.com/apernet/hysteria/core/v2 v2.13.0`（commit `ca8fbd8`，MIT），以及 `github.com/apernet/hysteria/extras/v2` 的 obfs/auth。

- 服务端：`server.NewServer` + 自定义 `Outbound` + `Authenticator`。
- 客户端：`client.NewClient` + `TCP()` / `UDP()`。
- 拥塞控制、Brutal、BBR、Salamander、Gecko 由官方核心提供。
- 不复制官方源码到本仓库；通过 Go module 引用，锁定版本。

Go 版本：核心 v2.13.0 要求 Go 1.26。仓库 `go.mod` 写 `go 1.26.0`，依赖 Go 工具链自动下载（`GOTOOLCHAIN=auto`）。构建机需要能访问 Go module 代理。

### 5.2 反向 TCP

- 控制流是一条普通 HY2 TCP 流，虚拟地址 `_hy2tun_control_:0`。
- 服务端 `Outbound.TCP` 看到该地址，返回 `net.Pipe` 的一端；另一端交给控制处理器。
- 公网连接到达时，服务端生成 128 位随机 token，存入 `pending[token] = conn`，带 10 秒过期；通过控制流发 `open`。
- 客户端用 `_hy2tun_data_<token>:0` 打开数据流。服务端 `Outbound.TCP` 查表、删除、返回公网 `net.Conn`。
- token 一次性、不可猜、带过期，避免其他已认证的 HY2 用户猜测虚拟地址劫持公网连接。
- 服务端不主动打开 QUIC 流（HY2 的流由客户端发起），因此不需要服务端发起流的扩展。

### 5.3 反向 UDP

- 每条 UDP 映射一个 `client.UDP()` 会话，虚拟地址带每条映射独立的 128 位 token。
- token 在注册成功时由服务端生成，通过 `hello_ack` / `update_ack` 返回；客户端用它构造虚拟地址。
- 服务端 `Outbound.UDP` 用 token 查映射；查不到就返回错误，不暴露任何公网端口。
- 公网 UDP socket 每个映射一个，`ReadFrom` / `WriteTo` 包装成官方 `server.UDPConn`。
- 客户端按“公网对端地址”维护本地 UDP socket；每个 socket `DialUDP` 到内网目标，保证回复能归属到正确的公网对端。
- 客户端 `HyUDPConn.Send` 不是线程安全的（共用 `SendBuf`），所有发送必须串行化。
- 保活：每 20 秒发 1 字节到虚拟地址；服务端丢弃但刷新会话。HY2 服务端 UDP 空闲超时默认 60 秒。
- 已知限制：单个 UDP 报文超过 HY2 分片能力时会被丢弃；WireGuard 这类大包需要实测 MTU。

### 5.4 控制通道

- 客户端主动连接，服务端复用现有 QUIC 连接，不额外开端口。
- 控制流是长连接；断开即视为客户端离线，服务端关闭该客户端的所有公网监听和待处理连接。
- 控制协议使用 4 字节大端长度前缀 + JSON，单帧上限 1 MiB，协议版本 1。
- 控制流上的认证使用独立的 `control.token`，不复用 HY2 密码。这样即使把 HY2 代理密码分享给别人，对方也不能申请映射。
- v1 的信任模型是：**所有持有 control token 的客户端完全可信**。`client_id` 由客户端自报，服务端不做“一个客户端不能冒充另一个”的强绑定；token 泄露等同于映射管理权泄露。因此 token 只放在自己的客户端上，不随代理密码分享；泄露后轮换服务端和所有客户端的 token。
- 控制流地址 `_hy2tun_control_:0` 本身不是秘密；没有 token 会在握手阶段被拒绝。

### 5.5 配置权威与同步

- **客户端本地配置文件是唯一事实来源。**
- 客户端配置有单调递增的 `revision`，每次本地修改或成功应用服务端推送都加一。
- 服务端保存每个客户端“最后已知”的 `revision` 和映射列表，仅用于展示和冲突检测。
- 服务端 Web 修改在线客户端时，发送 `push`，带 `base_revision`。
- 客户端只有在 `base_revision == 当前 revision` 时才应用；否则返回 `conflict`，服务端 Web 提示“客户端配置已变化，请刷新后重试”。
- 应用成功后，客户端先原子写配置文件，再回复 `push_ack`；写盘失败则不应用，返回错误。
- 客户端本地 Web 修改：先写配置文件、`revision++`，再向服务端发 `update`；服务端绑定失败时，映射保留在配置中并标记失败原因。
- v1 不允许编辑离线客户端；离线时服务端 Web 只读展示最后已知配置。

原子写：同目录临时文件 → `fsync` → `rename` → 保留上一版 `.bak`。文件权限 0600。

#### 5.5.1 revision 的精确规则

- 客户端 revision 是权威值。客户端 `hello` 带来多少，服务端就接受多少；即使比服务端记录的低（用户恢复了旧配置），也以客户端为准，服务端重置缓存。
- 客户端本地修改：先 `revision++`，再原子写盘，再发 `update`。写盘失败则回滚内存和 revision。
- 服务端收到 `update`：只要 `revision > lastKnown` 就接受并更新 `lastKnown`；小于等于时返回 `stale_revision`，不改变任何映射。
- 服务端 `push`：`base_revision` 必须等于服务端当前 `lastKnown`（也就是客户端当前 revision）。客户端应用成功后把 revision 设为 `push.revision`，**不再额外加一**，然后写盘并 `push_ack`。
- `hello_ack` 里如果服务端分配了端口（客户端请求 0），客户端把实际端口写回配置，`revision++`，然后立即发一次 `update`。服务端按“revision 更大”接受。
- `update_ack` 里如果服务端分配了端口，客户端只把端口写回配置，**不再加 revision**；这次分配属于同一个 update。
- UDP token 不写入配置文件；每次注册由服务端重新生成。客户端发现 token 变化就重建该映射的 UDP 会话。
- 同一个客户端的控制消息在服务端串行处理（每客户端一个队列/互斥），避免 Web push 和客户端 update 交叉导致 base_revision 错乱。

### 5.6 端口策略

- 客户端为每条映射指定 `remote_port`；`0` 表示由服务端自动分配。
- 服务端配置 `ports.allow` 和 `ports.deny`：
  - `allow` 为空：允许任意空闲端口。
  - `allow` 非空：只允许列表内的端口；`0` 自动分配也只在允许范围内挑。
  - `deny` 永远优先。
  - 服务端自动保留自己的 HY2 监听端口、Web 端口；保守起见 TCP/UDP 都保留。
- 语法：YAML 字符串列表，每项是单端口 `"2222"` 或闭区间 `"8000-8100"`；端口范围 1–65535，起点不大于终点。
- 自动分配范围：
  - `allow` 非空时，从 `allow` 内挑。
  - `allow` 为空时，从 `ports.auto_range` 挑，默认 `"20000-29999"`，避免落到系统保留端口或 Linux 临时端口段。
  - 自动分配跳过 `deny`、服务端保留端口、正在监听的端口、未过期的离线预留端口。
- 离线端口预留：
  - 客户端断开后，它最后使用的端口保留 `ports.offline_reserve`，默认 `24h`，用于让重连的客户端优先拿回原端口。
  - 离线预留是软预留：自动分配会跳过；如果另一个客户端显式申请该端口，允许占用并取消预留。
  - 预留过期后自动释放；服务端 Web 在客户端详情里显示“端口预留至 …”。
- 冲突检查按协议分别进行：TCP 2222 和 UDP 2222 可以同时存在。
- 自动分配成功后，客户端把实际端口写回配置；具体 revision 规则见 5.5.1。
- 绑定失败返回稳定错误码，例如 `port_in_use`、`port_not_allowed`、`port_reserved`、`no_free_port`。
- 删除映射、`enabled: false`、或修改类型/端口/内网目标时，关闭该映射上所有已建立的公网连接；只改备注不关闭连接。服务端和客户端都按映射跟踪活跃连接。

### 5.7 Web 管理

- 服务端 Web 和客户端 Web 都是内嵌在二进制里的服务端渲染页面 + 少量 JS；不依赖 CDN、不要求 Node 构建。
- 服务端 Web 默认监听 `127.0.0.1:7500`，避免默认暴露到公网；需要远程管理时改配置或放在反向代理/VPN 后面。
- 客户端 Web 默认监听 `0.0.0.0:7501`，因为局域网管理是明确需求；必须登录。
- 单管理员账号：`web.username` + bcrypt `web.password_hash`。首次启动 hash 为空时生成随机密码，打印一次到日志，写入配置。
- 会话：HMAC 签名的 HttpOnly Cookie，`SameSite=Lax`，默认 7 天；签名密钥首次启动生成并保存在状态文件。
- 登录限速：每 IP 每分钟 5 次失败后短暂锁定。
- 修改类 API 要求 `X-Requested-With: hy2tun` 且校验 `Origin`；防止 CSRF。
- 实时数据用 SSE（`/api/events`），不用 WebSocket，方便过反向代理。
- 可选 `web.tls.cert/key`；不配置就是 HTTP。公网暴露时必须由用户加 TLS 反向代理或只绑 VPN 地址。

服务端页面（已与用户确认方向）：

- 顶部一级导航：概览 / 客户端 / 全部映射 / 设置。
- 客户端页：左侧客户端列表，右侧选中客户端详情；映射表 + 编辑抽屉。
- 概览：在线客户端数、活跃映射数、总上行/下行、服务端监听信息。
- 全部映射：跨客户端的平铺表，支持按类型、状态、端口筛选。
- 设置：只读配置摘要 + 配置文件路径；不做在线编辑。

客户端页面：

- 顶部：客户端名、连接状态、服务端地址、版本。
- 统计卡：运行时长、活跃映射、上行、下行。
- Tab：端口映射 / 日志 / 设置。
- 映射表列：类型、服务器:端口、内网目标、备注、状态、流量。
- 新增/编辑抽屉：类型（TCP/UDP）、服务器（当前连接，只读）、服务端端口（0 = 自动）、内网目标、备注。
- 设置页只读展示服务端地址、TLS、obfs、控制令牌（打码）、配置文件路径；修改这些需要改文件并重启。

响应式：宽度小于 900px 时，主从布局折叠为“列表 → 详情”，详情页有返回按钮。

## 6. 控制协议

### 6.1 帧格式

```
uint32 长度 N（大端）
N 字节 JSON
```

- 单帧上限 1 MiB；超过直接断开。
- 协议版本字段 `protocol: 1`；版本不匹配返回 `unsupported_protocol` 并断开。
- 所有消息都带 `type`。
- 未知字段忽略；未知 `type` 返回 `bad_message`。

### 6.2 消息

**hello（客户端 → 服务端）**

```json
{
  "type": "hello",
  "protocol": 1,
  "token": "control-token",
  "client_id": "c-8f3a...",
  "name": "home-router",
  "version": "0.1.0",
  "revision": 7,
  "mappings": [
    {
      "id": "m-1a2b",
      "type": "tcp",
      "remote_port": 2222,
      "local_addr": "192.168.1.10:22",
      "note": "NAS SSH",
      "enabled": true
    }
  ]
}
```

**hello_ack（服务端 → 客户端）**

```json
{
  "type": "hello_ack",
  "ok": true,
  "server_version": "0.1.0",
  "results": [
    {
      "id": "m-1a2b",
      "ok": true,
      "assigned_port": 2222,
      "udp_token": "",
      "error": ""
    }
  ]
}
```

- UDP 映射成功时 `udp_token` 为 128 位随机十六进制；TCP 映射为空。
- 认证失败：`{"type":"hello_ack","ok":false,"error":"unauthorized"}`，随后关闭控制流。
- 单条映射失败不影响其他映射；失败项带稳定错误码。

**update（客户端 → 服务端）**

客户端本地修改后发送完整映射列表：

```json
{"type":"update","revision":8,"mappings":[...]}
```

**update_ack（服务端 → 客户端）**

```json
{"type":"update_ack","revision":8,"results":[...]}
```

**push（服务端 → 客户端）**

```json
{"type":"push","base_revision":8,"revision":9,"mappings":[...]}
```

**push_ack（客户端 → 服务端）**

```json
{"type":"push_ack","ok":true,"revision":9,"results":[...]}
```

冲突：`{"type":"push_ack","ok":false,"error":"conflict","revision":8}`。

**open（服务端 → 客户端）**

```json
{"type":"open","token":"a1b2...","mapping_id":"m-1a2b"}
```

**open_result（客户端 → 服务端，可选）**

```json
{"type":"open_result","token":"a1b2...","ok":false,"error":"dial_timeout"}
```

用于服务端日志和 Web 展示；不影响数据面。

**ping / pong**

```json
{"type":"ping","ts":1793076000}
{"type":"pong","ts":1793076000}
```

每 15 秒一次；30 秒无 pong 视为控制流失效。

### 6.3 状态机

客户端：

```
启动 → 读取配置 → 连接 HY2 → 打开控制流 → hello
  → hello_ack 成功 → 为每条 UDP 映射创建会话并保活
  → 进入运行：处理 open / push / ping
  → 控制流断开 → 关闭本地代理/会话 → 指数退避重连（1s..30s）
```

服务端：

```
接受 HY2 连接 → 认证（HY2 密码）
  → 若客户端打开控制流：校验 control token → 注册客户端
  → 为映射绑定公网端口 → 返回结果
  → 运行：接受公网连接、转发、处理 update/push_ack
  → 控制流断开 → 关闭该客户端所有监听、pending、活跃数据连接和 UDP socket
```

### 6.4 错误码

| 错误码 | 含义 |
|---|---|
| `unauthorized` | control token 错误 |
| `unsupported_protocol` | 协议版本不匹配 |
| `bad_message` | JSON 或字段非法 |
| `port_not_allowed` | 端口不在白名单 |
| `port_reserved` | 端口被服务端保留 |
| `port_in_use` | 端口已被占用 |
| `no_free_port` | 自动分配范围内没有空闲端口 |
| `invalid_target` | 内网目标地址非法 |
| `conflict` | 配置 revision 冲突 |
| `stale_revision` | 客户端 update 的 revision 不大于服务端已记录值 |
| `internal` | 服务端内部错误 |

## 7. 服务端设计

### 7.1 启动流程

1. 读取并校验 YAML。
2. 初始化状态文件、Web 会话密钥。
3. 构造 HY2 `server.Config`：
   - `Conn`：UDP socket；如果配置了 Salamander，先用 `obfs.WrapPacketConnSalamander` 包装。
   - `TLSConfig`：证书/私钥；支持 `GetCertificate` 以便未来热加载。
   - `Authenticator`：password 或 userpass。
   - `Outbound`：自定义路由器（见 7.2）。
   - `QUICConfig`、`BandwidthConfig`、`CongestionConfig`。
   - `DisableUDP: false`（反向 UDP 依赖它）。
   - `EventLogger`：连接/断开/请求日志，写入服务端日志。
4. 启动管理 Web。
5. `Serve()` 阻塞运行。

### 7.2 Outbound 路由

`Outbound.TCP(reqAddr)`：

- `_hy2tun_control_:0` → 创建 `net.Pipe`，把一端交给控制处理器，返回另一端。
- `_hy2tun_data_<token>:0` → 查 `pending[token]`；存在则删除并返回公网 `net.Conn`；不存在返回错误。
- 其他地址 → 默认直连（标准 HY2 代理行为）。

`Outbound.UDP(reqAddr)`：

- `_hy2tun_udp_<token>:0` → 查 UDP 映射 token；返回该映射公网 UDP socket 的包装；不存在返回错误。
- 其他地址 → 默认 UDP 直连。

`Outbound.CheckUDP(reqAddr)`：

- 虚拟地址返回 nil；其他地址按默认策略（v1 允许全部）。

### 7.3 映射管理

每个客户端一个 `clientSession`：

- `clientID`、`name`、`revision`、`mappings`、`controlConn`、`pending`、`listeners`。
- 注册时对比新旧映射：
  - 删除的映射：关闭监听，清理 pending。
  - 新增的映射：校验端口、绑定、生成 UDP token。
  - 修改的映射：
    - 端口或类型变化：按删除 + 新增处理，重建监听。
    - 仅内网目标变化：保留监听和 UDP token，更新元数据，关闭该映射的活跃连接。
    - 仅备注变化：保留监听和活跃连接。
  - 比较端口时使用服务端实际绑定端口：`remote_port: 0` 刚被分配为 P 后，客户端回写的 `remote_port: P` 视为未变化，不重建监听，也不更换 UDP token。
  - 未变化的映射：保留监听和 UDP token。
- 客户端断开：关闭所有监听、所有 pending 公网连接、所有 UDP 会话（关闭 socket 让 HY2 会话自然退出）。
- 同一 `client_id` 重连：替换旧 session；旧 session 先关闭，避免端口冲突。

#### 7.3.1 服务端推送的事务顺序

服务端 Web 修改在线客户端时，按以下顺序执行，避免客户端写入了服务端却绑不上的配置：

1. 校验请求体、revision、端口白名单。
2. **预绑定**所有新增或变更的端口；任何一条失败则整体拒绝，关闭刚绑定的端口，返回 400/409 和每条映射的错误。不改客户端配置，不发 push。
3. 计算 `revision = base_revision + 1`，发 `push`，等待 `push_ack`，超时 10 秒。
4. 收到成功 ack：提交。关闭被删除映射的监听和活跃连接，更新 `state.json`，返回 200。
5. 收到冲突 ack：关闭预绑定端口，返回 409，提示客户端配置已变化。
6. 超时或连接断开：关闭预绑定端口，保持旧配置；客户端可能已经应用并写盘，重连时 `hello` 会重新注册，服务端重新绑定。这是可恢复的不一致，不静默丢弃客户端配置。
7. 只改备注的映射不重新绑定；只改 `enabled` 的映射按新增/删除处理。

**部分失败不提交**：一条映射失败就整体拒绝，避免出现“客户端以为成功、服务端只生效一半”的状态。

#### 7.3.2 控制流断开后的连接处理

- 控制流断开即视为客户端离线。
- 服务端关闭该客户端的所有公网监听、所有 pending 公网连接、所有已建立的映射数据连接、所有 UDP socket。
- 服务端无法直接关闭整条 QUIC 连接（`Outbound` 不暴露连接句柄），但关闭公网连接会让对应数据流自然结束；客户端侧也会主动关闭自己的数据流和本地连接。
- 客户端控制流断开后，关闭所有映射数据连接和 UDP 会话，销毁旧 HY2 client，按 1s→2s→4s…最多 30s 退避重连，重新 `hello`。
- 已有公网连接会断，这是明确行为，不做“无损迁移”。

### 7.4 公网监听

TCP：

```go
ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, strconv.Itoa(port)))
```

- `bindHost` 默认空（所有地址）；可选配置 `listen_host` 只绑指定地址。
- Accept 后设置 `TCP_NODELAY`，生成 token，登记 pending，发 `open`。
- 发送失败或 10 秒内没有数据流认领，关闭公网连接。
- 每个公网连接单独计数上行/下行字节。

UDP：

```go
conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
```

- 每个映射一个 socket；包装成 `server.UDPConn`。
- 包装的 `ReadFrom` 返回公网对端字符串；`WriteTo` 解析地址并发送。
- `WriteTo` 收到虚拟地址时直接丢弃并返回成功（保活探针）。
- 关闭映射时关闭 socket。

### 7.5 Web 与 API

服务端 Web 使用 Go `html/template` + 内嵌静态资源；实时数据用 SSE。

API：

| 方法 | 路径 | 用途 |
|---|---|---|
| POST | `/api/login` | 登录 |
| POST | `/api/logout` | 登出 |
| GET | `/api/overview` | 服务端状态、统计 |
| GET | `/api/clients` | 客户端列表 |
| GET | `/api/clients/{id}` | 客户端详情 + 映射 |
| PUT | `/api/clients/{id}/mappings` | 推送映射配置（仅在线） |
| GET | `/api/mappings` | 全部映射 |
| GET | `/api/events` | SSE：在线状态、流量 |
| GET | `/api/config/summary` | 只读配置摘要 |

`PUT /api/clients/{id}/mappings` 请求体：

```json
{"base_revision":8,"mappings":[...]}
```

服务端校验端口和格式后，`revision = base_revision + 1`，发 `push`，等待 `push_ack`（10 秒超时）。成功返回新 revision；冲突返回 409。

### 7.6 持久化

- `state.json`：`clients: {id: {name, version, revision, mappings, last_seen, online}}`。
- 只保存最后已知状态，不保存密码、token、流量。
- 变更后 debounce 1 秒原子写入。
- 服务端重启后，在线客户端会重连并刷新；离线客户端从 `state.json` 展示。
- 流量计数只在内存中，重启清零。

## 8. 客户端设计

### 8.1 启动与重连

1. 读取配置；缺失 `client.id` 时生成 UUID 并写回。
2. 校验映射：ID 唯一、类型合法、端口范围、目标地址可解析（域名允许延迟解析）。
3. 构造 HY2 `client.Config`：
   - `ConnFactory`：UDP socket；Salamander 用 `obfs.WrapPacketConnSalamander`。
   - `ServerAddr`、`Auth`、`TLSConfig`（SNI、insecure、pinSHA256、CA）。
   - `BandwidthConfig`、`QUICConfig`、`CongestionConfig`。
4. `client.NewClient`，打开控制流，发 `hello`。
5. `hello_ack` 成功后：
   - TCP 映射：只记录状态，等待 `open`。
   - UDP 映射：`client.UDP()` 创建会话，向虚拟地址发探针，启动保活和接收循环。
6. 控制流断开：关闭所有映射数据连接、UDP 会话和本地代理连接，指数退避重连；TCP 公网连接会断，这是可接受的。
7. 进程退出：关闭控制流、HY2 客户端、Web。

### 8.2 本地 Web

- 与 5.7 一致；编辑映射时：
  1. 校验表单。
  2. 更新内存配置，`revision++`。
  3. 原子写配置文件；失败则回滚内存并提示。
  4. 如果控制流在线，发 `update`；否则标记“等待服务端”，重连后由 `hello` 带上。
  5. 根据 `update_ack` 更新每条映射的状态和 `assigned_port`。
- 删除映射：关闭对应 UDP 会话；TCP 无需本地监听。
- 修改 `remote_port`：视为删除 + 新增；服务端会重建监听。
- 日志页展示最近 500 行内存日志（环形缓冲），支持级别过滤；不落盘，除非配置了 `log.file`。

### 8.3 映射执行

TCP：

- 收到 `open`：用 10 秒超时 `net.Dial("tcp", local_addr)`。
- 成功：`client.TCP("_hy2tun_data_<token>:0")`；把本地连接和返回的 `net.Conn` 双向复制。
- 失败：不打开数据流，发 `open_result` 错误；服务端关闭公网连接。
- 复制使用两个 goroutine；任一侧结束就关闭两侧。半关闭语义不保证。

UDP：

- 每条映射一个 `HyUDPConn`。
- 接收循环：`Receive()` → 取 `(data, peerAddr)` → 找到/创建该 peer 的本地 UDP socket → `Write`。
- 本地 socket 的读循环：读回复 → `Send(reply, peerAddr)`；发送加互斥锁。
- peer 空闲 60 秒关闭 socket 并删除。
- 保活 ticker 每 20 秒 `Send([]byte{0}, virtualAddr)`。
- 映射删除或客户端退出：关闭所有本地 socket 和 `HyUDPConn`。

### 8.4 代理模式（可选）

- 配置 `proxy.socks5` / `proxy.http` 非空时启用。
- SOCKS5：支持无认证和用户名密码；支持 CONNECT；UDP ASSOCIATE 在 v1 不要求。
- HTTP：支持 CONNECT；普通 HTTP 转发可作为 should-have。
- 代理连接通过 `client.TCP(addr)` / `client.UDP()` 走 HY2。
- 代理监听默认只绑 `127.0.0.1`，避免变成开放代理。

## 9. 配置

### 9.1 服务端 `server.yaml`

```yaml
listen: ":443"
listen_host: ""           # 可选：映射公网监听绑定的地址，空 = 所有地址

auth:
  type: password          # password | userpass
  password: "change-me"
  # users:
  #   alice: "pass1"

tls:
  cert: "/etc/hy2tun/cert.pem"
  key: "/etc/hy2tun/key.pem"

obfs:
  type: salamander        # 空 = 不启用
  salamander:
    password: "obfs-pass"

bandwidth:
  up: "100 mbps"
  down: "100 mbps"
  ignore_client: false

quic:
  initial_stream_receive_window: "8 mb"
  max_stream_receive_window: "8 mb"
  initial_connection_receive_window: "20 mb"
  max_connection_receive_window: "20 mb"
  max_idle_timeout: "30s"
  max_incoming_streams: 4096

control:
  token: "replace-with-32-byte-random-hex"

ports:
  allow: ["2222", "8000-8100", "51820"]
  deny: []
  auto_range: "20000-29999"   # allow 为空时自动分配使用的范围
  offline_reserve: "24h"      # 客户端离线后保留最后端口的时长

web:
  listen: "127.0.0.1:7500"
  username: "admin"
  password_hash: ""        # 空 = 首次启动生成并打印
  tls:
    cert: ""
    key: ""

state_file: "/var/lib/hy2tun/state.json"

log:
  level: "info"
  file: ""
```

### 9.2 客户端 `client.yaml`

```yaml
server: "sg.example.com:443"

auth: "change-me"          # 或 "user:pass"

tls:
  sni: "sg.example.com"
  insecure: false
  pin_sha256: ""
  ca: ""

obfs:
  type: salamander
  salamander:
    password: "obfs-pass"

bandwidth:
  up: "50 mbps"
  down: "200 mbps"

control:
  token: "same-as-server"

client:
  id: ""                   # 空 = 首次启动生成
  name: "home-router"
  revision: 1

mappings:
  - id: "m-1a2b"
    type: tcp
    remote_port: 2222
    local_addr: "192.168.1.10:22"
    note: "NAS SSH"
    enabled: true
  - id: "m-3c4d"
    type: udp
    remote_port: 51820
    local_addr: "192.168.1.10:51820"
    note: "WireGuard"
    enabled: true

web:
  listen: "0.0.0.0:7501"
  username: "admin"
  password_hash: ""
  tls:
    cert: ""
    key: ""

proxy:
  socks5: ""               # 例如 127.0.0.1:1080
  socks5_auth: ""          # 可选，"user:pass"
  http: ""                 # 例如 127.0.0.1:8080
  http_auth: ""            # 可选，"user:pass"

log:
  level: "info"
  file: ""
```

### 9.3 校验规则

- `listen`、`server`、`web.listen` 必须是合法 `host:port`；`listen` / `web.listen` 允许端口 0（测试用临时端口），`server` 端口必须是 1–65535。
- `auth.type` 只能是 `password` 或 `userpass`；`userpass` 至少一个用户。
- TLS 证书和私钥必须同时提供；客户端 `insecure` 与 `pin_sha256` 可以同时使用，pin 优先。
- `control.token` 至少 16 字节；建议 32 字节随机值。服务端和客户端必须一致。
- 映射 ID 在同一客户端内唯一；缺失时生成。
- `type` 只能是 `tcp` / `udp`；`remote_port` 0–65535；`local_addr` 必须能 `SplitHostPort`。
- `enabled: false` 的映射不注册，但保留在配置和 Web 列表中。
- 端口范围解析失败、起止颠倒、越界都拒绝启动并给出具体字段。

## 10. 数据模型

```go
type Mapping struct {
    ID         string `json:"id" yaml:"id"`
    Type       string `json:"type" yaml:"type"`               // tcp | udp
    RemotePort int    `json:"remote_port" yaml:"remote_port"` // 0 = auto
    LocalAddr  string `json:"local_addr" yaml:"local_addr"`
    Note       string `json:"note" yaml:"note"`
    Enabled    bool   `json:"enabled" yaml:"enabled"`
}

type ClientState struct {
    ID        string    `json:"id"`
    Name      string    `json:"name"`
    Version   string    `json:"version"`
    Revision  int64     `json:"revision"`
    Online    bool      `json:"online"`
    LastSeen  time.Time `json:"last_seen"`
    Mappings  []Mapping `json:"mappings"`
    // ReservedUntil keeps the client's last ports available for auto-allocation
    // for a grace period after it goes offline. Zero means no reservation.
    ReservedUntil time.Time `json:"reserved_until,omitempty"`
    // v1 不记录客户端公网 IP：官方 Outbound 回调不暴露 QUIC 远端地址，
    // 不为了展示 IP 引入 goroutine-local hack。需要时再通过 EventLogger 扩展。
}
```

运行时补充字段（不落盘）：

```go
type MappingRuntime struct {
    Status       string // pending | running | failed | disabled
    Error        string
    AssignedPort int
    UDPToken     string
    TxBytes      uint64
    RxBytes      uint64
}
```

## 11. 安全

- HY2 密码和控制 token 分离；控制 token 泄露不等于代理密码泄露，反之亦然。
- 控制 token 用 `subtle.ConstantTimeCompare` 比较。
- 数据流 token、UDP token 都是 128 位随机值，一次性或按映射生命周期有效；不可从映射 ID 推导。
- 服务端只允许已认证且持有 control token 的连接注册映射。
- 端口白名单在服务端强制执行，客户端无法绕过；`deny` 优先。
- 服务端默认只监听 `127.0.0.1:7500`；客户端 Web 监听 `0.0.0.0` 但必须登录。
- Web 密码只存 bcrypt hash；Cookie HttpOnly + SameSite=Lax；登录限速；修改类 API 校验 Origin 和自定义头。
- 配置文件权限 0600；日志不打印密码、token、私钥。
- 公网 UDP 监听只转发给已注册映射对应的客户端，不提供开放中继。
- 客户端本地 Web 走 HTTP 时，局域网内可能被嗅探；文档明确建议启用 `web.tls` 或只在可信 LAN/VPN 使用。

## 12. 平台与交付

### 12.1 构建

- `CGO_ENABLED=0`，静态链接。
- `-trimpath -ldflags "-s -w"`。
- Go 1.26 工具链；`go.mod` 锁定核心 v2.13.0。
- 目标矩阵：
  - `linux/amd64`
  - `linux/arm64`
  - `linux/arm`（GOARM=7）
  - `linux/mipsle`（GOMIPS=softfloat，OpenWrt 常见）
  - `linux/mips`（GOMIPS=softfloat，按需）
  - `windows/amd64`
  - `windows/arm64`（按需）
- 每个平台产出 `hy2tund` / `hy2tun` 两个二进制，tar.gz/zip + SHA256SUMS。

### 12.2 运行方式

- Linux 服务端/客户端：systemd unit 示例，`Restart=always`，`LimitNOFILE=1048576`。
- OpenWrt：procd init 脚本，`respawn`，配置放 `/etc/hy2tun/`，状态放 `/var/run/hy2tun/` 或 `/etc/hy2tun/`（视 flash 可写性）。
- Windows：提供 README 说明用任务计划程序开机启动；是否实现原生 Windows 服务由实现者决定（见 16 节）。
- 升级：替换二进制 + 重启；配置文件向后兼容，未知字段忽略。

### 12.3 二进制体积

- 目标：客户端 stripped 后尽量小于 20 MB；OpenWrt 设备若 flash 不足，使用 extroot 或只部署客户端。
- 不在 v1 引入 UPX（可能触发杀软误报）。
- 服务端不要求体积优化。

## 13. 测试与验收

### 13.1 单元测试

- 配置解析与校验：端口范围、白名单、TLS 组合、token 长度、映射 ID 唯一。
- 端口分配：allow/deny/保留端口/自动分配/冲突。
- 离线端口预留：24h 内自动分配跳过；显式申请可占用并取消预留；过期释放。
- revision 冲突：push 的 base_revision 不匹配时拒绝。
- revision 规则：hello 采用客户端的更低 revision；`update` 的 revision 不大于 lastKnown 时返回 `stale_revision`；push 成功后 revision 精确等于 `push.revision`；hello_ack 分配端口后的 update 被接受。
- 推送事务：任一映射预绑定失败时整体拒绝，已预绑定端口全部释放。
- 删除/禁用/修改映射时关闭该映射的活跃连接；只改备注不关闭。
- 控制协议编解码：长度前缀、超大帧、未知字段、未知 type。
- 原子写配置：写失败回滚、`.bak` 生成。
- 虚拟地址解析：control/data/udp token 的合法与非法输入。

### 13.2 集成测试（Go，回环）

用自签名证书启动真实 `hy2tund` 和 `hy2tun`，使用临时端口：

1. TCP echo：公网端口 → 客户端本地 echo 服务，双向数据一致。
2. UDP echo：公网 UDP → 客户端本地 UDP echo，多个公网源端口并发，回复归属正确。
3. 标准 HY2 客户端：用官方 core client 连接服务端，TCP/UDP 代理可用；确认它不能注册映射。
4. 端口白名单：不在 allow 内的端口被拒绝；deny 优先；自动分配落在 allow 内。
5. 端口冲突：两个客户端申请同一端口，第二个失败且不影响第一个。
6. revision 冲突：客户端本地修改后，服务端用旧 base_revision push，客户端拒绝。
7. 断线重连：杀掉控制流，客户端重连后映射恢复；旧监听已释放。
8. 映射删除：删除后公网端口立即关闭。
9. 客户端 Web：登录、增删改映射、写盘、重启后配置保留。
10. 服务端 Web：登录、查看客户端、在线修改映射、客户端文件更新。
11. 推送事务：一条新映射端口被占用时，整次 push 被拒绝，旧映射继续工作。
12. 删除映射：已建立的公网 TCP 连接被关闭，监听端口释放。
13. 离线预留：客户端断开后 24h 内自动分配跳过其端口；显式申请可占用。

### 13.3 构建验证

- 上述目标矩阵全部 `go build` 成功。
- Linux amd64 上跑集成测试。
- OpenWrt 二进制在至少一台真实设备上启动、登录 Web、建立一条 TCP 和一条 UDP 映射。

### 13.4 手工性能验收（甲骨文线路）

在客户端内网起 `iperf3 -s`，分别测试：

- TCP 映射 vs 原生 HY2 代理 vs FRP（同一路径、同一时间段）。
- UDP 映射（`iperf3 -u -b 100M`）60 秒，记录丢包和抖动。
- 大包 UDP（例如 1400 字节）验证分片/MTU 行为。

验收标准：

- TCP 映射吞吐应接近原生 HY2 代理（在测量噪声范围内），并明显优于同路径 FRP。
- UDP 映射在 100 Mbps 下不应出现持续丢包或明显抖动；具体阈值由用户在真实线路判断。
- 不承诺固定数字；以用户实际线路的对比结果为准。

## 14. 风险与缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| HY2 核心 API 变化 | 编译失败/行为变化 | 锁定 v2.13.0；升级前跑集成测试 |
| 反向 UDP 大包分片 | 丢包、抖动 | 文档说明 MTU；实测 WireGuard；必要时限制单包大小 |
| 客户端 UDP socket 数量 | 大量公网对端时资源上涨 | 每 peer 空闲 60 秒回收；设置上限和日志 |
| OpenWrt flash 不足 | 无法安装 | 提供 stripped 二进制；文档建议 extroot；按设备架构构建 |
| 服务端 Web 暴露 | 管理面被攻击 | 默认 127.0.0.1；登录限速；建议 TLS/VPN |
| 客户端 Web 被局域网嗅探 | 密码泄露 | 支持 web.tls；文档警告 |
| 配置同步冲突 | 用户修改丢失 | revision + base_revision；冲突时拒绝并提示 |
| TCP 半关闭语义 | 少数协议异常 | 文档说明；v1 不保证 |
| 控制流断开导致公网连接断 | 短暂中断 | 客户端快速重连；公网端口重新绑定；已有连接不保证恢复 |

## 15. 我替你先定的（可推翻）

这些不是用户明确同意的，是实现前可以改的默认选择：

1. 语言用 Go；复用官方 HY2 核心 v2.13.0。
2. 交付两个二进制 `hy2tund` / `hy2tun`，而不是一个二进制带两个子命令；目的是减小 OpenWrt 体积。
3. Web 用 Go 模板 + 内嵌静态资源 + SSE；不引入 Node 构建链。
4. 服务端 Web 默认 `127.0.0.1:7500`；客户端 Web 默认 `0.0.0.0:7501`。
5. Web 首次启动自动生成管理员密码并打印一次；不做“无密码拒绝启动”。
6. v1 只允许在线编辑客户端配置；离线客户端只读。
7. v1 的客户端代理模式只保证 SOCKS5 CONNECT 和 HTTP CONNECT；SOCKS5 UDP ASSOCIATE 不作为验收项。
8. 服务端设置页只读；改配置后重启。
9. 流量计数只在内存，重启清零。
10. 控制协议用长度前缀 JSON，不用 protobuf/msgpack。
11. 信任模型：所有持有 control token 的客户端完全可信，`client_id` 自报，不做 per-client 身份绑定。
12. 离线客户端端口软预留 24 小时；自动分配跳过，显式申请可占用。
13. 删除、禁用或修改映射会关闭该映射的活跃连接；只改备注不关闭。
14. v1 不展示客户端公网 IP。

## 16. 留给实现者的

- 包结构、接口拆分、函数命名。
- 前端具体实现（模板 + htmx/原生 JS 均可），只要满足：内嵌、无 CDN、无 Node 构建、响应式、可访问。
- 日志格式、CLI 参数、错误文案。
- Windows 开机启动方式：任务计划程序或原生服务包装。
- 是否在 v1 加 SOCKS5 UDP ASSOCIATE。
- 是否为服务端 Web 增加只读的 HY2 代理流量统计（`TrafficLogger`）。
- 构建脚本和 CI 的具体形态。
- 配置字段的兼容策略细节（未知字段忽略即可）。

## 17. 附录：源码依据

以下结论来自对官方仓库固定 commit 的阅读，尚未编译或运行验证：

- 核心版本与 Go 要求：`core/go.mod`，commit `ca8fbd874fd6ca82413948de6dc5924405e103e1`，`go 1.26.0`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/core/go.mod
- 客户端公开 `TCP(addr)` / `UDP()` 接口：`core/client/client.go`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/core/client/client.go
- 服务端可替换 `Outbound`（TCP/UDP）与 `UDPConn` 接口：`core/server/config.go`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/core/server/config.go
- UDP 会话在首包时调用 `Outbound.UDP(reqAddr)`，`ReadFrom` 的源地址会作为 `UDPMessage.Addr` 回传客户端：`core/server/udp.go`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/core/server/udp.go
- 客户端 `HyUDPConn.Receive` 返回 `(data, addr)`，`Send(data, addr)` 使用同一地址标签：`core/client/udp.go`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/core/client/udp.go
- `UDPMessage` 线格式：`core/internal/protocol/proxy.go`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/core/internal/protocol/proxy.go
- Salamander 包装：`extras/obfs/salamander.go`、`extras/obfs/conn.go`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/extras/obfs/salamander.go
- 认证器：`extras/auth/password.go`、`extras/auth/userpass.go`。
  https://github.com/HyNetworks/hysteria/blob/ca8fbd874fd6ca82413948de6dc5924405e103e1/extras/auth/password.go
- 类似反向隧道的先例（用于对照，不作为依赖）：HY2 SCALE 用官方核心 + 控制流 + 数据流实现反向 TCP；其反向 UDP 走长度分帧可靠流，本设计改为原生 Datagram。
  https://github.com/FrankoonG/hy2scale
