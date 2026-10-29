# hy2tun 多服务端与按服务端代理设计

- 状态：待批准
- 日期：2026-10-29
- 目标版本：v1.4.0
- 目标读者：实现该功能的工程师

## 1. 目标

一个 `hy2tun` 客户端进程同时连接多个服务端，并且每个服务端完全独立：

- 每个服务端有自己的地址、HY2 凭据、control token、TLS、obfs、带宽、QUIC/传输和端口跳跃配置。
- 每个服务端有自己的 `client.id`、名称、revision、映射列表和凭据代际。
- 每个服务端可以配置自己的 SOCKS5/HTTP 本地代理监听地址。
- 代理默认关闭；默认只启用反向端口映射。
- 客户端 Web 控制台可以按服务端查看状态、管理映射、轮换凭据，并开关/修改该服务端的代理监听地址。
- 一个服务端断线、重连、轮换或配置更新不影响其他服务端。
- 现有单服务端 `client.yaml` 保持兼容，升级后无需手工迁移。

## 2. 非目标

- 不做服务端之间的映射共享、继承或同步。
- 不做多服务端负载均衡、自动故障转移或同一映射的跨服务端漂移。
- 不做全局共享代理；代理属于具体服务端。
- 不改服务端 `hy2tund` 的多租户或账户模型。
- 不把代理认证字段（`socks5_auth`、`http_auth`）放进 Web 热编辑；第一版只热编辑开关和监听地址。
- 不实现服务端控制台选择/切换客户端所属服务端；每个服务端仍然只看到连接它的那个客户端身份。
- 第一版不做按服务端拆分的日志页；保留进程级日志，服务端详情页不展示独立日志流。

## 3. 配置模型

### 3.1 多服务端配置

```yaml
web:
  listen: "0.0.0.0:7501"
  username: "admin"
  password_hash: "..."

log:
  level: "info"

servers:
  - name: "us"
    server: "us.example.com:24443"
    auth: "hy2-password"
    tls:
      ca: "/etc/hy2tun/us-ca.pem"
      sni: "us.example.com"
    obfs:
      type: "salamander"
      salamander:
        password: "obfs-password"
    bandwidth:
      up: "100 mbps"
      down: "200 mbps"
      ignore_client: false
    transport:
      udp:
        hop_interval: "30s"
    control:
      token: "0123456789abcdef0123456789abcdef"
    client:
      id: ""                    # 空则由客户端生成并持久化
      name: "home-us"
      revision: 0
    credentials:
      generation: 1
    mappings:
      - id: "ssh"
        type: "tcp"
        remote_port: 25022
        local_addr: "127.0.0.1:22"
        enabled: true
    proxy:
      enabled: false
      socks5: "127.0.0.1:1080"
      http: "127.0.0.1:8080"
      socks5_auth: ""
      http_auth: ""

  - name: "sg"
    server: "sg.example.com:24443"
    auth: "another-password"
    tls:
      ca: "/etc/hy2tun/sg-ca.pem"
    control:
      token: "abcdef0123456789abcdef0123456789"
    client:
      id: ""
      name: "home-sg"
      revision: 0
    mappings:
      - id: "ssh"
        type: "tcp"
        remote_port: 35022
        local_addr: "127.0.0.1:22"
        enabled: true
      - id: "dns"
        type: "udp"
        remote_port: 0
        local_addr: "127.0.0.1:53"
        enabled: true
    proxy:
      enabled: true
      socks5: "127.0.0.1:1081"
      http: "127.0.0.1:8081"
```

### 3.2 字段规则

- `servers` 非空时，顶层只允许 `web`、`log` 和运行期元数据；旧的单服务端字段
  （`server`、`auth`、`tls`、`obfs`、`bandwidth`、`transport`、`control`、
  `client`、`credentials`、`mappings`、`proxy`）必须为空，否则校验失败。
- `servers` 为空时，顶层旧字段按原语义解析为一个名为 `default` 的服务端；
  旧配置文件不改写也能启动。
- `servers[].name` 必填，唯一，只允许字母、数字、`.`、`_`、`-`，长度 1-64。
  它用于 Web 路由、日志和 API，不作为服务端身份。
- `servers[].server`、`auth`、`control.token` 必填，沿用现有校验。
- `servers[].client.id` 为空时，客户端首次启动生成并持久化一个独立 ID；每个
  服务端有独立 ID，不共享。
- `servers[].client.name` 为空时默认使用 `servers[].name`。
- `servers[].mappings`、`credentials`、`transport`、`proxy` 都只作用于该服务端。
- `proxy.enabled` 默认 `false`。`false` 时不启动该服务端代理，地址字段保留但
  不监听。
- `proxy.enabled: true` 时，`socks5` 和 `http` 至少有一个非空；地址必须是
  明确的 `host:port`，端口必须在 1-65535，不允许 `:0`。
- `proxy.socks5_auth` / `proxy.http_auth` 仍是文件配置，第一版 Web 不编辑。
- `web`、`log` 是客户端进程级配置，不按服务端拆分。

### 3.3 启动与绑定失败

- 配置层错误（缺少必填字段、`servers` 与旧字段混用、启用代理但没有地址、
  明确的重复地址）导致整个客户端启动失败。
- 运行时端口绑定失败（端口被其他进程占用、通配地址与具体地址冲突）只影响
  该服务端：该服务端的 SOCKS5/HTTP 代理整体保持关闭，记录错误并在 Web
  显示；该服务端的端口映射继续运行，其他服务端不受影响。
- 客户端 Web 监听失败仍是进程级错误，整个客户端启动失败。
- 代理监听不会出现半开启状态：同一服务端的 SOCKS5 和 HTTP 要么都绑定成功，
  要么都关闭。

### 3.4 旧配置兼容

旧配置：

```yaml
server: "us.example.com:24443"
auth: "..."
control: { token: "..." }
client: { id: "c1", name: "home", revision: 3 }
mappings: [...]
proxy:
  socks5: "127.0.0.1:1080"
```

内部转换为一个 `name: "default"` 的服务端。旧配置中 `proxy.socks5` 或
`proxy.http` 非空且没有 `enabled` 字段时，为兼容旧行为视为启用；新的
`servers[].proxy` 默认关闭，必须显式 `enabled: true`。

## 4. 运行时架构

### 4.1 Supervisor

新增一个客户端 supervisor，拥有：

- 一份完整 `ClientConfig`
- 一个共享 Web 控制台
- 一个共享日志器
- 一个代理监听管理器
- `map[string]*serverRuntime`，每个服务端一个独立运行时

每个 `serverRuntime` 负责：

- HY2 连接/重连
- control 握手、注册、心跳
- 该服务端的映射监听和 TCP/UDP 数据面
- 该服务端的凭据代际、轮换、同步
- 该服务端的端口跳跃
- 该服务端的代理监听（通过共享代理管理器绑定）

现有单服务端 `Client` 中的连接、控制、映射和代理逻辑应抽取为
`serverRuntime`，不要复制一份协议实现。Supervisor 负责配置持久化、Web API
聚合和跨服务端约束。

### 4.2 生命周期

1. 加载并校验完整配置；生成缺失的每服务端 `client.id`。
2. 启动共享 Web 控制台。
3. 为每个服务端创建 runtime 并异步连接；连接失败只记录该服务端状态并重试。
4. 任一服务端注册/推送/轮换只更新对应 runtime 和配置块。
5. 关闭时先停止所有 runtime，再关闭代理监听和 Web。

一个服务端配置错误导致整个进程启动失败；运行时连接失败不阻止其他服务端
启动。

## 5. 代理监听管理

### 5.1 独立性

代理属于 `servers[].proxy`。不同服务端可以：

- 都关闭代理
- 一个开启、另一个关闭
- 都开启，但监听地址必须不同

关闭一个服务端的代理不影响其他服务端的代理或端口映射。

### 5.2 端口冲突

以下监听地址必须互不冲突：

- 所有已启用服务端的 `proxy.socks5`
- 所有已启用服务端的 `proxy.http`
- 客户端 `web.listen`
- 同一服务端内 SOCKS5 与 HTTP

校验分两层：

1. 配置层：规范化地址并检测完全相同的 `host:port`；同一服务端
   `socks5 == http` 直接拒绝。
2. 应用层：对新增地址执行实际 `net.Listen` 预绑定；通配地址
   （`0.0.0.0`、`::`）与具体地址的冲突由操作系统绑定结果判定。

端口 `0` 不允许用于代理监听，避免 Web 无法展示稳定地址。

### 5.3 热应用事务

Web 修改代理配置时：

1. 校验新地址、启用状态和端口冲突。
2. 对新增监听地址执行预绑定；失败则关闭新监听，保持旧监听和旧配置。
3. 保留地址未变化的监听及活动连接。
4. 原子保存配置；保存失败则关闭新监听，保持旧监听和旧配置。
5. 交换运行时状态，关闭被移除的监听及其活动连接。
6. 返回该服务端的最新代理状态。

关闭代理只关闭该服务端的代理监听和活动连接，不影响端口映射。

### 5.4 代理与断线

服务端断线时，该服务端的本地代理监听可以继续存在，但请求会失败，直到重连。
代理开关本身不依赖服务端在线状态。

## 6. Web 控制台

### 6.1 页面

客户端 Web 新增服务端选择/分组：

- 服务端列表：名称、地址、在线状态、映射数、代理开关状态
- 服务端详情：连接状态、版本、revision、映射、凭据、代理设置；日志使用进程级日志页
- 旧单服务端配置仍显示为一个 `default` 服务端

每个服务端的代理卡片包含：

- 启用开关
- SOCKS5 监听地址
- HTTP 监听地址
- 保存按钮
- 当前绑定状态/错误

代理开关和地址修改不需要重启客户端；保存后立即应用。

### 6.2 API

新增按服务端作用域的 API：

```text
GET  /api/servers
GET  /api/servers/{name}/status
GET  /api/servers/{name}/mappings
GET  /api/servers/{name}/settings
PUT  /api/servers/{name}/proxy
```

映射、凭据和名称操作使用同一作用域。旧单服务端 API 继续映射到 `default`，
保证旧脚本可用。

`PUT /api/servers/{name}/proxy` 请求：

```json
{
  "enabled": true,
  "socks5": "127.0.0.1:1080",
  "http": "127.0.0.1:8080"
}
```

所有修改需要有效会话、`X-Requested-With: hy2tun` 和同源 `Origin`。代理开关和
监听地址属于日常网络设置，不要求重复输入 Web 密码；Web 管理员密码和 HY2
凭据操作仍沿用现有密码确认规则。

旧单服务端 API 的行为：

- 如果配置由旧单服务端字段转换而来，旧 API 映射到 `default`。
- 如果配置使用 `servers` 且不存在名为 `default` 的服务端，旧的服务端作用域
  API 返回 `404`，不会猜测“第一个服务端”。
- `/api/settings/web`、`/api/logout`、`/metrics`、`/healthz` 等进程级端点
  保持进程级语义，不按服务端拆分。

## 7. 配置持久化

- 服务端推送映射时，只替换该服务端配置块，保留其他服务端、Web、日志和
  运行期字段。
- 客户端 Web 修改代理、名称或映射时，写入完整多服务端配置文件的对应块。
- 写入使用现有原子保存（临时文件、fsync、rename、`.bak`）。
- 每个服务端的 `client.revision` 独立；服务端只看到自己的 revision。
- 旧配置第一次发生写操作时，仍保持顶层旧格式，除非用户显式迁移到
  `servers`；不得因为一次代理开关把旧配置意外改写成多服务端格式。
- 旧配置缺少 `client.id` 或 `web.password_hash` 时，首次启动可以按现有行为
  将生成值写回旧格式；这不是格式迁移，不改变旧字段结构。

## 8. 安全

- 代理默认关闭，避免升级后意外开放本地 SOCKS5/HTTP。
- 只绑定配置的地址；默认示例使用 `127.0.0.1`。
- Web 代理修改需要会话和 CSRF；不需要当前密码，但不得允许未认证请求。
- 端口冲突和绑定失败不能留下半开的监听或半写入配置。
- 关闭代理/修改地址时主动关闭对应活动连接，避免旧连接继续使用旧入口。
- 日志记录服务端名称、代理状态变化和错误，不记录代理认证密码。

## 9. 测试

### 配置

- 多服务端解析、唯一名称、每服务端独立字段。
- 旧单服务端配置转换为 `default`。
- 顶层旧字段与 `servers` 混用被拒绝。
- 代理默认关闭；启用但无地址被拒绝；端口 0 被拒绝。
- 旧代理有地址但无 `enabled` 时按旧行为启用。

### 运行时

- 两个服务端同时连接，各自映射和 revision 独立。
- 一个服务端断开/重连不影响另一个。
- 服务端推送只更新目标配置块。
- 每服务端独立 client ID/凭据代际。

### 代理

- 两个服务端同端口冲突被拒绝。
- 代理端口与 Web 端口冲突被拒绝。
- 启动时代理端口被占用：该服务端代理关闭且映射继续，其他服务端不受影响。
- 开启/关闭/修改地址热应用。
- 新地址绑定失败时旧监听保持。
- 配置保存失败时旧监听保持。
- 未变化地址保留活动连接；移除地址关闭连接。
- 代理默认关闭，端口映射仍工作。

### Web

- 服务端列表和详情。
- 代理开关/地址保存、错误提示、CSRF。
- 旧 API 兼容 `default`；多服务端配置没有 `default` 时返回 404。
- 真实 HTTP + 配置文件集成测试。

### 端到端

- 两个测试服务端，一个共享内网服务但各自不同公网端口。
- 两个服务端各自独有映射。
- 只开启其中一个服务端的代理。
- 关闭一个服务端，另一个映射继续工作。

## 10. 兼容与发布

- 目标版本 `v1.4.0`，单服务端配置和现有 API 保持兼容。
- 升级后首次启动不自动改写旧 YAML。
- 文档需要新增多服务端配置示例、迁移说明、代理默认关闭说明和端口冲突规则。
- 服务端 `hy2tund` 不需要协议变更；每个服务端仍看到独立的客户端身份。

## 11. 实现者决定

以下细节由实现者决定，不改变产品行为：

- 路由/文件名和内部类型命名。
- 服务端选择器的具体布局。
- 代理监听的内部注册表结构。
- 配置迁移是否提供显式 CLI；第一版不强制提供。
- 代理开关是否需要额外确认对话框；错误提示必须清晰。
