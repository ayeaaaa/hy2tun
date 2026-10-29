# hy2tun OpenWrt LuCI 基础配置页设计

- 状态：待批准
- 日期：2026-10-29
- 目标版本：v1.5.0
- 目标读者：实现该功能的工程师

## 1. 背景与目标

当前 OpenWrt 安装后，用户必须 SSH 到路由器编辑
`/etc/hy2tun/client.yaml`，再执行 init 脚本启动服务。LuCI 页面只能看状态和
跳转到客户端 Web 控制台。

本版本把 LuCI 的 **Services → hy2tun** 页面变成单服务端基础配置面板：

- 在页面填写一份可运行的单服务端 YAML 配置。
- 保存前校验，失败不覆盖旧配置。
- 保存后可启动、停止、重启服务，并设置开机自启。
- 服务运行时可直接打开客户端 Web 控制台。
- 页面显示服务状态和最近日志。
- 多服务端、obfs、带宽、代理等高级配置继续走客户端 Web 控制台或 SSH，
  LuCI 不提供这些字段。

目标是让普通 OpenWrt 用户完成：

```text
安装 ipk -> 打开 LuCI -> 填服务端/认证/token/映射/控制台密码
-> 保存 -> 启动 -> 打开控制台
```

不需要再手写 YAML 或执行 SSH 命令。

## 2. 非目标

- 不在 LuCI 中管理多服务端；检测到 `servers:` 时只显示提示和客户端控制台入口。
- 不编辑 obfs、带宽、传输/端口跳跃、SOCKS5/HTTP 代理、Web TLS 证书、
  CA 文件、日志文件等高级字段。
- 不做原始 YAML 文本编辑器。
- 不做服务端 `hy2tund` 的 LuCI 管理。
- 不实现 CA 证书上传；需要自定义 CA 的用户使用高级配置。
- 不改变客户端 Web 控制台现有的多服务端/高级配置能力。

## 3. 用户体验

### 3.1 首次安装

`hy2tun` 包的 post-install 不再把 `client.yaml.example` 复制为
`/etc/hy2tun/client.yaml`，也不默认启用服务。首次打开 LuCI 页面时：

- 显示空的基础配置表单。
- 默认客户端名称为 `openwrt`。
- 默认 Web 控制台监听 `0.0.0.0:7501`，用户名 `admin`。
- 用户必须填写服务端、HY2 认证、control token；如果启用 Web 控制台，
  还必须设置控制台密码。
- 保存成功后，用户点击“启动”，再点击“打开控制台”。

升级已有安装时，不删除、不覆盖现有 `client.yaml`，也不改变其启用状态。

### 3.2 页面结构

页面分为以下区域：

1. **状态卡片**
   - 运行状态：运行中/未运行。
   - 开机自启：开/关。
   - 配置文件状态：未创建/已创建/无效。
   - 多服务端提示（如适用）。
   - 按钮：启动、停止、重启、刷新。

2. **基础配置表单**
   - 服务端地址：`host:port`，也接受端口范围。
   - HY2 认证：密码输入框；已有值时显示“已设置，留空保持不变”。
   - Control token：密码输入框；已有值时显示“已设置，留空保持不变”。
   - TLS SNI：可选；留空使用服务端地址中的主机名。
   - 跳过证书校验：复选框，带安全警告；默认关闭。
   - 客户端名称：默认 `openwrt`。
   - 映射表：
     - 类型：TCP/UDP。
     - 远端端口：`0` 表示由服务端自动分配。
     - 本地地址：`host:port`。
     - 备注：可选。
     - 启用：复选框。
     - 添加/删除行。
   - Web 控制台：
     - 监听地址，默认 `0.0.0.0:7501`；留空表示禁用控制台。
     - 用户名，默认 `admin`。
     - 密码：新配置必填；已有密码时留空保持不变；需要二次确认。
   - 服务停止时：保存、保存并启动按钮。
   - 服务运行时：表单只读，提供停止、重启当前配置按钮。

3. **高级配置提示**
   - 如果现有配置包含高级字段，显示“保存基础配置会保留这些字段，但 YAML
     注释可能被重新序列化”。
   - 提示按字段区分：客户端控制台可修改认证/token/映射/代理/Web 账号密码；
     TLS CA/pin、obfs、带宽、传输、Web TLS、日志文件需要 SSH。
   - 如果配置是多服务端，隐藏表单，只显示“此配置由客户端控制台管理”和
     “打开客户端控制台”按钮。

4. **日志**
   - 显示 `logread | grep hy2tun | tail -50` 的结果。
   - 提供刷新按钮。

### 3.3 控制台链接

- 仅当服务运行且 `web.listen` 非空时启用“打开控制台”。
- 如果监听地址是 `0.0.0.0`、`::` 或空主机，使用浏览器当前主机名。
- 如果监听地址是 `127.0.0.1` 或 `::1`，链接使用回环地址，并提示该地址可能
  只能从路由器本机访问。
- 如果配置了 Web TLS（高级字段），使用 `https`；否则使用 `http`。
- 端口取自 `web.listen`。

### 3.4 保存与重启语义

运行中的客户端会写回 `client.yaml`（首次 client ID、服务端分配端口、
服务端推送、Web 编辑等）。因此不允许 LuCI 在服务运行时覆盖配置。

状态矩阵：

| 服务状态 | 表单 | 可用操作 |
| --- | --- | --- |
| 运行中 | 只读 | 停止、重启当前配置、刷新、开机自启、打开控制台、查看日志 |
| 已停止 | 可编辑 | 保存、保存并启动、启动、刷新、开机自启、查看日志 |
| 多服务端 | 隐藏 | 打开控制台、启停/重启、开机自启、查看日志 |
| YAML 解析失败 | 隐藏 | 停止/禁用、开机自启、查看状态/日志、SSH 修复提示；启动/重启禁用 |

“重启”只在服务运行中提供；停止状态使用“启动”。启动和重启都执行相同的
`--check-config`、Web 密码前置检查和 5 秒运行状态轮询。

- “停止”：停止服务，不修改配置；停止完成后重新 GET 配置，表单变为可编辑。
- “保存”：仅在服务停止时允许。校验并原子写入；不启动服务。
- “保存并启动”：仅在服务停止时允许。保存成功后启动。
- “启动”：启动前执行 `hy2tun --check-config`；配置不存在或无效时返回错误；
  Web 控制台已启用但没有密码哈希时也拒绝启动，提示先设置密码或清空监听
  地址。表单有未保存修改时先提示保存。
- 启动/重启后轮询 `running` 最多 5 秒；仍未运行则返回错误和最新日志。
- “重启”：不修改配置，只重启当前文件；仅在服务运行中提供，停止状态使用
  “启动”。重启执行与启动相同的预检查。
- “Web 控制台已启用但没有密码哈希”的拒绝启动规则是 LuCI 前置检查；现有
  `hy2tun`/init 脚本的首次启动自动生成密码行为保持不变，避免破坏直接使用
  CLI 的部署。
- “开机自启”开关在所有状态都可用（包括运行中、多服务端和 YAML 解析失败），
  调用 init 脚本的 enable/disable，不改变当前运行状态。

`running` 的权威来源是 `/etc/init.d/hy2tun status` 的退出码（0 为运行中），
即 procd 的服务实例状态。崩溃/respawn 循环也视为运行中，因此保存会被阻止；
用户需要先停止服务。停止/启动后轮询该状态，最多 5 秒。
- 不提供“保存并重启”这个复合动作；需要应用修改时，停止 → 编辑 → 保存并
  启动。这样避免运行中写回与 LuCI 保存互相覆盖。

配置 revision 是配置文件内容的 SHA-256。`config get` 返回当前 revision；
`config set` 必须携带页面加载时的 revision。文件已被其他页面或客户端修改时，
保存返回冲突（HTTP 409），页面提示刷新，不覆盖新内容。服务停止后客户端
不再写回，revision 稳定；停止完成后页面必须重新 GET 配置。

### 3.5 无效配置

- YAML 能解析但校验失败：`config get` 仍返回字段、`valid: false` 和错误信息，
  页面显示表单和错误，允许修正基础字段。
- 如果错误来自基础表单不可编辑的高级字段（例如代理冲突、TLS 文件、
  `transport`、`log.level`），页面显示“高级字段可能无效，请使用客户端控制台
  或 SSH 修复”，但**不禁用保存**，因为修改基础字段可能解除跨字段冲突
  （例如 `web.listen` 与代理端口冲突）。保存仍由后端校验，失败时返回带
  `advanced: true` 的结构化错误，不写文件。
- 如果配置包含未知顶层字段，页面显示“未知字段无法保留，请使用客户端控制台
  或 SSH 修改”，并禁用保存；`config set` 也拒绝写入，避免静默删除。
- YAML 无法解析：页面显示错误和 SSH 修复提示，不显示可保存表单。
- 多服务端配置：页面只显示只读提示和控制台入口，不显示基础表单。

## 4. 后端配置助手

### 4.1 为什么需要 CLI 助手

LuCI 使用 Lua，直接解析/生成 YAML 容易破坏现有配置、注释和高级字段，也
无法可靠生成 bcrypt 密码哈希。因此给客户端二进制增加一个本地配置子命令，
由 LuCI 调用：

```text
hy2tun config get -c /etc/hy2tun/client.yaml
hy2tun config set -c /etc/hy2tun/client.yaml
```

`config set` 从标准输入读取 JSON；`config get` 向标准输出写 JSON。
两者都不输出 HY2 密码、control token、Web 密码或密码哈希。

### 4.2 `config get` 输出

```json
{
  "exists": true,
  "multi": false,
  "revision": "sha256-hex-of-config-file",
  "valid": true,
  "validation_error": "",
  "advanced_error": false,
  "server": "us.example.com:24443",
  "server_identity": "us.example.com:24443",
  "server_locked": false,
  "credential_generation": 1,
  "auth_set": true,
  "control_token_set": true,
  "tls_sni": "us.example.com",
  "tls_insecure": false,
  "client_name": "openwrt",
  "mappings": [
    {
      "id": "m-ssh",
      "type": "tcp",
      "remote_port": 25022,
      "local_addr": "127.0.0.1:22",
      "note": "router ssh",
      "enabled": true
    }
  ],
  "web_listen": "0.0.0.0:7501",
  "web_username": "admin",
  "password_set": true,
  "web_tls": false,
  "advanced": false,
  "unknown_fields": false,
  "log_level": "info",
  "log_file": ""
}
```

规则：

- 配置文件不存在时返回 `exists: false`、空 `revision`、`valid: false`、
  `validation_error: ""` 和空表单默认值：`client_name: "openwrt"`、
  `web_listen: "0.0.0.0:7501"`、`web_username: "admin"`。页面按
  `exists:false` 显示新建状态，不显示空错误横幅。
- 配置文件存在时返回原始值，不合成默认值；已有的空 `web.listen` 保持为空，
  不会因为打开页面或保存而自动开启 Web 控制台。`valid`/`advanced_error`
  在配置副本上计算，不用 `Validate` 的副作用覆盖表单值。
- 空映射 ID 原样返回为空；前端只用自增 DOM key 区分行，不把临时 ID 当成
  服务端映射 ID。保存时由 `SaveClient` 生成 ID，action 返回新配置后页面用
  返回值替换映射行。`credential_generation` 的 0 值按 1 展示。
- YAML 无法解析时返回非零退出码和错误信息；LuCI 不显示表单，避免覆盖。
- YAML 能解析但校验失败时，仍返回字段，并设置 `valid: false` 和
  `validation_error`，允许用户在表单中修正。
- `config get` 同时执行 `Validate` 和 `CheckClient`；`valid` 只有在两者都
  通过时为真。`CheckClient` 的 CA/Web TLS/bcrypt/log 文件检查会做少量文件
  系统读取，页面加载可接受。
- `advanced_error: true` 表示基础字段副本通过、但完整 `Validate` 或
  `CheckClient` 失败；页面显示 SSH/客户端控制台提示，但不禁用保存，因为
  基础字段修改可能解除跨字段冲突。
- `multi: true` 时返回只读的 `exists`、`revision`、`valid`、
  `validation_error`、`web_listen`、`web_tls`、`password_set`、`log_file`；
  不返回可编辑表单字段。多服务端页面不提供密码设置，启动时允许客户端按现有
  行为生成密码并在日志中显示。
- `advanced: true` 表示存在基础表单之外的单服务端字段；只用于提示。
- `unknown_fields: true` 表示 YAML 顶层存在未知键；页面禁用保存，避免静默
  删除未知配置。
- `auth_set`、`control_token_set`、`password_set` 只报告是否已设置，
  不返回秘密值。
- `tls_sni` 返回 YAML 原始值，不返回根据服务端主机名推导的有效 SNI；留空
  在保存后仍为空。
- `server_identity` 是规范化后的服务端身份：主机名小写、去空白，端口范围
  排序并合并等价范围；用于判断“服务端是否变更”，避免空白/大小写/等价范围
  误触发迁移。
- `server_locked: true` 表示存在服务端相关高级字段（`tls.ca`、
  `tls.pin_sha256`、`obfs`、`bandwidth`、`transport`）或凭据代际大于 1；
  此时页面只读服务端地址，避免把旧服务端配置套到新服务端。
- `credential_generation` 是当前凭据代际。代际大于 1 时页面只读 HY2 认证和
  control token，提示使用客户端控制台轮换。
- `valid` 为 `Validate`、`CheckClient` 以及“单服务端且 Web 控制台启用时必须
  有密码哈希”三者都通过；后者失败时 `validation_error` 明确提示。多服务端
  配置不应用密码哈希前置条件，`valid` 只反映 `Validate`/`CheckClient`。
- `log_file` 是高级 `log.file` 的只读路径；日志 action 优先读取该文件，为空
  时使用 `logread`。
- `revision` 是文件内容的 SHA-256；配置文件不存在时为空。

### 4.3 `config set` 输入

```json
{
  "expected_revision": "sha256-hex-of-config-file",
  "server": "us.example.com:24443",
  "auth": "",
  "control_token": "",
  "tls_sni": "us.example.com",
  "tls_insecure": false,
  "client_name": "openwrt",
  "mappings": [
    {
      "id": "m-ssh",
      "type": "tcp",
      "remote_port": 25022,
      "local_addr": "127.0.0.1:22",
      "note": "router ssh",
      "enabled": true
    }
  ],
  "web_listen": "0.0.0.0:7501",
  "web_username": "admin",
  "web_password": "",
  "web_password_confirm": ""
}
```

规则：

- `expected_revision` 必须匹配当前文件内容的 SHA-256；新配置为空字符串。
  不匹配返回冲突，不写文件。读取 revision、比较、写入三步在
  `<config>.lock` 的 advisory `flock` 内完成，避免两个并发保存都通过检查；
  Windows 构建允许退化为无锁（该子命令只用于 OpenWrt/Linux）。
- `server`、`client_name`、`mappings`、`web_listen`、`web_username` 必须出现
  在 JSON 中；缺失字段返回 `code:"validation"`，不能把“缺失”当成“清空”。
  显式空数组/空字符串是有效输入，分别表示清空映射/禁用控制台。
  `auth`、`control_token`、`web_password` 可以缺失或为空，表示保留。
- 服务端地址变更以 `server_identity` 规范化比较，而不是原始字符串比较。
  旧 `server` 为空或无法解析时，新值按“新服务端”处理：要求同时提供新的
  auth/token，并清空 `client.id`/`revision`，避免静默复用旧凭据。
  变更时，如果现有配置存在服务端相关高级字段（`tls.ca`、`tls.pin_sha256`、
  `obfs`、`bandwidth`、`transport`）或凭据代际大于 1，拒绝保存并提示使用
  客户端控制台/SSH；服务端身份不变时允许编辑其他基础字段。
- 允许的服务端地址变更按“迁移到新服务端”处理：必须同时提供新的 `auth`
  和 `control_token`（不能留空沿用），并清空 `client.id`、把
  `client.revision` 设为 0；映射保留页面中的 local/remote 配置，用户需自行
  确认远端端口在新服务端可用。
- 服务端身份变化且 `tls_sni` 与旧值相同（用户没有主动改 SNI）时清空
  `tls_sni`，让新主机名作为默认；用户需要自定义 SNI 时在迁移后重新填写。
  页面在服务端地址输入变化时自动清空 SNI 输入框。
- `auth` 为空表示保留现有值；新配置中为空则校验失败；新值至少 8 字节。
  凭据代际大于 1 时拒绝修改认证，提示使用客户端控制台轮换。
- `control_token` 为空表示保留现有值；新配置中为空则校验失败；沿用至少
  16 字节校验；凭据代际大于 1 时拒绝修改。
- LuCI 修改 auth/token **不增加** `credentials.generation`，也不写
  `previous_*`；轮换仍由客户端控制台/服务端推送负责。
- `web_password` 为空表示保留现有哈希；新配置启用 Web 控制台且没有哈希时
  校验失败；新密码至少 8 个字符，且必须与 `web_password_confirm` 一致。
- `web_password` 非空时使用 bcrypt 生成哈希，明文不落盘。
- `web_listen` 为空表示禁用 Web 控制台；此时清空 `web.password_hash`，
  且 `web_password` 必须为空；重新启用时必须设置新密码。
- `tls_sni` 为空表示清除 SNI；`tls_insecure` 直接覆盖。
- 映射的 `id` 为空时由现有 `SaveClient` 生成；已有 id 保持不变。
- 多服务端配置直接拒绝，返回“请使用客户端控制台”错误。
- 配置含未知顶层字段时直接拒绝，返回“请使用客户端控制台或 SSH 修改”错误。
- 读取现有配置使用 **不校验** 的 `LoadClientForEdit`（只做 YAML 解析），
  这样解析成功但校验失败的配置也能被页面修复；不能使用会先校验的
  `LoadClient`。
- 更新后调用现有 `Validate` 和 `CheckClient`；失败不写文件。基础字段先单独
  校验；基础字段通过后，`Validate`/`CheckClient` 仍失败的错误标记为
  `advanced: true`，页面显示 SSH/客户端控制台提示。
- 写入使用 `SaveClient`；它会把 `credentials.generation: 0` 规范为 1、空
  `web.username` 在有 Web 监听时规范为 `admin`、空 `log.level` 规范为
  `info`。这是现有配置默认语义，页面提示会说明保存会规范化这些默认值。
- 基础字段校验使用一份“只保留基础字段”的配置副本：清空 CA/pin、obfs、
  bandwidth、transport、proxy、Web TLS/trust_proxy、log.file，并把
  log.level 设为 `info`；其余字段（server/auth/token/client/mappings/
  web.listen/web.username/web.password/TLS SNI/insecure）保留。该副本的
  校验错误视为基础错误；副本通过而完整配置失败视为高级错误。
- 成功使用现有 `SaveClient`（临时文件、fsync、rename、0600、`.bak`），
  并返回新的 `revision`。

### 4.4 保留的高级字段

`config set` 先加载现有单服务端配置，只覆盖基础字段，保留：

- `tls.ca`、`tls.pin_sha256`
- `obfs`
- `bandwidth`
- `transport`
- `proxy`
- `credentials`
- `client.id`、`client.revision`
- `web.tls`、`web.trust_proxy`
- `log.level`、`log.file`

`advanced: true` 的判定：

- `tls.ca` 或 `tls.pin_sha256` 非空。
- `obfs` 非默认值。
- `bandwidth` 非默认值。
- `transport` 非空。
- `proxy` 非默认值。
- `web.tls` 非空或 `web.trust_proxy` 为真。
- `log.level` 为非空且不是 `info`，或 `log.file` 非空。

`client.id`、`client.revision`、`credentials.generation` 属于运行期元数据，
不触发高级提示。

YAML 顶层存在已知集合之外的键时设置 `unknown_fields: true`，页面禁用保存；
`config set` 拒绝写入。未知嵌套键不做完整递归检测，但页面始终提示“保存会
重新序列化 YAML，注释可能丢失”。已知高级字段会保留，未知顶层字段不会静默
删除。多服务端配置不做任何写入。

### 4.5 CLI 行为与错误契约

`config get`：

- YAML 解析成功但 `valid:false` 仍退出 0，输出完整 JSON。
- YAML 解析/IO 错误退出 1，stderr 输出 JSON
  `{"ok":false,"code":"parse_error","error":"..."}`。
- 不返回秘密值。

`config set`：

- 成功退出 0，stdout 输出 `{"ok":true,"revision":"..."}`。
- 失败退出码：
  - 2：基础校验失败。
  - 3：revision 冲突。
  - 4：未知顶层字段或多服务端配置。
  - 5：高级字段校验失败。
  - 1：内部/IO 错误。
- stderr 统一输出 JSON：
  `{"ok":false,"code":"validation|conflict|unknown_fields|multi|advanced|internal","error":"...","advanced":false}`。
- 不输出秘密值。
- `--require-stopped`：在锁内检查 `/etc/init.d/hy2tun status`；运行中返回
  `code:"service_running"`，退出码 6，不与 revision 冲突混淆。LuCI 的
  `save` 使用该参数。该检查与写入之间仍存在外部并发启动的极小窗口；单管理员
  场景接受这一 best-effort 语义。
- 所有命令只读写本地文件，不连接服务端。
- 帮助文本列出 `get`、`set`、`-c` 和 `--require-stopped`。

LuCI 映射：

- `config` JSON 的 `code:"conflict"` → HTTP 409，页面提示刷新。
- `code:"service_running"` → HTTP 400，页面提示先停止服务。
- `validation`/`advanced`/`unknown_fields`/`multi` → HTTP 400，页面显示对应
  提示。
- 内部错误 → HTTP 500。
- `save_start` 保存成功但启动失败时返回 `ok:false`、`saved:true`、新
  `config`/`revision`、`error` 和日志；页面保留新配置并显示启动失败，而不是
  当作保存失败。

## 5. LuCI 实现

### 5.1 控制器

在现有 `luci.controller.hy2tun` 中增加：

- `admin/services/hy2tun`：现有模板页。
- `admin/services/hy2tun/data`：GET，返回 JSON 状态和 `config get` 结果。
- `admin/services/hy2tun/action`：POST，`post = true`，由 LuCI 校验 CSRF
  token；接受 `op=save|save_start|start|stop|restart|enable|disable|logs`
  和 `payload` JSON。

`data` 返回：

```json
{
  "ok": true,
  "running": false,
  "enabled": false,
  "config": { "...": "config get 输出" },
  "config_error": "",
  "logs": "最近日志"
}
```

`config get` 失败时 `config` 为 `null`，`config_error` 包含解析/CLI 错误；
`running`、`enabled`、`logs` 仍然返回，页面可以显示状态和日志。

`action` 返回统一 JSON：

```json
{
  "ok": true,
  "config": {},
  "running": false,
  "enabled": false,
  "logs": "",
  "advanced": false
}
```

失败返回 `{"ok": false, "error": "...", "advanced": false, "config_error": "",
"running": false, "enabled": false, "logs": ""}`，HTTP 400、409 或 500。

### 5.2 调用助手

- `config get`：使用 `luci.sys.exec` 执行固定路径
  `/usr/bin/hy2tun config get -c /etc/hy2tun/client.yaml`，解析 stdout JSON。
- `config set`：把 JSON 写入 `/tmp` 下 0600 临时文件，执行
  `/usr/bin/hy2tun config set -c /etc/hy2tun/client.yaml < temp`，捕获 stderr
  和退出码；删除临时文件。
- `save` 仅在服务停止时执行；服务运行时返回 409/400 错误。
- `save_start` 仅在服务停止时执行：先带 `--require-stopped` 执行
  `config set`，成功后再 start；保存失败不启动，启动失败返回错误和最新日志。
- `start`/`restart` 的预检查：`hy2tun --check-config` 通过；单服务端配置如果
  `web.listen` 非空则 `password_set` 必须为真。多服务端配置跳过密码检查，
  允许客户端按现有行为生成密码并打印到日志。该密码检查只存在于 LuCI
  action，不改变 init 脚本/CLI 的现有行为。
- `stop` 执行后轮询确认 inactive（最多 5 秒），然后由页面重新 GET 配置和
  revision；不在控制器内自动保存。
- `restart` 仅在当前 `running=true` 时允许；停止状态返回错误并提示使用
  `start`。`start` 和 `restart` 都先执行启动预检查。
- 服务操作：只允许白名单动作，执行
  `/etc/init.d/hy2tun <action> >/dev/null 2>&1`，避免子进程输出污染 CGI
  响应。
- 日志：`log_file` 为空时执行固定命令
  `logread | grep hy2tun | tail -50`；非空时用
  `tail -n 50 -- <shellquote(log_file)>` 读取该文件。路径来自 root 管理的
  配置，命令使用 shellquote，不把路径拼进 URL。
- 所有命令路径固定，不拼接用户输入。

### 5.3 前端

- 使用现有 LuCI 页面模板，内联 JavaScript 和少量 CSS，不引入新前端依赖。
- 页面加载时 GET `data`，渲染状态和表单。
- 映射表用原生 JS 动态增删行；每行保留隐藏 `id`。
- 保存时校验：
  - 服务端地址非空。
  - 新配置的认证、token 非空。
  - 新密码至少 8 个字符，且确认密码一致。
  - 启用 Web 控制台且没有已有密码时，新密码必填。
  - 映射本地地址非空。
- 保存 POST `application/x-www-form-urlencoded`，字段为 `op=save`、
  `payload=<JSON>`、`token=<CSRF>`；这样兼容 LuCI 的 POST 安全校验。
- 保存成功后 action 重新执行 `config get`，返回新的 `config`（包含生成的
  映射 ID 和新 revision）；页面用它替换当前表单数据/映射行，再刷新状态。
  `save_start` 在同一个 action 内完成保存和启动。
- 服务运行时表单只读；停止后重新 GET 配置，避免使用运行期写回前的旧
  revision。服务停止后表单可编辑。
- `server_locked` 时服务端地址字段只读；`credential_generation > 1` 时
  认证/token 字段只读，并显示控制台轮换提示。
- 页面用原始 `server` 字符串变化显示“如果这是不同服务端，需要重新填写认证/
  token”的软提示；实际是否迁移由后端 `server_identity` 判断。等价写法只显示
  提示但不会触发迁移。迁移成功后接受返回的新配置（client.id 已清空、
  revision 已重置）。
- 状态按钮在请求期间禁用，防止重复操作。
- 错误显示在页面顶部，不吞掉后端错误；revision 冲突提示刷新。
- 多服务端：隐藏表单，使用只读 `web_listen`/`web_tls` 显示客户端控制台
  入口。
- 无效配置：显示 `validation_error`，基础字段错误时保留表单允许修正；
  `advanced_error` 只显示 SSH/客户端控制台提示，不禁用保存。
- `unknown_fields: true` 时禁用保存，并提示使用客户端控制台/SSH。
- YAML 解析错误：显示 `config_error` 和 SSH 修复提示，不显示可保存表单。

### 5.4 页面安全

- 页面仅位于 LuCI `admin` 下，依赖 LuCI 登录会话。
- POST action 使用 `post = true`，由 LuCI 校验 `token`。
- `config get` 不返回秘密值；表单中秘密字段初始为空。
- 密码明文只存在于浏览器内存和一次 CLI stdin 临时文件；CLI 生成 bcrypt
  哈希后立即删除临时文件。
- 配置文件权限保持 0600。
- 日志原样展示，但仅 LuCI 管理员可见；不把日志写入 URL。

## 6. 包与兼容性

- `hy2tun` 二进制新增 `config` 子命令；现有 `--check-config`、`-c`、
  `--version` 行为不变。
- `luci-app-hy2tun` 新增控制器 action 和页面 JS；依赖改为
  `luci-base, luci-compat, hy2tun (>= 1.5.0)`，避免新页面安装到旧二进制。
- `hy2tun` 包 post-install 改为：
  - 安装 `/etc/hy2tun/client.yaml.example`。
  - 不复制为 `client.yaml`。
  - 不默认 enable 服务。
  - 升级时如果已有 `client.yaml`，不修改、不删除、不改变 enable 状态。
- 新增 `examples/client-basic.yaml` 作为包内示例，只包含单服务端基础字段；
  `examples/client.yaml` 保留为完整高级参考。这样用户手动复制包内示例时不会
  立刻得到 `server_locked:true` 或缺失 CA。
- `scripts/openwrt/README.md` 和双语文档更新安装流程。
- 旧单服务端配置继续可用；保存基础表单后 YAML 会重新序列化。
- 多服务端配置完全不受页面写入影响。

## 7. 测试

### 7.1 Go 单元测试

覆盖：

- 新配置 `get` 返回默认值、`exists: false`、`valid: false` 和空错误。
- `set` 创建配置、生成映射 id、生成 bcrypt 哈希。
- 留空 auth/token/password 保留旧值。
- 新配置缺少 auth/token/password 被拒绝；短密码/短认证被拒绝。
- 多服务端 `get` 标记 `multi` 并返回只读 `web_listen`/`web_tls`/
  `password_set`；`set` 拒绝；多服务端 `valid` 不受密码哈希前置条件影响，
  启动跳过该检查。
- 高级字段在 `set` 后保留；`advanced` 检测覆盖各高级字段和未知顶层键。
- 无效映射、无效 Web 监听、无效密码不覆盖旧文件。
- 配置解析失败不覆盖旧文件。
- revision 不匹配拒绝写入；成功写入返回新 revision；并发保存只有一个成功。
- 错误码/JSON code 与退出码符合 4.5；`config get` 校验失败仍退出 0。
- `--require-stopped` 在服务运行时拒绝写入。
- 保存后重新 `get` 返回生成的映射 ID，连续保存不会重新分配 ID。
- Web 控制台禁用时清空密码哈希，重新启用必须提供新密码。
- Web 控制台启用但无密码哈希时 `valid: false`，且拒绝启动。
- 存在服务端相关高级字段时，修改 `server` 被拒绝；不改 `server` 仍可保存。
- 允许修改 `server` 时必须提供新 auth/token，并清空 client.id/revision。
- 等价服务端写法（空白/大小写/范围顺序）不触发迁移；真正的主机或端口变化
  才触发；旧 server 为空/不可解析时按新服务端处理。
- 缺失必填 JSON 字段被拒绝，显式空 mappings/web.listen 被接受。
- 凭据代际大于 1 时修改 server/auth/token 被拒绝；auth/token 修改不增加
  generation。
- 已有配置的空 `web.listen` 不被默认值覆盖。
- `tls_sni` 原样返回，空值保存后仍为空；服务端迁移时旧 SNI 被清空，显式
  新 SNI 保留。
- `log_file` 非空时日志 action 读取文件；`log.level: debug` 标记 advanced。
- `client-basic.yaml` 不包含高级字段，复制后页面可用。
- 高级字段无效时返回 `advanced: true`，不覆盖旧文件。
- `.bak` 和 0600 权限保持。

### 7.2 LuCI/包测试

- `scripts/openwrt/test-ipk.sh` 检查新控制器 action、页面文件和 CLI 子命令
  在包内。
- 控制器 Lua 语法检查；至少验证 `require "nixio.fs"` 和 POST action 存在。
- 页面不再把 init 脚本输出写入 CGI 响应。

### 7.3 真机验证

在 ImmortalWrt 24.10.2 x86_64：

1. 安装新 `.ipk`，确认没有自动生成 active 配置、没有自动 enable。
2. 打开 LuCI 页面，填写服务端/认证/token/映射/控制台密码，保存。
3. 启动服务，确认状态变为运行中。
4. 打开客户端 Web 控制台，确认密码可登录。
5. 服务运行时确认表单只读；停止后编辑映射并保存、启动，确认服务端映射更新。
6. 使用“保存并启动”，确认 save→start 顺序生效。
7. 构造 revision 冲突（两个页面先后保存），确认后者被拒绝且提示刷新。
8. 停止/启动/重启切换；运行中、停止、多服务端和解析失败状态下都能切换
   开机自启。
9. 多服务端配置：页面只显示提示和控制台入口，不覆盖文件；无密码哈希时
   启动仍走客户端自动生成并显示日志。
10. 高级字段：保存基础配置后字段仍在，注释丢失提示可见。
11. 无效配置：基础错误可修正；高级错误显示 SSH/控制台提示且不写文件。
12. 未知顶层字段：保存被拒绝，文件不变。
13. 启动失败（例如端口冲突）：5 秒内状态未运行，页面返回错误和日志。
14. 连续保存两次：映射 ID 不变，不会触发服务端重新分配端口。
15. YAML 解析失败且服务运行中：仍可从页面停止/禁用服务。
16. `log.file` 非空：页面日志显示该文件末尾内容。

## 8. 发布

- 版本 v1.5.0。
- 中英文 CHANGELOG 和 Release notes。
- 构建所有架构 `.ipk` 和 `luci-app-hy2tun`。
- 生产环境升级客户端/服务端（服务端二进制无功能变化，仍保持版本一致）。
- 更新 OpenWrt 安装文档，说明页面配置流程。
