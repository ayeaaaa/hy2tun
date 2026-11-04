# hy2tun HY2/TCP 双传输与自动回切设计

- 状态：待用户批准完整设计（模式语义已确认）
- 日期：2026-11-04
- 目标版本：v1.6.0

## 1. 已确认需求

用户需要解决客户端网络封锁 UDP 时隧道不可用的问题：

1. 参考 FRP 的 TCP 通道，新增 TCP 传输，而不是把 Hysteria 2 改成 TCP。
2. TCP 和 HY2 共用同一个数字端口，例如 `24443/TCP` 和 `24443/UDP`。
3. 客户端和服务端都有 `hy2`、`tcp`、`hy2+tcp` 三种模式开关。
4. 客户端的每个 `servers:` 条目独立选择模式，互不影响。
5. `hy2+tcp` 优先 HY2，UDP 不可达时兜底到 TCP；UDP 恢复后自动回切。
6. TCP 兜底期间，TCP 映射和 UDP 映射都继续通过 TCP 隧道传输。
7. 保持旧单服务端配置兼容；不自动迁移配置、不擅自开启生产 TCP 端口。

## 2. 实现者建议与非目标

实现者建议（需要随全文批准）：

- 采用普通 TLS/TCP + `github.com/fatedier/yamux`，复用成熟的多路复用库。
- TCP 通道必须使用 TLS 1.3，复用现有服务端证书和客户端 CA/SNI/pin 设置。
- 第一版自动切换按重新建立会话处理，已有 TCP 长连接会断开；UDP 会话重建
  时可能丢包。业务需要自行重连。不宣称不中断迁移或 exactly-once。
- 用户可从两端 Web 控制台修改模式；OpenWrt 单服务端 LuCI 表单也增加模式。
- 后台恢复探测、防抖阈值使用有文档的默认值，不先添加大量调参表单。

非目标：FRP 协议兼容、未加密 TCP、WebSocket/HTTP/2 伪装、P2P、Realms、
UDP hole punching、负载均衡、跨服务端迁移、无缝搬迁现有连接。

## 3. 模式与兼容性

| 服务端 mode | 监听 | 客户端 mode | 行为 |
| --- | --- | --- | --- |
| `hy2` | UDP | `hy2` | 现有行为 |
| `tcp` | TLS/TCP | `tcp` | 只走 TCP，不探测 HY2 |
| `hy2+tcp` | UDP + TLS/TCP | `hy2+tcp` | HY2 优先、TCP 兜底、自动回切 |

上述三行不是唯一允许组合；只要两端有共同传输即可：客户端 `hy2`/`tcp` 可以
连接双模式服务端；双模式客户端也可以连接仅 HY2 或仅 TCP 的服务端。
强制 `hy2` 客户端对仅 `tcp` 服务端（反之亦然）不会改用另一种传输。
无法连接时保留错误；仅在服务端实际返回能力信息时显示模式不兼容，不能凭
UDP 超时推断服务端配置。

新增 `mode` 位于服务端顶层、旧单服务端客户端顶层、多服务端 `servers[].mode`。
缺失或空值的有效模式为 `hy2`；省略字段不在普通保存时被强制写成新格式。
非法模式被配置校验拒绝。多服务端配置不允许额外使用顶层客户端 `mode`。

```yaml
# server.yaml
mode: hy2+tcp
listen: ":24443"
# auth、control、tls、ports、web 等保留现有配置
```

```yaml
# client.yaml（多服务端）
web:
  listen: "0.0.0.0:7501"
  username: admin
  password_hash: "<现有 bcrypt hash>"
servers:
  - name: us
    mode: hy2+tcp
    server: "us.example.com:24443"
    auth: "<HY2 认证>"
    control:
      token: "<control token>"
    client:
      name: home-us
    mappings: []
  - name: sg
    mode: tcp
    server: "sg.example.com:24443"
    auth: "<HY2 认证>"
    control:
      token: "<control token>"
    client:
      name: home-sg
    mappings: []
```

TCP 端口从 `server` 的第一个端口项起始端口派生；不强制 443，不新增必须填写
的地址。HY2 端口跳跃在 `hy2`/`hy2+tcp` 中继续可用；双模式服务端只在其
`listen` 第一个起始端口上监听 TCP，并继续管理原有 UDP redirect。
`tcp` 模式不支持端口范围/UDP 跳跃参数，配置含范围或跳跃设置时拒绝，而不是
默默忽略。测试用 `:0` 需在双模式绑定时保证 TCP 使用 UDP 实际分配到的数字端口。

同数字端口不等于与其他 TCP 服务共享端口：若 HTTPS、FRP 或 TCP 映射已占用
该 TCP 端口，开启 TCP 模式失败并保留旧模式。服务端保留隧道监听端口，不能
分配为公网映射。云安全组和主机防火墙必须另外允许对应 TCP 端口。

## 4. 传输抽象与复用

新增项目内部传输接口：建立会话、打开控制流、打开 TCP 数据流、打开数据报
会话、关闭、报告实际传输。HY2 适配器封装现有 `hyclient.Client`；TCP 适配器
使用 TLS/yamux。不要为了少改代码而把 TCP 通道伪装成第三方 Hysteria 实现。

客户端 worker 保持原有映射、凭据、持久化和重连职责；其网络创建/关闭、
`handleOpen`、UDP 读写和 `TCPContext` 改走统一接口。Supervisor 仍拥有一个
共享 Web 控制台和多个独立 worker。

服务端保持一套 mapping manager、state store、端口策略、凭据存储和 Web
管理台。两种传输共用这套逻辑，不创建两个彼此抢端口的独立服务端。

现有本地 SOCKS5 TCP CONNECT、HTTP CONNECT 同样使用当前传输：TCP 模式下
通过独立 yamux TCP-egress 流请求服务端拨号。权限与现有 HY2 代理一致，不能
绕过认证。标准 Hysteria 客户端仍只使用 HY2 监听；TCP 通道不是 FRP 或标准
Hysteria TCP 协议。

`obfs` 和 QUIC 参数只作用于 HY2；TCP 使用 TLS，不套 Salamander。TCP 模式
不具有 HY2 的 Brutal/BBR 行为。现有 bandwidth 上下行字节速率限制需要在 TCP
适配层兑现（复用维护良好的速率限制库），不能静默取消；QUIC 特有配置不会被
解释为 TCP 设置。

## 5. TCP 会话与安全

1. 客户端连接服务端同数字端口的 TCP listener。
2. 完成 TLS 1.3 握手，遵守现有 CA、SNI、insecure、证书 pin。
3. 在 TLS 内交换版本化、长度受限的 TCP greeting：协议标识、版本、HY2
   认证、control token；服务端验证现有 current/previous 凭据。认证失败关闭。
4. 认证成功才创建 yamux 会话和应用流；首个控制流继续使用现有 Hello /
   HelloAck、映射注册、update、push、轮换等语义。
5. 数据流使用有限的类型枚举与长度受限元数据：控制、反向 TCP、UDP 映射、
   TCP 代理；无法识别的流/版本拒绝，不能当成任意外网地址拨号。

不在日志、URL、metrics、错误信息中输出认证/token。TCP greeting 不超过
64 KiB；TLS + greeting 总握手有截止时间。所有 TCP 数据流必须归属于已认证
会话。两类 token 区分处理，沿用现有语义：

- 反向 TCP data token 是 128-bit 随机、短时过期、单次认领的 token。
- UDP mapping token 是 128-bit 随机、映射生命周期内可重复使用的 token，
  用于 probe/keepalive/UDP 会话重建；它不是每个数据报的一次性 token。
  映射删除、替换或正式模式切换后旧 token 失效，新 token 由 HelloAck /
  UpdateAck 返回；不因单条 TCP UDP stream 重连而强制一次性消耗。

认领时核验映射归属及存活会话，不能跨客户端或跨已废弃会话使用。不改变旧 HY2
客户端的 UDP token 重用方式。
rotation/revoke 对 TCP 的新握手和控制流同样生效。

TLS 探测或 fallback 不能降低证书安全：明确的认证失败、pin/CA/证书错误、
协议不兼容属于配置/安全错误，不作为“UDP 封锁”自动降级。网络超时、连接拒绝、
网络不可达和已有会话失活才允许兜底。

限制 TLS 待认证连接、yamux backlog/逻辑流数、UDP 帧与队列，沿用现有映射/
TCP/pending token 上限。应用背压必须有上限和取消机制，不能无限缓存，也不能
因一个 UDP stream 写不动而堵住控制流。所有新 goroutine、探测与排队写在
session close、配置变更、worker Close 时退出。

## 6. UDP over TCP

每个 UDP 映射使用独立 yamux stream，帧包括数据报长度、peer 地址长度、peer
地址与 payload。接收用长度校验 + `io.ReadFull`，保留数据报边界，不能简单
`io.Copy`。返回流量按 peer 分发，保留当前本地 peer socket / idle 清理逻辑。

有效 payload 延用现有不超过 4096 字节的预算；peer 元数据限制为 256 字节；
固定头以外的帧长度不得超过这两个上限之和。队列每映射最多 32 个待发送数据报。
溢出丢弃并累计 drop counter，不能无限占内存。畸形/超长帧关闭该流，不分配
攻击者指定的大缓冲。发送/接收双方校验映射 token 与会话所有者。

TCP 丢包时会有队头阻塞，UDP 的延迟和抖动可能增加；这是封锁 UDP 场景的
可用性兜底，不是实时性能优化。切换时不重放旧队列，避免过时、重复数据报。

## 7. 自动 failover / failback

以下是实现者建议的默认值，使用常量、测试时注入时钟；第一版不做自动参数 UI：

- HY2 或 TCP 建连（网络连接、传输认证和控制 HelloAck）总预算：10 秒。
- 应用控制心跳：每 10 秒一次；30 秒无回应则关闭失活会话并重连。
- TCP 工作期间每 30 秒探测 HY2，间隔带 ±20% 随机抖动。
- 回切要求 3 次连续成功的认证探测，且 TCP 已持续至少 60 秒。
- 一次失败清零成功计数。失败后重试使用现有指数退避并增加随机抖动。

状态逻辑：

1. 强制 `hy2` 仅建 HY2；强制 `tcp` 仅建 TCP。
2. 自动模式启动先建 HY2；可兜底的网络失败后立即尝试 TCP。
3. HY2 活跃时不维护长期 TCP 备用会话；确认失活后尝试 TCP。
4. TCP 活跃时后台 HY2 探测不触碰当前 TCP、映射或配置。
5. 达到恢复阈值后建立一个新的已验证 HY2 候选会话，再进行重新注册切换。
6. 模式/认证/目标发生改变时取消旧探测，废弃旧结果；探测完成必须复核当前
   worker 的配置版本和凭据代际。
7. 在 TCP 活跃期间不会为了探测而先关闭 TCP；候选验证失败保持 TCP 工作。
8. 切换期间控制更新/轮换/本地配置修改序列化；新注册使用最新映射/revision，
   不能用探测开始时的旧快照覆盖本地配置。

新增独立的只读 `probe` 虚拟通道：验证 HY2 认证、control token、协议能力，
回应 nonce 和服务端能力；不发送普通 Hello、不注册 client ID，不分配端口，
不写 state。服务端限制探测握手数、帧长度和截止时间。新 HY2 server 支持
probe，TCP greeting 也返回模式能力；旧 HY2 server 不支持 probe 不影响旧
`hy2` 客户端的普通连接。

第一版沿用同 client ID 正式重新注册的生命周期：切换会断开该 worker 的旧
TCP 长连接/代理连接，重建 UDP 会话；其他 worker 和共享 Web 会话不受影响。
保留映射 ID、local_addr 和已分配 remote_port，尝试复用原公网端口；若端口
被其他程序占用，按原有规则报错，不悄悄迁移已指定端口。不承诺零丢包/零间隙。
回切的最终 HelloAck/映射注册若失败，重新建立 TCP 并报告失败；不能丢失配置。

## 8. 模式开关与状态

- 服务端 Web 设置页：全局 mode。客户端 Web：每个服务端独立 mode；旧单
  服务端页面操作 `default`。多服务端列表显示配置模式和实际传输。
- 模式变更属于敏感操作，沿用会话认证、CSRF、当前 Web 密码确认与限流。
- 客户端先校验/持久化，成功后仅重新配置受影响的 worker；不关闭共享 Web。
  若新模式允许当前实际传输，保持连接并调整自动探测策略；否则重连。
- 服务端开启新增监听先 pre-bind，再持久化，再开始接收；绑定/校验/保存失败
  保持旧模式。关闭某协议停止接收并关闭该协议会话；其他协议和 Web 保持。
- 服务端 mode 不自动改客户端 mode；页面提示检查兼容性及 TCP 防火墙。
- LuCI 单服务端表单增加 mode；继续停止后保存再启动，不改多服务端只读策略。
- 模式变更提示可能中断现有业务连接；不做浏览器 mockup/整体布局重设计。

新增只读状态：`mode`（配置模式）、`active_transport`（`hy2`/`tcp`/空）、
`switch_reason`、`last_switch_at`、`last_probe_at`、`last_probe_error`、
`switch_count`。服务端客户端详情按实际入站通道显示 transport，不信任客户端
自报值；离线显示最后状态/未知，不虚报在线。
metrics 添加传输状态与切换计数；错误不得含凭据。不把“进程运行”当成“隧道
可用”。TCP 模式下界面明确提示 UDP over TCP 的延迟和队头阻塞代价。

## 9. 代码边界

- `internal/config`：mode 类型、默认/合法值、端口派生、与端口跳跃互斥规则；
  single/multi clone、保存、BasicUpdate 保留/编辑 mode。
- `internal/transport`（新增）：公共接口、HY2 适配、TLS/yamux 适配、帧、限流、
  受限 greeting 和认证错误类别；依赖版本固定，不自写 multiplexer。
- `internal/client`：worker 状态机、探测/心跳、代理/UDP/TCP 适配、并发变更。
- `internal/server`：双 listener 生命周期、共用控制/映射、probe、TCP 适配、
  认证归属和模式在线应用。
- `internal/webui` / `internal/view`：模式控制、状态、metrics，复用现有页面。
- `scripts/openwrt`：mode 表单/CLI 兼容和包验证。

实现者选定具体库版本、内部方法签名和文件拆分，实施计划固定后再编码。

## 10. 验证与发布门槛

- 旧 YAML 省略 mode：行为和公网映射保持；旧客户端可连新 HY2/双模式服务端。
- 3×3 模式组合：允许组合可连接，不允许组合明确失败不私自改模式。
- TCP-only 环境：控制注册、TCP/UDP 映射双向、SOCKS5/HTTP CONNECT、凭据
  轮换、映射 push/删除和状态显示均验证。
- 同数字 UDP/TCP 监听、TCP 端口占用、mode pre-bind/保存失败回滚。
- UDP 被阻断后的 TCP failover、UDP 恢复后的 failback；全程探测不会踢掉
  TCP 会话、增加 state 客户端或抢映射端口；抖动/延迟探测不无限切换。
- 并发 mode 修改、凭据轮换、Close、本地/服务端映射编辑：不泄漏 goroutine/
  token/FD，不覆盖新配置；在 race detector 下测试。
- TLS 校验失败/旧凭据撤销/恶意 greeting/数据流/数据报不得绕过认证。
- TCP 多流背压/UDP 队列溢出/最大帧/拆包黏包/超时都有自动化测试；吞吐和
  延迟用本地丢包/限速实验测量，不把 TCP 兜底说成性能提升。
- Linux 本地网络命名空间或可控测试端点注入 UDP 封锁；不要直接封生产 UDP。
- OpenWrt amd64 真机和其他发布架构静态构建/QEMU 检查；Windows 构建通过。
- `go test ./...`、race、vet、staticcheck、controller/package tests；CI、
  中英文 README/CHANGELOG/Release notes 和全部资产。
- 先升级服务端保持 `hy2`，再升级客户端；确认旧模式后经用户允许启用双模式
  和同数字 TCP 防火墙。保留二进制/配置备份，不迁移现有映射和凭据。

## 11. 已检查的复用方案

- FRP 的 `client/connector.go` 使用 TLS/TCP 和 `fatedier/yamux` 来分离底层
  连接与逻辑流，适合本项目参考；不复制其整个客户端/控制协议。
- FRP 官方 `go.mod` 当前引用 `github.com/fatedier/yamux v0.3.0`；实施时复核
  维护状态及兼容性并 pin 版本，不使用浮动 `latest`。
- 参考源码：
  - https://github.com/fatedier/frp/blob/dev/client/connector.go
  - https://github.com/fatedier/frp/blob/dev/go.mod
  - https://github.com/fatedier/yamux
- 现有 source 已确认 `mappingManager.register` 会关闭同 ID 的旧会话，且
  `runOnce` 直接绑定 HY2；因此独立 probe 和传输抽象是必需部分，不能只添加
  配置开关就声称 TCP/无缝回切已实现。
