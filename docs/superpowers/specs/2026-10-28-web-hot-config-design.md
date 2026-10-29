# hy2tun Web 热配置编辑设计

- 状态：待批准
- 日期：2026-10-28
- 适用版本：v1.1
- 目标读者：实现这套功能的工程师

## 1. 背景

v1 的服务端 Web 和客户端 Web 都只能管理映射；设置页是只读的。用户需要 SSH 到服务器或本机改 `server.yaml` / `client.yaml`，体验很差。

用户希望：

- 服务端 Web 能改服务端自己的配置。
- 客户端 Web 能改客户端自己的配置。
- 包括 Web 管理员用户名/密码、HY2 认证密码、control token。
- HY2 密码和 control token 支持两种同步方式：
  - 服务端自动推送给在线客户端。
  - 客户端自己在 Web 上手动同步。
- 旧密码/token 一直有效，直到用户手动点“撤销旧凭据”。
- 网页允许回显旧密码/token。

v1.1 先做**不需要重启进程就能生效**的字段；需要重启的字段（listen、TLS、obfs、QUIC、Web 监听、日志、state_file）继续留在配置文件，后续版本再做。

## 2. 目标与非目标

### 2.1 目标

1. 服务端 Web 可改：
   - Web 管理员用户名、密码（密码只能重置，不能回显）。
   - HY2 认证密码（`auth.type: password`）。
   - control token。
   - `ports.allow`、`ports.deny`、`ports.auto_range`、`ports.offline_reserve`。
   - `web.trust_proxy`。
   - 在线客户端映射（已有）。
2. 客户端 Web 可改：
   - Web 管理员用户名、密码（密码只能重置）。
   - HY2 认证密码。
   - control token。
   - 客户端名称。
   - 映射（已有）。
3. 凭据轮换：
   - 服务端保存 current / previous 两代凭据。
   - 新旧两代同时有效，直到手动撤销。
   - 自动推送：在线客户端收到新值，写配置并重连。
   - 手动同步：客户端 Web 自己填新值。
   - 服务端 Web 显示每个客户端的同步状态。
4. 服务端 Web 可回显 current/previous 的 HY2 密码和 control token；客户端 Web 可回显自己的 current 值。
5. 所有改动热生效，不重启 `hy2tund` / `hy2tun` 进程。
6. 配置写入保持原子（temp → fsync → rename → `.bak`）和 0600。

### 2.2 非目标（v1.1 不做）

- `listen`、`listen_host`、TLS 证书/私钥、obfs、QUIC 窗口、Web 监听地址/TLS、日志、state_file 的网页编辑。
- `auth.type: userpass` 的用户列表编辑（v1.1 只支持 `password`）。
- 客户端 `server` 地址、TLS、obfs、bandwidth、本地 SOCKS5/HTTP 监听地址的网页编辑。
- 多用户、权限分级、审计日志持久化。
- Web 管理员密码回显（只存 bcrypt 哈希，无法回显）。

### 2.3 反目标

- 改配置把自己锁在 Web 外面。
- 旧凭据在“撤销”后仍然能建立新连接。
- 密码/token 出现在结构化日志、URL、错误信息或 `.bak` 以外的可读位置。
- 配置写入一半导致服务端/客户端启动失败。
- 自动推送失败后客户端配置处于半更新状态。

## 3. 凭据代际模型

### 3.1 数据结构

服务端配置新增：

```yaml
credentials:
  generation: 2
  previous_generation: 1
  previous_auth: "old-hy2-password"
  previous_control_token: "old-control-token"
```

- `auth.password` 和 `control.token` 表示 current。
- `credentials.generation` 表示 current 代际；缺失或 0 视为 1。
- `previous_*` 为空表示没有旧代际。
- previous 只用于接受旧客户端；不会自动推送。

客户端配置新增：

```yaml
credentials:
  generation: 2
```

- `auth` 和 `control.token` 表示 current。
- `credentials.generation` 表示客户端当前使用的代际；缺失或 0 视为 1。
- 客户端不保存 previous。

### 3.2 接受规则

服务端 HY2 认证器同时接受：

- `auth.password`（current）
- `credentials.previous_auth`（previous，若非空）

服务端 control token 校验同时接受：

- `control.token`（current）
- `credentials.previous_control_token`（previous，若非空）

客户端 `hello` 增加 `credential_generation`。服务端记录 session 的代际：

- 等于 current generation：状态 `synced`。
- 等于 previous generation：状态 `pending`。
- 其他：状态 `unknown`，仍可连接但 Web 显示异常。

代际只用于状态展示和撤销时的会话清理，不作为认证依据；认证仍由密码/token 是否匹配决定。

### 3.3 轮换流程

服务端 Web 修改 HY2 密码或 control token 时：

1. 要求当前 Web 管理员密码确认。
2. 校验新值：HY2 密码至少 8 字节；control token 至少 16 字节。
3. 生成 `newGeneration = currentGeneration + 1`。
4. 把 current 写入 previous，把新值写入 current。
5. 先写配置文件（原子），成功后再切换运行时凭据。
6. 根据用户选择：
   - **自动推送**：向所有在线客户端发送 `credential_update`。
   - **手动同步**：不发推送，服务端 Web 显示新 generation 和“待同步”客户端列表。
7. 新旧两代继续有效，直到用户点“撤销旧凭据”。

客户端收到 `credential_update`：

1. 校验 generation 大于本地 generation。
2. 原子写入 `auth`、`control.token`、`credentials.generation`。
3. 回 `credential_ack`。
4. 关闭当前控制流和 HY2 客户端，用新凭据重连。
5. 重连后的 `hello` 带新 generation。

手动同步：

1. 用户在服务端 Web 看到 new generation 和 current/previous 凭据（可回显）。
2. 在客户端 Web 填 generation、HY2 密码、control token。
3. 客户端原子写配置，关闭当前控制流/HY2，用新值重连。
4. 服务端 `hello` 收到新 generation，标记 `synced`。

撤销旧凭据：

1. 要求当前 Web 管理员密码确认。
2. 清空 `previous_generation`、`previous_auth`、`previous_control_token`。
3. 原子写配置。
4. 切换运行时凭据，只接受 current。
5. 主动关闭所有 generation != current 的控制会话；这些客户端会尝试用旧凭据重连并失败，直到手动更新。
6. 标准 HY2 代理客户端：旧密码的新连接立即失败；已经建立的代理连接可能继续到自然关闭，这是 Hysteria 认证的连接级特性，文档要说明。

### 3.4 离线客户端

- 自动推送只覆盖在线客户端。
- 离线客户端保留旧凭据，Web 显示“待同步”。
- 旧凭据一直有效，直到撤销；所以离线客户端在撤销前仍可用旧值上线。
- 撤销后，旧客户端无法连接；用户必须在客户端 Web 手动改新值。

### 3.5 连续轮换

如果 previous 仍存在时再次修改凭据：

- 允许，但会丢弃更早的 previous。
- Web 必须显示警告：仍在使用更早代际的客户端将立即失去连接。
- 新 previous 是上一次的 current。
- 轮换时主动关闭 generation 既不是新 current、也不是新 previous 的控制会话；这些客户端只能手动更新。

### 3.6 单字段轮换

HY2 密码和 control token 可以只改一个，也可以一起改：

- 表单中留空表示“保持当前值”。
- 两个都留空 → 400，不产生新代际。
- 新值与当前值相同 → 400 `no_change`。
- 只校验实际要改的字段：HY2 密码至少 8 字节，control token 至少 16 字节；未改字段不因历史长度被重新校验。
- previous 快照是“旧的 current 对”：
  - 只改密码：current=(newPassword, token)，previous=(oldPassword, token)。
  - 只改 token：current=(password, newToken)，previous=(password, oldToken)。
  - 两个都改：previous=(oldPassword, oldToken)。
- 客户端只要 password/token 任一匹配 previous 对，就能通过认证；generation 决定它显示为 pending。

### 3.7 自动推送的返回与超时

`PUT /api/settings/credentials` 带 `sync_mode: auto` 时：

- 只向在 `hello` 中声明 `credential_update_v1` 能力的在线客户端发送 `credential_update`。
- 每个客户端最多等待 5 秒的 `credential_ack`；请求最多阻塞 5 秒，不等待离线客户端。
- 返回每个在线客户端的结果：`synced`、`pending`、`unsupported`、`timeout`。
- 超时/失败的客户端保持 pending，旧凭据继续有效；不自动重试，页面轮询 `/api/settings` 查看状态。
- 未声明能力的旧客户端标记 `unsupported`，不发送推送，必须手动同步。

`sync_mode: manual` 时不发送推送，直接返回新 generation 和 pending 客户端列表。

### 3.8 手动同步的 generation 校验

客户端 Web 手动同步时：

- 必须填正整数 generation，且 `generation > 本地 generation`；否则 400。
- 如果最近一次 `hello_ack` 有服务端 generation，页面提示用户填这个值；不一致时给出警告，但允许提交（例如客户端当前离线、只能先写配置）。
- 服务端以 `hello` 里的 generation 做同步状态展示，不用它做认证；填错只会显示 pending/unknown，不会绕过密码/token 校验。
- 服务端 Web 的凭据页显示新 generation，供手动同步复制。

## 4. Web 管理员账号

### 4.1 服务端 Web

- 用户名可回显；密码只显示“已设置”，不能回显。
- 修改需要当前密码确认。
- 新密码至少 8 字符；bcrypt 哈希写入 `web.password_hash`。
- 修改成功后：
  - 更新 Web 登录使用的用户名/哈希。
  - 撤销所有现有 Web 会话。
  - 返回 204，浏览器跳转登录页。
- 用户名不能为空；修改用户名同样撤销所有会话。

### 4.2 客户端 Web

规则与服务端 Web 相同，作用于客户端自己的 `web.username` / `web.password_hash`。

### 4.3 会话管理

`internal/webui/shared` 的 SessionManager 增加：

- `UpdateAdmin(username, passwordHash string)`
- `RevokeAll()`

`UpdateAdmin` 只更新登录校验使用的凭据；`RevokeAll` 清空会话存储，所有 Cookie 立即失效。Web 登录处理器每次从 SessionManager 读取当前用户名/哈希。

## 5. 密码/token 回显

### 5.1 允许回显的内容

- 服务端 Web：current/previous 的 HY2 密码、control token。
- 客户端 Web：current 的 HY2 密码、control token。
- Web 管理员用户名。
- Web 管理员密码：**不可回显**，只显示“已设置”。

### 5.2 安全约束

- 默认打码；用户点“显示”后才请求。
- 显示接口要求当前 Web 管理员密码确认。
- 响应头 `Cache-Control: no-store`。
- 不写入结构化日志、不写入 URL、不写入错误信息。
- 显示接口限速：每 IP 每分钟 5 次失败后短暂锁定。
- 会话失效后不能调用。
- 页面关闭或刷新后不保留明文。

### 5.3 API

服务端：

- `POST /api/settings/credentials/reveal`
  - 请求：`{"password":"当前 Web 密码"}`
  - 响应：`{"current":{"generation":2,"auth":"...","control_token":"..."},"previous":{"generation":1,"auth":"...","control_token":"..."}}`
- 客户端：
  - 同样的接口，返回 current。

## 6. 热生效机制

### 6.1 服务端

**HY2 认证密码 / control token**

- 新增 `dynamicAuthenticator`，实现 `hyserver.Authenticator`，内部用 `atomic.Pointer[CredentialSet]` 读取 current/previous。
- `control` 包的 token 校验也读取同一个 `CredentialSet`。
- 修改时先写配置，再 `atomic.Store` 新集合；失败则保持旧集合。
- 不需要重启 HY2 服务端。

**端口白名单**

- `mappingManager` 持有 `atomic.Pointer[ports.Policy]`。
- `PUT /api/settings/ports` 构造并校验新 Policy，写配置，成功后原子替换。
- 已建立的映射保持运行；新注册/更新使用新 Policy。
- Web 显示提示：被移出白名单的已运行映射不会自动关闭。

**offline_reserve**

- `state.Store` 增加 `SetOfflineReserve(time.Duration)`，用原子值；`SetOnline(false)` 使用当前值。
- 写配置成功后调用。

**web.trust_proxy**

- Web 处理器读取 `atomic.Bool`；修改后立即生效。

**Web 管理员**

- SessionManager 的 `UpdateAdmin` + `RevokeAll`，见 4.3。

### 6.2 客户端

**HY2 认证密码 / control token**

- 原子写配置，成功后关闭控制流和 HY2 客户端，触发重连循环。
- 不重启进程。
- 重连使用新 `auth` / `control.token`。
- Web 返回 202，前端轮询 `/api/status` 显示“重连中/已连接”。

**客户端名称**

- `control.Update` 增加可选 `name` 字段。
- 客户端写配置并发送 update；服务端更新 session/state 的显示名。
- 不重连。

**Web 管理员**

- 同服务端，SessionManager 更新并撤销会话。

**映射**

- 已有能力，不变。

### 6.3 配置写入顺序

统一规则：

1. 校验输入。
2. 构造新配置副本。
3. 写临时文件 → fsync → rename → `.bak`。
4. 写成功后切换运行时状态。
5. 写失败则运行时不变，返回 500。

如果运行时切换失败（理论上只在构造 Policy 等阶段发生），恢复旧配置文件和旧运行时。

## 7. 协议变更

### 7.1 Hello

新增字段：

```json
{
  "type": "hello",
  "credential_generation": 2,
  "capabilities": ["credential_update_v1"]
}
```

`capabilities` 缺失表示旧客户端，不支持自动凭据推送；服务端只对这些客户端标记 `unsupported`，要求手动同步。

### 7.2 HelloAck

新增字段：

```json
{
  "type": "hello_ack",
  "credential_generation": 2,
  "credential_synced": true
}
```

### 7.3 CredentialUpdate（服务端 → 客户端，自动推送）

```json
{
  "type": "credential_update",
  "generation": 2,
  "auth": "new-hy2-password",
  "control_token": "new-control-token"
}
```

### 7.4 CredentialAck（客户端 → 服务端）

```json
{
  "type": "credential_ack",
  "generation": 2,
  "ok": true,
  "error": ""
}
```

### 7.5 Update 增加 name

```json
{
  "type": "update",
  "revision": 19,
  "name": "new-name",
  "mappings": [...]
}
```

服务端更新 session name 和 state；name 为空表示不修改。

## 8. API 与页面

### 8.1 服务端 Web

页面 `/settings` 从只读改为分区表单：

1. **Web 管理员**
   - 用户名
   - 当前密码
   - 新密码 / 确认
   - 保存
2. **HY2 凭据**
   - 当前 generation
   - current 密码/token（打码 + 显示）
   - previous 密码/token（打码 + 显示）
   - 新密码、新 control token
   - 同步方式：自动推送 / 手动同步
   - 保存并轮换
   - 撤销旧凭据
   - 客户端同步状态表：名称、generation、状态、最后在线
3. **端口白名单**
   - allow / deny / auto_range / offline_reserve
   - 保存
   - 提示：已运行映射不受影响
4. **代理信任**
   - trust_proxy 开关
   - 当前密码确认
5. **只读项**
   - listen、TLS、obfs、QUIC、Web 监听、日志、state_file
   - 提示“修改需编辑文件并重启”

API：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/settings` | 返回 Web 管理员、端口、trust_proxy、generation、客户端同步状态；不含明文密码/token |
| PUT | `/api/settings/web` | 修改 Web 管理员用户名/密码 |
| PUT | `/api/settings/ports` | 修改端口策略 |
| PUT | `/api/settings/credentials` | 轮换 HY2 密码/control token，带 sync_mode |
| POST | `/api/settings/credentials/reveal` | 回显 current/previous 密码/token |
| POST | `/api/settings/credentials/revoke` | 撤销 previous |

所有写操作要求 `X-Requested-With: hy2tun` + 同源 Origin。Web 管理员、trust-proxy 和 HY2 凭据操作还要求当前 Web 密码确认；端口策略修改不需要重复密码。

### 8.2 客户端 Web

页面 `/settings`：

1. **Web 管理员**
   - 用户名、当前密码、新密码/确认
2. **HY2 凭据**
   - 本地 generation、服务端 generation（来自 hello_ack）
   - current 密码/token（打码 + 显示）
   - 新 generation、新密码、新 token
   - 保存并重连
3. **客户端名称**
4. **只读项**
   - server 地址、TLS、obfs、bandwidth、本地代理监听
   - 提示“修改需编辑文件并重启”

API：

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/api/settings` | 返回 Web 管理员、客户端名称、本地/服务端 generation、脱敏凭据 |
| PUT | `/api/settings/web` | 修改 Web 管理员 |
| PUT | `/api/settings/credentials` | 手动同步 generation/auth/control token，触发重连 |
| POST | `/api/settings/credentials/reveal` | 回显 current 密码/token |

## 9. 配置格式

服务端示例：

```yaml
auth:
  type: password
  password: "new-password"
control:
  token: "new-token"
credentials:
  generation: 2
  previous_generation: 1
  previous_auth: "old-password"
  previous_control_token: "old-token"
```

客户端示例：

```yaml
auth: "new-password"
control:
  token: "new-token"
credentials:
  generation: 2
```

兼容性：

- 旧配置没有 `credentials`：generation 视为 1，previous 为空。
- 旧客户端 hello 没有 `credential_generation`：服务端按 1 处理；如果服务端 current 是 1，标记 synced。
- 新客户端连接旧服务端：服务端忽略未知字段；客户端仍能工作，但没有代际迁移能力。

## 10. 安全

- Web 管理员、trust-proxy 和 HY2 凭据等敏感写操作要求当前 Web 密码确认；失败限速。端口策略修改只要求会话和 CSRF。
- 回显接口不缓存、不写日志、不出现在 URL。
- 密码/token 只存于 0600 配置文件和内存；`.bak` 同样 0600。
- 撤销旧凭据后，旧密码/token 不能建立新连接；主动关闭旧代际控制会话。
- 标准 HY2 代理的已建立连接可能持续到自然关闭；文档明确说明。
- Web 管理员密码只存 bcrypt；不能回显。
- 修改 Web 管理员后撤销所有会话。
- 配置写入失败不改变运行时；避免半更新。
- 连续轮换时提示更早代际客户端会掉线。

## 11. 测试与验收

### 11.1 单元测试

- generation 默认/递增/previous 清空。
- dynamicAuthenticator 接受 current/previous，拒绝其他。
- control token 接受 current/previous。
- 配置 Save/Load 保留 previous 凭据和 generation。
- Web 管理员更新后旧会话失效。
- 端口 Policy 热替换；已运行映射不受影响。
- state offline reserve 热更新。
- CredentialUpdate/Ack 编解码。
- Update.name 更新 session name。

### 11.2 集成测试

1. 服务端 Web 轮换凭据，自动推送：
   - 客户端配置更新到新 generation。
   - 客户端重连成功。
   - 服务端显示 synced。
2. 手动同步：
   - 服务端不推送。
   - 客户端 Web 填新值后重连成功。
   - 服务端显示 synced。
3. 离线客户端：
   - 自动推送不覆盖离线客户端。
   - 旧凭据仍可上线。
   - 撤销后旧客户端无法连接。
4. 撤销旧凭据：
   - 旧代际控制会话被关闭。
   - 旧密码新连接失败。
5. 回显：
   - 错误 Web 密码拒绝。
   - 正确密码返回 current/previous。
   - 响应 no-store。
6. Web 管理员修改：
   - 旧会话 401。
   - 新用户名/密码可登录。
7. 端口白名单热更新：
   - 新端口申请成功/失败符合新 Policy。
   - 已运行映射继续工作。
8. 客户端名称热更新：
   - 服务端 Web 显示新名称。
9. 配置写入失败：
   - 运行时保持旧值，接口返回错误。
10. 单字段轮换：
   - 只改密码时 previous=(oldPassword, currentToken)，旧客户端可连接。
   - 只改 token 时 previous=(currentPassword, oldToken)。
   - 两个字段都留空或新值相同返回 400。
11. 自动推送超时：
   - 一个客户端不 ack，接口在 5 秒内返回 timeout，旧凭据仍有效。
   - 客户端保持 pending，不阻塞其他客户端。
12. 第二次轮换：
   - 更早代际的控制会话被关闭；新 previous 的客户端仍可连接。
13. 旧客户端能力：
   - hello 无 `credential_update_v1` 时不发送推送，标记 unsupported。
14. 手动 generation：
   - 小于等于本地 generation 返回 400。
   - 与服务端 generation 不一致时警告但可提交，状态显示 pending/unknown。

### 11.3 E2E

在现有 E2E 基础上增加：

- 服务端 Web 改 HY2 密码/control token，自动推送。
- 客户端重连，TCP/UDP 映射继续工作。
- 撤销旧凭据，旧客户端连接失败。
- 客户端 Web 改名称/凭据，服务端视图更新。

## 12. 风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| 改 Web 密码后锁死 | 无法登录 | 当前密码确认、二次确认、会话撤销后仍可用新密码登录 |
| 自动推送半失败 | 客户端配置不一致 | 写盘成功后 ack；失败保持旧配置；服务端显示 pending |
| 撤销旧凭据后离线客户端失联 | 需要手动更新 | 撤销前显示待同步列表和确认 |
| 旧标准 HY2 连接继续 | 旧密码连接未立即断 | 文档说明；新连接已拒绝 |
| 配置序列化丢字段 | 改一个字段丢其他字段 | 全量结构体读写 + round-trip 测试 |
| Web 回显被截屏 | 凭据泄露 | 默认打码、当前密码确认、no-store、限速 |
| 端口白名单热更新与已运行映射不一致 | 旧映射继续运行 | Web 明确提示；需要时手动删除映射 |

## 13. 已定的实现规则（审查后补充）

1. **单字段轮换**：HY2 密码和 control token 可以只改一个；留空表示保持；previous 保存旧的 current 对；只校验实际改动的字段。
2. **自动推送返回**：请求最多等待 5 秒；返回每个在线客户端的 `synced/pending/unsupported/timeout`；超时客户端保持 pending，不自动重试。
3. **第二次轮换**：主动关闭 generation 既不是新 current、也不是新 previous 的控制会话，避免更早代际继续在线。
4. **手动 generation**：客户端 Web 要求 `generation > 本地 generation`；与服务端 generation 不一致时警告但允许提交；generation 只用于状态展示，不参与认证。
5. **旧客户端兼容**：`hello` 增加 `capabilities: ["credential_update_v1"]`；没有该能力的客户端不接收自动推送，标记 `unsupported`，走手动同步，避免旧客户端因未知消息类型断线。

## 14. 留给实现者的


- 具体 API 路径、表单字段顺序、错误文案。
- `CredentialSet` 的包位置和原子指针实现。
- dynamicAuthenticator 的 id 返回值（建议 current/previous 都返回 `user`）。
- Web 设置页的布局细节。
- 审计日志格式（建议 slog info，只记字段名，不记值）。
- 是否在 v1.1 支持 `userpass`（建议不支持）。
- 是否把 `auto_range`/`offline_reserve` 放在同一个端口设置 API（建议是）。
- 是否在客户端 Web 显示服务端 generation 的历史（建议只显示最近一次 hello_ack 的值）。
