# HY2/TCP Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 hy2tun 增加同数字端口的 TLS/TCP 传输、多服务端三模式开关、UDP over TCP，以及 HY2 优先的自动兜底和恢复回切。

**Architecture:** 一套控制/映射/持久化逻辑承载两种底层传输；客户端每个 worker 独立选择并维护会话。TCP 使用 TLS 1.3 和成熟 yamux，UDP 数据报封装在独立流中；恢复探测不注册客户端。自动切换重新注册会话，不迁移已有业务连接。

**Tech Stack:** Go 1.26、现有 Hysteria 2 core/extras、`github.com/fatedier/yamux v0.3.0`、`golang.org/x/time v0.16.0`、yaml.v3、testify、现有 Web UI/LuCI。

**Spec:** `docs/superpowers/specs/2026-11-04-tcp-fallback-design.md`

## Global Constraints

- 目标版本 `v1.6.0`；模式字符串只有 `hy2`、`tcp`、`hy2+tcp`。
- 缺失/空 mode 的有效模式是 `hy2`；用 `yaml:"mode,omitempty"` 保留旧 YAML 形状，不默认写出新字段。
- 服务端顶层 mode；单服务端客户端顶层 mode；多服务端 `servers[].mode`，不允许多服务端额外设置顶层 mode。
- 普通地址同数字端口，例如 `24443/UDP` 和 `24443/TCP`；不强制 443，不与已有其他 TCP 服务共享 socket。
- 双模式端口跳跃时 TCP 使用第一个端口项起始端口；纯 `tcp` 配置不允许端口范围/跳跃设置。
- TCP 必须 TLS 1.3，沿用 CA/SNI/insecure/pin；禁止自动降低证书安全、明文 TCP、WebSocket/HTTP 伪装。
- TCP 同时承载反向 TCP、UDP 映射和当前本地 SOCKS5/HTTP CONNECT；标准 Hysteria 客户端仍使用 UDP。
- TCP greeting ≤64 KiB；UDP payload ≤4096 字节、peer 地址 ≤256 字节；每映射待发送队列 ≤32 个数据报。
- 保留 current/previous 认证代际；反向 TCP token 单次且有 TTL，UDP token 可在存活映射中复用；TCP 新流必须绑定真实认证会话/映射所有者。
- 建连到控制 HelloAck 总预算 10 秒；控制心跳 10 秒；30 秒无回应关闭失活会话。
- TCP 期间 HY2 探测周期 30 秒、±20% jitter；3 次连续成功且 TCP 已保持至少 60 秒才回切；失败清零计数。
- 安全/认证/协议错误不触发降级；网络超时、拒绝、不可达、已建立会话失活才触发。
- 独立 probe 不发送普通 Hello、不注册 client ID、不分配端口、不写 state、不影响活跃 TCP。
- 切换只影响对应 worker；已有 TCP/代理连接会断开，UDP 可能丢包，不重放队列，不承诺无缝或 exactly-once。
- mode 在线变更复用登录、CSRF、当前 Web 密码确认和限流；服务端新增监听 pre-bind → persist → serve，失败保持旧模式。
- 不取消现有带宽限制；TCP 采用字节速率限制，不声称拥有 HY2/Brutal/BBR 的效果。
- 不改生产配置/防火墙，不在生产注入 UDP 封锁；发布、生产升级和开启双模式分别按授权执行。
- 双语 README/CHANGELOG/Release notes；Go test/race/vet/staticcheck、LuCI/package tests、Linux/Windows/其他发布架构检查。

## Review Focus

- 证书/pin/认证错误不能被误分类为 UDP 封锁；自动模式必须保留安全错误且不拨 TCP（Task 2、7）。
- 凭据轮换、mode 修改、Close 与后台探测并发时，旧探测结果不能覆盖新配置或踢掉当前会话（Task 7、8）。
- 恶意长度、黏包/拆包、慢接收方不能导致大缓冲、无限队列或控制流被阻塞（Task 3、11）。
- TCP listener 被占用、持久化失败时服务端 mode 变更必须回滚，不能停止仍工作的 HY2/Web（Task 5、8）。
- 自动切换期间保持映射定义/端口与多服务端隔离；不能虚报隧道可用或把恢复 probe 注册成第二个客户端（Task 4、7、11）。

---

## 文件与依赖边界

新增文件按职责拆分：`internal/config/mode.go`；`internal/transport/` 的接口、TLS、HY2 适配、TCP greeting、yamux、stream、数据报、速率限制；`internal/server/tcp_transport.go`、`probe.go`、`listeners.go`、`mode.go`；`internal/client/session.go`、`failover.go`、`heartbeat.go`、`mode.go`；两端 `handlers_mode.go`。

每个 Task 在独立分支上产生可测试提交；中间提交不单独发布。Task 1–6 建立基础通道；Task 7–8 加自动选择和在线应用；Task 9–10 接界面；Task 11–12 验证兼容与准备发布。

### Task 1: mode 配置、默认行为与 TCP 地址派生

**Files:**
- Create: `internal/config/mode.go`, `internal/config/mode_test.go`
- Modify: `internal/config/client.go`, `internal/config/server.go`, `internal/config/servers.go`

**Interfaces:**
- `type TransportMode string`；常量 `ModeHY2="hy2"`、`ModeTCP="tcp"`、`ModeAuto="hy2+tcp"`。
- `func (m TransportMode) Effective() TransportMode`、`Validate() error`、`AllowsHY2() bool`、`AllowsTCP() bool`。
- `ClientConfig.Mode`、`ClientServerConfig.Mode`、`ServerConfig.Mode`：`yaml:"mode,omitempty" json:"mode,omitempty"`。
- `func (c *ClientConfig) TCPServerAddress() (string, error)`：由 server 第一个范围起始端口派生，不排序后取最小值。
- `func (c *ServerConfig) TCPListenAddress() (string, error)`：由 listen 第一个范围起始端口派生。

- [ ] **Step 1: 写配置 RED 测试**

`TestTransportModeDefaultAndAllows`：
```go
require.Equal(t, ModeHY2, TransportMode("").Effective())
require.True(t, ModeAuto.AllowsHY2())
require.True(t, ModeAuto.AllowsTCP())
require.False(t, ModeTCP.AllowsHY2())
require.Error(t, TransportMode("invalid").Validate())
```
`TestTCPAddressUsesFirstSpecifiedPort` 断言 `host:30002-30005,30000` → `host:30002`；IPv6 保留括号。`TestModeLegacyYAMLNotMaterialized` 加载/保存无 mode 文件后不含 `mode:`。`TestModeMultiIndependent` 两项分别 auto/tcp，ToClientConfig 和 ServerConfigs 保持各自 mode，顶层 mode 非空拒绝。`TestTCPModeRejectsHopping` 覆盖纯 tcp 范围/跳跃参数拒绝，hy2/auto 保持旧支持。

- [ ] **Step 2: 运行 RED**：`go test ./internal/config -run 'TestTransportMode|TestTCPAddress|TestMode|TestTCPMode' -count=1`；应因接口不存在而失败。
- [ ] **Step 3: 实现接口并在单/多客户端、服务端 Validate 中接入**。不要在 applyDefaults 中把空 mode 改成非空；所有使用方读取 Effective。更新 legacyFieldsSet 与转换/clone。对纯 tcp 检查输入是否含范围或 hop 设置，不能只看范围归一后端口数。
- [ ] **Step 4: GREEN**：上述命令及 `go test ./internal/config -count=1` 全部通过。
- [ ] **Step 5: Commit**：`feat(config): add HY2 TCP transport modes`。

### Task 2: 传输契约、HY2 适配与 TLS/错误分类

**Files:**
- Create: `internal/transport/transport.go`, `errors.go`, `tls.go`, `hy2.go`, `transport_test.go`, `tls_test.go`, `hy2_test.go`
- Reference: `internal/client/client.go:878-915`, `internal/client/control.go:147-242`

**Interfaces:**
```go
type Kind string // HY2="hy2", TCP="tcp"
type TCPRequest struct { ReverseToken string; Target string } // 恰好一个非空
type DatagramConn interface {
    Receive() ([]byte, string, error)
    Send([]byte, string) error
    Close() error
}
type ProbeInfo struct { Version int; Nonce string; Mode config.TransportMode; ServerVersion string }
type Session interface {
    Kind() Kind
    OpenControl(context.Context) (net.Conn, error)
    OpenTCP(context.Context, TCPRequest) (net.Conn, error)
    OpenUDP(context.Context, string) (DatagramConn, error) // UDP mapping token
    Probe(context.Context) (ProbeInfo, error)
    Close() error
}
type DialOptions struct {
    HY2 *hyclient.Config
    TCPAddress string
    TLS *tls.Config
    Auth, ControlToken, ClientID string
    Up, Down uint64 // 字节/秒，0 无限
}
type HY2Factory func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error)
```
- `DialHY2(ctx context.Context, opts DialOptions, factory HY2Factory) (Session,error)`。
- `WrapHY2(client hyclient.Client, controlToken string) Session`（现有 fake 测试的适配入口）。
- `ClientTLS(config.ClientTLSConfig, server string) (*tls.Config,error)`；共享 CA/SNI/pin 逻辑，TCP TLS 最低 1.3。
- `Error{Code string; Cause error}`，实现 Error/Unwrap；`FallbackAllowed(err error) bool`；导出 `ErrAuthentication`、`ErrProtocol`、`ErrCertificatePin`、`ErrQueueFull`。

- [ ] **Step 1: RED 测试**。`TestHY2AdapterVirtualAddresses` 用现有 fake HY2 验证控制/反向 TCP/UDP 地址路径，不创建新协议实现；非法 TCPRequest 拒绝。`TestClientTLSPreservesCAAndPin` 用 testutil.SelfSignedCert 验证 CA、SNI、pin、一张错误证书拒绝；TLS 最低版本为 1.3。`TestFallbackAllowed` 对 Hysteria core 的 AuthError/ConfigError/ProtocolError、x509.CertificateInvalidError、pin sentinel 返回 false，对 net.OpError 超时/连接拒绝、context.DeadlineExceeded、io.EOF 返回 true；context.Canceled/未知普通 error 返回 false。
- [ ] **Step 2: RED**：`go test ./internal/transport -count=1`；应失败。
- [ ] **Step 3: 实现适配器/错误与 TLS helper**。用 errors.As/Is，不用匹配含密码的 error 文本。HY2 调用的取消必须关闭已创建 packetConn/session，及时清理未交付的连接；Probe 的帧方法到 Task 3 定义后接入。此任务可只验证 unsupported-probe 错误，不注册客户端。
- [ ] **Step 4: GREEN**：`go test ./internal/transport -count=1`；取消/Close 后未交付 HY2/TCP conn 都被关闭；测试不依赖远程网络。
- [ ] **Step 5: Commit**：`refactor(transport): introduce session and HY2 adapter`。

### Task 3: TLS/yamux 会话、有限帧和 UDP over TCP

**Files:**
- Modify: `go.mod`, `go.sum`, `internal/transport/hy2.go`
- Create: `internal/transport/wire.go`, `tcp.go`, `stream.go`, `datagram.go`, `rate.go` 及各自 `_test.go`

**Interfaces:**
- pin `github.com/fatedier/yamux v0.3.0` 和 `golang.org/x/time v0.16.0`，不升级其他现有依赖。
- `Greeting{Magic,Version,Auth,ControlToken,ClientID,Up,Down}`；Magic=`hy2tun-tcp`，Version=1。
- `GreetingAck{OK,Version,Mode,ServerVersion,Error}`；错误值只允许 `unauthorized`、`unsupported_protocol`、`bad_message`。
- `Principal{ClientID string; Generation int64; TokenDigest [32]byte}`，不保存可回显的明文 token。
- `TCPAuthFunc func(context.Context, net.Addr, Greeting) (Principal,error)`。
- `RateLimits{Up,Down uint64}`；`TCPServerOptions{TLS *tls.Config; Authenticate TCPAuthFunc; Mode config.TransportMode; Version string; Limits RateLimits}`。
- `DialTCP(ctx context.Context, opts DialOptions) (Session,error)`。
- `AcceptTCP(ctx context.Context, conn net.Conn, opts TCPServerOptions) (*TCPPeer,error)`。
- `TCPPeer.Principal() Principal`、`Accept(ctx) (StreamHeader,net.Conn,error)`、`Close() error`。
- `StreamHeader{Kind uint8; Token,Target string}`，Kind=1 control、2 reverse_tcp、3 udp、4 tcp_proxy；`WriteStreamAck(conn net.Conn, err error) error` 返回 OK 或安全错误，不回显 token/target。
- `ProbeRequest{Version int; Token,Nonce string}`、`ProbeReply{OK bool; Error string; Info ProbeInfo}`。
- `WriteProbeRequest(io.Writer,ProbeRequest) error`、`ReadProbeRequest(io.Reader) (ProbeRequest,error)`、`WriteProbeReply(io.Writer,ProbeReply) error`、`ReadProbeReply(io.Reader) (ProbeReply,error)`，均使用64KiB有限JSON frame；nonce为随机16字节的32字符hex。

Wire键沿用项目snake_case：Greeting使用`magic/version/auth/control_token/client_id/up/down`；StreamHeader使用`kind/token/target`。TCPServerOptions.Limits由服务端当前bandwidth填入，TCP adapter不能只限制客户端方向。

- [ ] **Step 1: RED 测试**。`TestTCPGreetingTLSAndAuthentication` 验证 TLS 1.2/错误 auth/错误 token/协议版本失败，成功后才建立 mux。`TestDatagramFrameBoundaries` 用分段 reader 验证黏包拆包、peer/payload 边界；`TestDatagramRejectsOversize` 声称 1 GiB 长度应在读 payload 前拒绝。`TestTCPUDPQueueBounded` 满32队列返回 ErrQueueFull，不关闭控制流。`TestTCPStreamAckRejectsBadKind` / `TestTCPStreamCancelClosesPending` / `TestTCPRateLimitCancels` 覆盖错误 header、取消和限流。
- [ ] **Step 2: RED**：`go test ./internal/transport -run 'TestTCP|TestDatagram' -count=1`。
- [ ] **Step 3: 实现 root TLS greeting 和 mux**。有限 JSON 使用 4 字节 big-endian 长度，先验证 limit 再分配；greeting 最大65536、stream header/ack最大4096。所有新流等待 application ack 后才交给业务。配置 yamux：AcceptBacklog=256、MaxStreamWindowSize=1 MiB、StreamOpenTimeout/ConnectionWriteTimeout=10s、KeepAliveInterval=10s；主动并发逻辑流最多1024。TLS+greeting遵守调用方截止时间；认证前不创建mux。服务器端所有新流交给 TCPPeer.Accept，由上层控制顺序与归属。
- [ ] **Step 4: 实现数据报帧和限流**。数据报为 `[uint32 frameLen][uint16 addrLen][addr][payload]`，frameLen包括addrLen但不含uint32；payload≤4096、addr≤256、frameLen≤4354。每映射发送队列32；队列满 ErrQueueFull，stream关闭丢弃队列并释放等待者。HY2 adapter Probe 用 `_hy2tun_probe_:0` + 有限 ProbeRequest/Reply，不调用 Hello。上下行共享 session 级 rate.Limiter（0无限，burst/chunk=16KiB），只限制 TCP 数据/数据报，不限制控制/认证，等待使用ctx。客户端限额与服务端自身限额分别执行，不静默取消任何一方配置。
- [ ] **Step 5: GREEN**：`go test -race ./internal/transport -count=1`通过。
- [ ] **Step 6: Commit**：`feat(transport): add authenticated TLS yamux transport`。

### Task 4: 服务端 TCP/probe 处理与映射归属

**Files:**
- Create: `internal/server/tcp_transport.go`, `probe.go`, `tcp_transport_test.go`, `probe_test.go`
- Modify: `internal/server/control.go`, `virtual.go`, `tcp.go`, `udp.go`, `mapping_runtime.go`, `mapping_session.go`, `mapping.go`, `rotation.go`

**Interfaces:**
- `authenticateTCP(ctx,addr,greeting) (transport.Principal,error)`：使用 current/previous HY2 auth 与 control token。
- `serveTCPPeer(ctx context.Context, peer *transport.TCPPeer) error`。
- 保留 `controlHandler.Serve(net.Conn)` 作为 HY2 入口；内部新增 `serveWithMeta(conn net.Conn, meta sessionMeta)`；`sessionMeta{Kind transport.Kind; Principal *transport.Principal; Peer *transport.TCPPeer; CloseTransport func() error}`。
- `clientSession.transport transport.Kind`、`clientSession.tcpPeer *transport.TCPPeer`；每次正式会话有独立对象，不只比较 client ID。
- `dataStreams.takeFor(token string, owner *clientSession) (net.Conn,error)`；pendingConn 记录 owner；原 take 用于旧 HY2入口。
- `mappingManager.udpRuntimeFor(token string, owner *clientSession) (*mappingRuntime,error)`，原 udpRuntime 保留旧 HY2 bearer-token路径。
- `serveProbe(conn net.Conn)`；响应版本/nonce/模式，不注册客户端。

首个控制注册10秒内未完成则关闭TCP root。查找数据/UDP映射时必须同时匹配当前live owner和`owner.tcpPeer == peer`，不能仅凭ClientID。认证回调和greeting能力取握手时currentConfig/当前凭据，不使用endpoint启动时的陈旧mode。所有新greeting/stream错误回复保持有限错误枚举，不返回秘密值。

- [ ] **Step 1: RED 测试**。`TestTCPPeerRequiresControlFirst` 认证后未完成控制注册前，数据/UDP/代理流拒绝；同root只允许一个control。`TestTCPDataTokenWrongOwnerDoesNotConsume`、`TestTCPUDPTokenWrongOwnerRejected`：错误会话/旧会话不能消费有效 token；正确会话随后成功。`TestProbeDoesNotRegisterOrPersist`：重复 probe 前后 Clients/状态文件/public listener 不变。`TestTCPRevokeClosesOldRoot`：撤销旧代际关闭TCP根连接及子流。`TestTCPUserpassAuth` 复用现有 userpass，不改变其不能在线轮换规则。
- [ ] **Step 2: RED**：`go test ./internal/server -run 'TestTCPPeer|TestTCPData|TestTCPUDP|TestProbe|TestTCPRevoke|TestTCPUserpass' -count=1`。
- [ ] **Step 3: 实现认证和流路由**。control Hello.ClientID 必须匹配root身份，Hello.Token digest匹配认证token，注册时再次核验当前凭据。root控制关闭时关闭整个peer。反向TCP认领/UDP查询在manager锁内验证live owner；tcp_proxy只在完成control后拨真实地址，拒绝所有 `_hy2tun_` 保留地址。旧 HY2 Outbound 没有调用方身份参数，保留已有128-bit bearer-token安全模型，不凭空信任新增自报client ID。
- [ ] **Step 4: 实现probe/UDP桥与清理**。probe最大64KiB、nonce必须匹配、10s deadline、并发上限32，鉴权后只读响应。UDP rootstream桥接同一个publicUDPConn，虚拟keepalive仍被丢弃；每个stream队列32，超限计数，双向都保留peer地址。root revoke/模式关闭/worker重连时释放peer、流、socket引用。正式映射换会话才更换UDP token。
- [ ] **Step 5: GREEN**：上述测试、`go test -race ./internal/server -count=1`通过。
- [ ] **Step 6: Commit**：`feat(server): route TCP sessions and isolated HY2 probes`。

### Task 5: 服务端三模式启动与双 listener 生命周期

**Files:**
- Create: `internal/server/listeners.go`, `listeners_test.go`
- Modify: `internal/server/server.go`, `state.go`, `control.go`, `server_test.go`

**Interfaces:**
- 内部 `endpoint interface { Kind() transport.Kind; Addr() net.Addr; Serve(context.Context) error; Close() error }`。
- `prepareEndpoint(kind transport.Kind, cfg *config.ServerConfig, bindAddr string) (endpoint,error)`；HY2拥有其packetConn/firewall cleanup，TCP使用Task4 handler。
- `Server.ListenAddresses() map[transport.Kind]string`；保留 ListenAddr：HY2有则返回UDP，否则返回TCP，用于原调用方地址派生。
- `mappingManager.detachTransport(kind transport.Kind)`：只关闭该传输会话，不关闭其他传输/共享Web。

TCP listener在启动处理goroutine前限制待认证连接64个，已认证root最多256个；控制客户端准入在manager锁内执行。加入`TestTCPHandshakeAdmissionBounded`，验证超额连接被关闭且不启动无限goroutine；释放已有slot后新的握手仍可成功。

- [ ] **Step 1: RED 测试**。`TestServerModeListenerMatrix` 断言 hy2只UDP、tcp只TCP、auto两者；auto `127.0.0.1:0`两个实际数字端口相同。`TestDualStartupTCPBindFailureCleansUDP` 第二个bind失败不残留第一个socket。`TestTCPModeDoesNotConstructHY2` 不创建UDP/firewall/Hysteria服务对象。`TestTunnelPortReservedAcrossTransports` 映射不可占用隧道listen数字端口。
- [ ] **Step 2: RED**：`go test ./internal/server -run 'TestServerMode|TestDualStartup|TestTCPModeDoes|TestTunnelPort' -count=1`。
- [ ] **Step 3: 重构 New/Start/Close**。先验证/load cert，建立共享credentials/state/mappings，绑定所需endpoint但不接收应用数据；开始Serve时两种listener共用control/mapping manager。绑定失败清理已创建资源；纯TCP不执行HY2依赖。endpoint错误/Close按身份注销，不调用共享Server.Close作为单endpoint清理。Start仍阻塞；任何必需listener启动失败返回错误，不虚报完整模式。实际基础端口固定，后续mode更新不因`:0`再随机换端口。
- [ ] **Step 4: GREEN**：`go test ./internal/server -count=1`、`go test ./internal/e2e -count=1`；原HY2生命周期测试通过。
- [ ] **Step 5: Commit**：`feat(server): support HY2 TCP and dual listeners`。

### Task 6: 客户端使用统一会话，强制模式与现有代理保持

**Files:**
- Create: `internal/client/session.go`, `session_test.go`
- Modify: `internal/client/client.go`, `control.go`, `tcp.go`, `udp.go`, `proxy_apply.go`，相关 `_test.go`

**Interfaces:**
- `Client.session transport.Session` 取代业务对第三方hyclient的直接依赖；保留 `newHY2` 工厂测试注入，通过 WrapHY2适配，不让TCP实现冒充hyclient.Client。
- `dialSession(ctx context.Context, kind transport.Kind, cfg *config.ClientConfig) (transport.Session,error)`。
- `runSession(ctx context.Context, session transport.Session) (connected bool, err error)`：setCurrent、HelloAck、control、UDP及关闭/身份校验共用。
- `currentSession() transport.Session`；TCPContext、handleOpen、startUDP通过Session方法。

HelloAck的unauthorized/unsupported_protocol、错误控制帧要转换为typed认证/协议错误供FallbackAllowed判断，不能仅fmt.Errorf后失去错误类别。加入`TestHelloAckSecurityErrorRetainsClass`，确认拒绝码不会被当成网络失败。

- [ ] **Step 1: RED 测试**。`TestForcedTCPNoUDPDial` 工厂记录kind，ModeTCP不尝试HY2；`TestForcedHY2NeverTCP`旧行为保持。`TestTransportCloseUnblocksUDPAndDials`关闭Session释放UDP send和local dial。`TestOldOwnerCannotTrackDataAfterSwitch`旧控制流data不能挂到新会话。`TestTCPProxyUsesCurrentTransport`本地SOCKS/HTTP TCPContext走当前session。
- [ ] **Step 2: RED**：`go test ./internal/client -run 'TestForced|TestTransportClose|TestOldOwner|TestTCPProxyUses' -count=1`。
- [ ] **Step 3: 实现对Session的调用**。每次根据最新config构建TLS/地址，纯TCP不依赖UDP解析/connFactory。10s建连总ctx传递到dial/OpenControl/HelloAck；已建立控制流不沿用过期的建连ctx。必须让临时session在取消或未交付时Close。保留现有握手配置write-back、token获取和mapping traffic计数。UDP Send遇ErrQueueFull只计数/drop，不把它当整会话失败。
- [ ] **Step 4: GREEN**：`go test ./internal/client ./internal/server ./internal/e2e -count=1`、`go test -race ./internal/client -count=1`；改造现有fake测试使用WrapHY2。
- [ ] **Step 5: Commit**：`refactor(client): route mappings through transport sessions`。

### Task 7: 自动 failover、独立恢复探测与心跳

**Files:**
- Create: `internal/client/failover.go`, `heartbeat.go`, `failover_test.go`, `heartbeat_test.go`
- Modify: `internal/client/client.go`, `session.go`, `control.go`, `configsync.go`, `internal/view/view.go`

**Interfaces:**
- `type Clock interface { Now() time.Time; After(time.Duration) <-chan time.Time }`。
- `type FailoverPolicy struct { ConnectTimeout, HeartbeatInterval, HeartbeatTimeout, ProbeInterval, MinTCPHold time.Duration; RecoverySuccesses int }`。
- `DefaultFailoverPolicy()`：10s/10s/30s/30s/60s/3。
- `RuntimeOptions{Policy FailoverPolicy; Clock Clock; Jitter func(time.Duration) time.Duration; Dial func(context.Context,transport.Kind,*config.ClientConfig)(transport.Session,error)}`；nil成员使用生产默认。
- `NewWithRuntime(cfg *config.ClientConfig, log *slog.Logger, opts RuntimeOptions) (*Client,error)`；New仍用默认；Supervisor测试入口在Task8接入。
- `view.TransportState{Mode config.TransportMode; ActiveTransport string; SwitchReason string; LastSwitchAt,LastProbeAt time.Time; LastProbeError string; SwitchCount uint64}`。
- `Client.TransportState() view.TransportState`。

TransportState字段使用snake_case JSON tag；`view.ClientStatus.Transport view.TransportState`作为内部快照字段，现有HTTP status转换器在Task9输出平铺字段。内部worker保存state，自动切换不改变yaml mode。

心跳TS保持现有int64字段，作为同一控制流内的唯一递增关联值使用；不把远端时间当本地deadline。收到旧会话/非当前已发TS的Pong不能延长新会话寿命，加入`TestHeartbeatLatePongIgnored`。

- [ ] **Step 1: RED 测试**。`TestAutoFallsBackOnNetworkFailure`先HY2再TCP；`TestAutoDoesNotFallbackOnAuthOrTLS`安全错误没有TCP拨号。`TestRecoveryRequiresThreeSuccessesAndHold` fakeclock前2成功不切、3成功但60s前不切、满足才切。`TestProbeFailureResetsRecovery`失败计数清零。`TestRecoveryProbeStaleEpochDiscarded`轮换/mode变化后旧成功不切。`TestProbeDoesNotUseHello` probe假实现一旦OpenControl失败测试。`TestHeartbeatTimeoutClosesSession`30s无Pong结束；正确Pong续期。`TestAutomaticSwitchPreservesLatestMappings`probe期间编辑，回切Hello使用新revision/targets。
- [ ] **Step 2: RED**：`go test ./internal/client -run 'TestAuto|TestRecovery|TestProbe|TestHeartbeat|TestAutomaticSwitch' -count=1`。
- [ ] **Step 3: 实现选择状态机**。auto初次HY2网络失败立即TCP；活跃HY2失活先TCP；TCP失败重新尝试HY2再TCP。重试沿用1s→30s指数上限，加±20% jitter；强制模式不启动另一协议的probe。state只在控制HelloAck有效后标记ActiveTransport；首次连接不算switch，已成功传输变化才增加SwitchCount。
- [ ] **Step 4: 实现独立probe/candidate和心跳**。TCP期间一次最多一个HY2 probe session，每轮成功/失败立即close；达到阈值后新建候选，先用probe验证，失败保持TCP。正式handoff复核`transportEpoch`/mode/credentials，并按最新cfg发送Hello，按批准边界关闭旧业务。心跳复用control.Ping/Pong TS，writer序列化，匹配已发TS；10s周期/30s超时都基于Clock。不在持有configMu时等待长期网络操作；候选HelloAck的write-back仍用现有revision防护。正式控制建连的可兜底网络失败重新拨TCP；安全/认证/协议拒绝不再拨TCP，显示错误。HelloAck整体OK但映射级端口/策略失败不切换传输，仅显示失败映射；不重放UDP队列。
- [ ] **Step 5: GREEN**：`go test -race ./internal/client -count=1`通过，fakeclock测试不等待真实60秒。
- [ ] **Step 6: Commit**：`feat(client): add automatic TCP fallback and HY2 recovery`。

### Task 8: 两端在线 mode 应用与多服务端隔离

**Files:**
- Create: `internal/client/mode.go`, `mode_test.go`, `internal/server/mode.go`, `mode_test.go`
- Modify: `internal/client/supervisor.go`, `configsync.go`, `internal/server/server.go`, `listeners.go`, `state.go`, `internal/view/view.go`, `internal/state/store.go`, `internal/state/store_test.go`

**Interfaces:**
- `Client.UpdateMode(config.TransportMode) error`；`Supervisor.UpdateMode(config.TransportMode) error`代理default worker；多服务端通过Server(name)选中worker调用。
- `NewSupervisorWithRuntime(cfg,*slog.Logger,RuntimeOptions) (*Supervisor,error)`：测试每worker独立注入；NewSupervisor生产用默认。
- `Server.UpdateMode(config.TransportMode) error`，`Server.Mode() config.TransportMode`。
- 客户端Settings快照保留TransportState（Task9固定JSON形状），ClientStatus.Transport返回worker快照。ClientView添加`Transport string`，入站实际kind来自sessionMeta；State.ClientState增加同名可选字段用于离线诊断。

- [ ] **Step 1: RED 测试**。`TestClientModeSaveFailureKeepsTransport`持久化失败不关旧session。`TestModeAutoKeepsAllowedTCPAndStartsProbe`active TCP转auto不立即掉线。`TestSupervisorModeOnlyChangesSelectedWorker`其他worker/Web/mappings/凭据不变；serverFromWorker、persistWorker保持mode。`TestServerModeBindConflictRollback`UDP仍工作，mode未持久化。`TestServerModeSaveFailureClosesNewListener`无新端口泄漏。`TestServerDisablingHY2LeavesTCPAndWeb`仅HY2会话退出。`TestModeUpdateRaceWithRotationAndClose`Close赢后候选资源清理，不覆盖凭据。
- [ ] **Step 2: RED**：`go test ./internal/client ./internal/server -run 'TestClientMode|TestModeAuto|TestSupervisorMode|TestServerMode|TestServerDisabling|TestModeUpdate' -count=1`。
- [ ] **Step 3: 实现mode事务**。客户端在worker configMu中clone/validate/persist/更新mode与epoch，解锁后通知选择loop；允许当前kind则保留，仅调整probe，否则取消重连。Supervisor回写包含Mode，保持worker→supervisor现有锁序，不在Supervisor锁下调用worker变更。
- [ ] **Step 4: 实现服务端事务**。增加modeMu串行化listener变更；读取当前cfg，pre-bind新增endpoint（不能提前Serve），在settingsMu下从最新cfg合并mode并persist，再激活新增/关闭移除。失败关闭候选，旧端点/会话不动。ModeOnly关闭不触碰共享store/Web/mappings全局生命周期。`Close`与UpdateMode共用modeMu，关闭后不启动新endpoint。存活会话actual Transport保存为只读诊断，离线不宣称活跃。
- [ ] **Step 5: GREEN**：`go test -race ./internal/client ./internal/server -count=1`通过。
- [ ] **Step 6: Commit**：`feat: apply transport modes without restarting web consoles`。

### Task 9: Web 模式开关、状态与 metrics

**Files:**
- Create: `internal/webui/clientui/handlers_mode.go`, `handlers_mode_test.go`, `internal/webui/serverui/handlers_mode.go`, `handlers_mode_test.go`
- Modify: 两端 `handlers.go`、`views.go`、`static/app.js`；客户端 `templates/index.html`、`templates/servers.html`；服务端 `templates/settings.html`、`templates/client_detail.html`；`internal/client/supervisor.go`/`client.go`/`internal/server/server.go`的Settings快照

**Interfaces:**
- 两端可选 `ModeAPI interface { UpdateMode(config.TransportMode) error }`，保留现有API接口兼容embedded/fake调用方；未实现返回501，不显示可操作控件。
- `clientui.SettingsView.Transport view.TransportState json:"transport"`；`clientui.ServerSummary`匿名嵌入`view.TransportState`以平铺server列表状态；`serverui.SettingsView.Mode config.TransportMode json:"mode"`、`serverui.ConfigSummary.Mode config.TransportMode json:"mode"`。
- 客户端 `PUT /api/settings/mode?server=<name>`，服务端 `PUT /api/settings/mode`。
- body `{"mode":"hy2+tcp","current_password":"..."}`；成功200返回secret-free TransportState；输入/安全400/403/429沿用existing helpers。
- status/SSE/servers JSON平铺使用 `mode`、`active_transport`、`switch_reason`、`last_switch_at`、`last_probe_at`、`last_probe_error`、`switch_count`。
- metrics：`hy2tun_client_transport_active{server="...",transport="hy2|tcp"}`、`hy2tun_client_transport_switches_total{server="..."}`；现有aggregate metrics保留，使用现有metrics token保护。label转义，有限server列表，不把目标/凭据写label。

- [ ] **Step 1: RED 测试**。`TestModeAPICSRFAndPassword`缺CSRF/当前密码拒绝，正确才UpdateMode。`TestModeAPIServerSelection`未知name404，不退回其他worker。`TestModeAPIInvalidAndSaveFailure`非法mode400，写盘500，快照不变。`TestTransportStateSecretFree`状态/SSE/模板/metrics不含auth/token。`TestMultiMetricsShowsEachServerTransport`不再只依赖default worker。页面测试检查三种option和“切换会断开现有连接”提示。
- [ ] **Step 2: RED**：`go test ./internal/webui/clientui ./internal/webui/serverui -run 'TestModeAPI|TestTransportState|TestMultiMetrics' -count=1`。
- [ ] **Step 3: 实现handler与页面小修改**。复用RequireCSRF、confirmCurrentPassword、body limit、JSON错误；用户显式提交才变更。多服务端列表只展示模式与actualkind，编辑走对应详情。实际状态不是简单推导mode；TCP-active明确标记UDP over TCP。JS成功后清空密码并刷新状态，失败保留当前配置，不使用innerHTML渲染错误。
- [ ] **Step 4: GREEN**：`go test ./internal/webui/... -count=1`，两端 `node --check static/app.js`，校验metrics label转义。
- [ ] **Step 5: Commit**：`feat(web): add transport mode controls and diagnostics`。

### Task 10: LuCI/Basic CLI 模式配置与包兼容

**Files:**
- Modify: `internal/config/basic.go`, `basic_test.go`, `cmd/hy2tun/config_test.go`
- Modify: `scripts/openwrt/luci-app-hy2tun/luasrc/view/hy2tun/status.htm`, `test-ipk.sh`, `test-luci-controller.lua`, `scripts/openwrt/build-luci-ipk.sh`, `examples/client-basic.yaml`

**Interfaces:**
- BasicConfig.Mode返回Effective；BasicUpdate.Mode为`*TransportMode`，`json:"mode"`；缺失表示保留已有mode，新建缺失仍省略mode。
- top-level known key名单加入mode；基础/高级copy保留mode，否则不得把纯TCP配置错判为未知/基础错误。
- LuCI新增`hy2tun-mode` select，options exactvalues；collectPayload明确发送mode；服务器列表仍保持只读，不在LuCI编辑多个worker。
- LuCI app依赖升为`hy2tun (>= 1.6.0)`，因为新版CLI/UI契约。

- [ ] **Step 1: RED 测试**。`TestBasicModePreserveWhenAbsent`旧CLI请求不把auto降回hy2；`TestBasicModeRoundTrip`三值get/set一致，非法拒绝且旧文件不变；`TestBasicTCPHoppingError`保留高级transport时报互斥错误，不能默默清理；`TestModeIsKnownTopLevelKey`不阻止保存。包测试检查select/payload/依赖及GitHub链接/现有POST target不丢失。
- [ ] **Step 2: RED**：`go test ./internal/config ./cmd/hy2tun -run 'TestBasicMode|TestBasicTCPHopping|TestModeIsKnown' -count=1`；新包grep失败。
- [ ] **Step 3: 实现mode保存与LuCI select**。仍停止后保存/启动，仍0600/revision/flock/secret留空保留；不把纯TCP网络字段变化当服务端身份迁移，不改变client ID或mapping revision。运行状态只读提示mode与实际传输由客户端控制台查看。
- [ ] **Step 4: GREEN**：`go test ./internal/config ./cmd/hy2tun -count=1`、`scripts/openwrt/test-ipk.sh`、`scripts/openwrt/test-luci-controller.sh`，内联JS/Lua语法检查。
- [ ] **Step 5: Commit**：`feat(openwrt): configure single-server transport modes in LuCI`。

### Task 11: 本地封锁/恢复 E2E、旧版本兼容与性能测量

**Files:**
- Create: `internal/e2e/transport_test.go`, `transport_gate_test.go`, `internal/transport/bench_test.go`
- Modify: `internal/e2e/e2e_test.go`（复用cert/echo/login helpers），`.github/workflows/ci.yml`（使普通E2E包含这些测试）

**Interfaces:**
- 测试专用 `transportGate`：`Addr() string`、`SetUDPEnabled(bool)`、`Close() error`；在同数字port分别绑定UDP/TCP转发到真实Server，丢弃UDP但不关闭TCP。
- `newTransportGate(t testing.TB, backend string) *transportGate`：测试专用构造函数，失败调用t.Fatal，资源用t.Cleanup回收。
- 测试用每worker独立RuntimeOptions把等待缩短到ConnectTimeout=1s、ProbeInterval=200ms、MinTCPHold=500ms、RecoverySuccesses=3；生产默认不变，算法阈值已由fakeclock测试固定。

- [ ] **Step 1: 写RED E2E测试**。`TestTransportModeNineCombinations`真实双端3×3，7种共同传输成功、2种互斥失败（纯hy2对纯tcp及反向），不静默降级。`TestUDPBlockedFallbackAndRecovery`调用newTransportGate使HY2不可达，TCP上TCP/UDP echo都通过，恢复后actualkind回HY2；期间state只有一个client、probe不改mapping/revision。`TestMultiTransportIsolation`两个server仅一个gate关闭，不影响另一个。`TestFallbackPushRotationAndProxy`TCP模式下push/删除/轮换、SOCKS5和HTTP CONNECT可用。`TestFallbackBackpressureCloseBounded`慢UDP接收/重负载下队列有上限，控制消息可处理，Close释放。
- [ ] **Step 2: RED**：`go test ./internal/e2e -run 'TestTransportMode|TestUDPBlocked|TestMultiTransport|TestFallback' -count=1`；初始应因gate构造函数缺失失败，其他行为缺口按bug处理，不跳过。
- [ ] **Step 3: 实现gate并补兼容与故障测试**。gate在loopback绑定同数字UDP/TCP并转发到backend，UDP开关按packet丢弃，TCP不受影响；保存源peer地址映射，Close关闭所有转发socket/goroutine。默认旧配置仍通过现有TestEndToEnd；用独立版本fixture/子进程验证v1.5.4 HY2客户端连新HY2/auto服务端（不替换生产binary），新hy2客户端连旧server。端口冲突、revoke在握手期间、nonce错误、probe unsupported、回切最终注册失败恢复TCP、不持久化stale config都有明确断言。用临时路径记录结果，不写密码。下载旧binary仅用于本地fixture并验证SHA256。
- [ ] **Step 4: 性能验证**。`BenchmarkTCPMultiplex`/`BenchmarkUDPFrame`给分配、并行流吞吐基线；本地netns/netem丢包/延迟测试是可选权限步骤，无权限则记录而不宣称通过。必须完成gate封锁恢复，不用公网数据泛称零丢包/高性能。
- [ ] **Step 5: GREEN**：`go test -race ./... -count=1`及focused E2E通过。
- [ ] **Step 6: Commit**：`test(e2e): verify UDP blocking TCP fallback and recovery`。

### Task 12: 双语文档、完整验证与发布交接

**Files:**
- Modify: `README.md`, `README.zh-CN.md`, `CHANGELOG.md`, `CHANGELOG.zh-CN.md`
- Modify: `examples/server.yaml`, `examples/client.yaml`, `examples/client-multi.yaml`, `scripts/openwrt/README.md`, `scripts/windows/README.md`, `scripts/systemd/hy2tund.service`/`hy2tun.service`（只在需要说明新监听时）

- [ ] **Step 1: 文档同步**。三模式表、同数字TCP/UDP但非同socket、范围TCP派生、强制模式互斥、TLS配置、UDP over TCP代价、10/30/60秒与3成功、切换断开边界、mode开关和actualkind、TCP防火墙、旧配置默认HY2。不要把新tcp通道写成FRP兼容，不改变生产mode；默认示例仍HY2省略mode，新模式用注释和独立示例展示。
- [ ] **Step 2: 完整验证**：
```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
staticcheck ./...
make build
scripts/openwrt/test-ipk.sh
scripts/openwrt/test-luci-controller.sh
```
另外构建Linux amd64/arm64/arm/mips/mipsle与Windows amd64；QEMU运行各Linux架构`-version`、`--check-config`、至少一个TCP帧烟测。记录CI依赖更新和go.sum，检查许可证NOTICE要求。
- [ ] **Step 3: 真机验收**。先备份OpenWrt配置和包；用隔离测试端点验证tcp、auto及恢复，不修改既有生产服务器防火墙。LuCI保存mode/启停，客户端控制台每server mode实际值，TLS/token错误提示、TCP port占用失败、GitHub链接、控制台密码和正式配置不丢失。测试后恢复原配置/运行状态，不遗留公网测试映射。
- [ ] **Step 4: Commit**：只在验收通过后添加双语v1.6.0变更说明；`docs: document HY2 TCP fallback and recovery`。
- [ ] **Step 5: 审查与交接**。完成逐项审查和全分支审查；获授权后合并推送，再打v1.6.0标签发布双语Release。升级服务端仍保持hy2→升级客户端仍保持hy2→验证原有映射→另获授权开放同数字TCP并启用auto。提供每台机器二进制/配置备份和回滚步骤；若失败停在发布前，不凭Go单测宣称自动切换已验证。

## 计划自检记录

- Spec §1–3：Task1/5/6；§4–6：Task2/3/4；§7：Task7/11；§8：Task8/9/10；§9–11：文件边界及Task11/12。
- Review Focus 五项分别有 Task2/7、Task7/8、Task3/11、Task5/8、Task4/7/11 的具名测试。
- `Session`、`DatagramConn`、`TCPPeer`、`RuntimeOptions`、`ModeAPI` 的消费方均引用生产任务给出的同名接口；配置不导入transport，避免循环依赖。
- Task4保留旧HY2入口并新增TCP身份验证，不要求第三方Outbound凭空提供不存在的调用身份。
- 所有流在handoff/Close时按真实session对象清理；数字同端口处理`:0`和port hopping的first-item语义，不用排序结果替代。
- 中间任务不是可发布版本；全量验收与发布/部署属于Task12门槛，不先打标签再补关键功能。
