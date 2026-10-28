# hy2tun 端口跳跃设计

> **历史说明：** 本文描述 v1.2 的客户端端口跳跃设计。服务端范围监听和自动
> 防火墙重定向已在 v1.3 实现；当前行为以 `README.md` 和
> `scripts/port-hopping/README.md` 为准。

- 状态：已批准
- 日期：2026-10-28
- 适用版本：v1.2
- 目标读者：实现这套功能的工程师

## 1. 背景

Hysteria 2 支持 UDP 端口跳跃（Port Hopping）：客户端在连接服务端时使用一组
UDP 端口，而不是固定一个端口；连接建立后按固定或随机间隔切换到另一个端口。
它主要用来绕过“针对单个 UDP 端口的限速/阻断”，不是用来突破 UDP 整体封锁。

当前 hy2tun 的客户端只接受单个端口：

```yaml
server: "sg.example.com:443"
```

`internal/config` 的 `validateHostPort` 会拒绝端口列表和范围；
`internal/client` 使用 `net.ResolveUDPAddr`，也没有 `transport.udp` 配置。
因此现在无法使用 Hysteria 2 的端口跳跃。

用户要求先做端口跳跃。为了控制风险，本阶段只改客户端，并采用官方支持的
“服务端单端口监听 + 防火墙重定向”方式；服务端自动解析 `listen: :range`
并管理 nftables/iptables 规则留到下一阶段。

## 2. 目标与非目标

### 2.1 目标

1. 客户端 `server` 支持 Hysteria 2 的多端口地址格式：
   - `host:443`
   - `host:443,8443`
   - `host:20000-30000`
   - `host:443,5000-6000,8443`
2. 客户端新增 `transport.udp` 配置：
   - `hop_interval`：固定跳跃间隔；
   - `min_hop_interval` / `max_hop_interval`：随机跳跃间隔；
   - 默认 30 秒，最小 5 秒；
   - 固定间隔和随机间隔不能同时配置。
3. 使用官方 `github.com/apernet/hysteria/extras/v2/transport/udphop`
   实现跳跃，不修改 HY2 线协议。
4. 与现有 Salamander obfs 组合：每个跳跃出来的 UDP socket 都要经过混淆。
5. 服务端继续监听单个端口；提供 nftables/iptables 重定向的部署说明，
   使服务端可以接收整个端口范围的 UDP 流量。
6. 单端口配置完全保持现有行为；旧配置文件不需要修改。
7. 更新英文/中文 README、示例配置和发布说明。

### 2.2 非目标

- 不实现服务端 `listen: ":20000-50000"` 的自动解析和防火墙规则管理。
  下一阶段再做；本阶段由运维手动配置重定向。
- 不实现 Mimic（Fake TCP）。Mimic 与端口跳跃互斥，且仅 Linux、需要 root
  和 eBPF。
- 不把公网映射端口做成跳跃端口。跳跃只影响客户端到服务端的 HY2 UDP 连接；
  `25022/tcp`、`25053/udp` 等公网映射端口仍然固定。
- 不做 Web 热配置编辑。`server` 和 `transport` 属于需要重启/重连的配置，
  本阶段只支持文件配置。
- 不支持一个客户端同时连接多个服务端。
- 不改变标准 Hysteria 2 客户端的代理路径；服务端仍然兼容标准 HY2 客户端。

### 2.3 反目标

- 不能在服务端没有重定向规则时完全静默地接受端口范围，让客户端“看起来配置
  成功、实际大部分端口不通”。客户端无法可靠探测服务端防火墙，但必须在启动
  时记录明确的 warning，并在文档/控制台中说明需要服务端重定向。
- 不能让 HY2 跳跃范围与公网 UDP 映射端口重叠，导致映射流量被防火墙抢走。
  重叠检查必须覆盖 `ports.allow`、`ports.auto_range` 和客户端显式 UDP 映射，
  不能只看 `ports.allow`。
- 不能因为端口跳跃改变现有单端口用户的连接行为，也不能在保存配置时给单端口
  用户自动写入 `transport.udp`。
- 不能把密码、token 或完整配置写进日志。

## 3. 已确定的方案

### 3.1 客户端配置

`server` 保持字符串字段，但校验从“单个 host:port”扩展为“host + 端口集合”。
端口集合语法与 Hysteria 2 一致：逗号分隔的单端口或闭区间范围。

```yaml
server: "sg.example.com:30000-40000,41000"

transport:
  udp:
    # 固定间隔，默认 30s，最小 5s
    hop_interval: "30s"

    # 或者随机间隔；不能和 hop_interval 同时出现
    # min_hop_interval: "15s"
    # max_hop_interval: "45s"
```

字段命名遵循本项目现有的 snake_case 风格，而不是官方 Hysteria 配置的
camelCase。文档会明确说明这是 hy2tun 自己的配置格式。

规则：

- `server` 没有 `-` 或 `,` 时按单端口处理，行为不变。
- `server` 含 `-` 或 `,` 时必须是合法端口集合；每个端口在 1-65535。
- 端口集合去重/规范化后如果只剩一个端口（例如 `443-443`、`443,443`），
  按单端口处理，配置间隔字段会校验失败。
- 单端口时配置了 `hop_interval` 等字段 → 校验失败，避免“以为在跳，其实没跳”。
- 端口集合时没有配置间隔 → 使用 `udphop` 默认 30 秒，但**不写回配置文件**。
- `hop_interval` 与 `min_hop_interval`/`max_hop_interval` 互斥。
- `min_hop_interval` 和 `max_hop_interval` 必须同时出现，且
  `min <= max`，两者都至少 5 秒。
- 间隔字段使用现有的 `config.Duration` 类型，支持 `30s`、`1m` 等写法。
- `udphop` 每次从集合中随机选端口，可能选中当前端口；不保证每个间隔都发生
  实际变化。验收应在多个间隔内观察“至少变化一次”，而不是要求每 5 秒必变。

### 3.2 客户端连接

当前 `Client.New` 流程：

1. `net.ResolveUDPAddr("udp", cfg.Server)`；
2. `buildTLSConfig`；
3. `buildConnFactory`（只有 obfs 时返回 Salamander factory）；
4. 每次 `runOnce` 创建新的 HY2 client。

改为：

1. 用 `net.SplitHostPort` 拆出 host 和 port 字符串。
2. 如果 port 字符串不含 `-` 或 `,`，继续用 `net.ResolveUDPAddr`。
3. 否则用 `udphop.ResolveUDPHopAddr(cfg.Server)`，得到
   `*udphop.UDPHopAddr`。
4. `TLSConfig.ServerName` 仍然优先用 `tls.sni`，否则用 host 部分；
   端口集合不影响 SNI。
5. `buildConnFactory` 根据地址类型返回：
   - 单端口：现有 `salamanderConnFactory`（有 obfs）或 `nil`（无 obfs）；
   - 端口集合：新的 `hopConnFactory`。
6. `hopConnFactory.New` 每次调用都创建一个新的
   `udphop.NewUDPHopPacketConn(hopAddr, interval, openInner)`。
   `openInner` 在无 obfs 时为 `nil`（使用 `net.ListenUDP`），有 obfs 时创建
   UDP socket 并包一层 Salamander。
7. HY2 client 关闭时关闭 hop packet conn；重连时重新创建，不复用旧 socket。
8. 使用端口集合启动时，客户端记录一条明确的 warning：
   “port hopping enabled; the server must redirect the entire UDP range to the
   HY2 listen port; hy2tun cannot verify server firewall rules.”
   这不是启动失败，因为客户端无法可靠探测服务端防火墙；但用户不能在没有
   任何提示的情况下以为配置已经生效。

`udphop` 会负责：

- 初始随机选一个端口；
- 按固定/随机间隔切换端口；
- 保留旧 socket 一小段时间以接收迟到的包；
- 对上层 QUIC 透明，不触发 HY2 重连。

### 3.3 服务端部署方式

本阶段服务端代码不改。继续使用：

```yaml
listen: ":24443"
```

由运维在服务端添加 UDP 重定向，把跳跃范围转发到 24443。例如 nftables：

```nft
table inet hy2tun_porthop {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    udp dport 30000-40000 redirect to :24443
  }
}
```

或 iptables：

```bash
iptables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
ip6tables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
```

要求：

- 服务端是 Linux；
- 有 `nft` 或 `iptables`/`ip6tables`；
- 有 root 或 `CAP_NET_ADMIN`；
- 云厂商安全组/防火墙放行整个 UDP 范围；
- 规则要持久化（例如发行版的 firewall 配置或 systemd 的启动/停止钩子），
  不能只在当前 shell 生效。

文档会同时给出“手动规则”和“如何验证规则生效”的步骤。

### 3.4 端口范围冲突

服务端的 HY2 跳跃范围是 UDP 端口；公网反向映射也可能是 UDP。两者不能重叠。

本阶段不改服务端端口策略，但文档必须明确：

- 如果 HY2 跳跃范围是 `30000-40000`，则以下来源都不能使用这个范围内的
  UDP 端口：
  - `ports.allow` 中显式允许的端口/范围；
  - `ports.auto_range`（`allow` 为空时的自动分配范围，默认
    `20000-29999`）；
  - 客户端显式配置的 UDP 映射端口。
- 例如 `25053/udp` 可以保留，因为它在 `30000-40000` 外；但如果使用默认
  `auto_range: "20000-29999"`，跳跃范围就不能选 `20000-30000`。
- 如果确实需要重叠，必须改用不重叠的范围，或等下一阶段实现服务端自动
  保留范围。

下一阶段实现服务端 `listen: :range` 时，应把整个范围加入 `reservedPorts`，
阻止公网映射使用这些端口；同时要在配置校验/文档中覆盖 `auto_range`。

### 3.5 兼容性

- **单端口客户端**：行为不变。
- **端口集合客户端 + 已配置重定向的服务端**：正常跳跃。
- **端口集合客户端 + 未配置重定向的服务端**：连接可能在第一个端口成功，
  但跳到未重定向端口后超时；这是部署错误，文档会明确要求先配置规则。
- **标准 Hysteria 2 服务端**：只要服务端有范围监听或手动重定向，hy2tun
  客户端可以跳跃。
- **标准 Hysteria 2 客户端**：服务端单端口 + 手动重定向时，客户端也可以
  使用端口集合；这不是 hy2tun 客户端专属功能。
- **Windows/OpenWrt 客户端**：可以跳跃，因为只涉及出站 UDP；服务端重定向
  仍然要求 Linux。
- **obfs**：Salamander 包装每个 hop socket；文档要求两端 obfs 配置一致。

## 4. 实现范围

### 4.1 配置层

- `internal/config/client.go`
  - 新增 `ClientTransportConfig` / `ClientTransportUDPConfig`。
  - `Transport` 使用指针 + `yaml:"transport,omitempty"`，避免
    `SaveClient` 给单端口用户写入空的 `transport` 段。
  - 间隔字段使用 `omitempty`；默认 30 秒只在构建 hop conn 时使用，不写入
    `applyDefaults`。
  - 新增端口集合校验辅助函数（可复用 `internal/ports.ParseRanges`）。
  - 校验 `server`、间隔互斥、最小 5 秒、单端口不允许间隔字段。
  - 默认值填充不改变单端口行为；单端口 + `Transport == nil` 的 YAML
    round-trip 必须保持无 `transport` 段。
- `internal/config/client_test.go`
  - 单端口、端口列表、范围、混合列表；
  - 非法端口、反向范围、空项；
  - 固定/随机间隔、互斥、过小间隔、单端口带间隔；
  - 默认 30 秒。

### 4.2 客户端连接层

- `internal/client/client.go`
  - `New` 中解析 hop 地址。
  - `buildConnFactory` 接受地址类型并返回 hop factory。
  - 新增 `hopConnFactory`，组合 `udphop` 和 Salamander。
  - 保持 `buildTLSConfig` 的 host 提取逻辑。
- 测试：
  - 地址解析选择正确（单端口 vs hop）。
  - factory 类型正确。
  - hop factory 创建/关闭不泄漏 socket。
  - 有 obfs 时每个 inner conn 都被包装（用 fake listener/factory 或可注入
    的 `ListenUDP` 函数测试）。
  - 现有单端口/obfs 测试继续通过。

### 4.3 文档与示例

- `README.md`、`README.zh-CN.md`：
  - 新增“端口跳跃”章节；
  - 客户端配置、间隔规则；
  - 服务端 nftables/iptables 重定向；
  - 云防火墙、端口冲突、验证方法、故障排查。
- `examples/client.yaml`：
  - 增加注释掉的 `transport.udp` 示例；
  - 保留单端口默认值。
- `examples/server.yaml`：
  - 增加注释说明“端口跳跃由防火墙重定向实现，`listen` 仍是单端口”。
- `scripts/`：
  - 可选：新增 `scripts/port-hopping/README.md`，只放命令和注意事项，
    不自动修改防火墙。
- 发布包已经包含两份 README，无需改 release 脚本。

### 4.4 明确不做

- 不在服务端代码里执行 `nft`/`iptables`；
- 不自动修改云安全组；
- 不解析服务端 `listen: :range`；
- 不增加 Web 设置项；
- 不引入 Mimic。

## 5. 测试与验收

### 5.1 自动测试

```bash
go test ./...
go test -race ./...
make build
```

新增测试覆盖：

- 配置解析/校验；
- 单端口回归；
- hop factory 生命周期；
- obfs + hop 组合；
- 现有 e2e 测试继续通过。

### 5.2 手工验收

在测试服务端上：

1. 保持 `listen: ":24443"`，添加 nftables 重定向
   `udp dport 30000-30010 redirect to :24443`。
2. 客户端配置 `server: "host:30000-30010"`，
   `hop_interval: "5s"`。
3. 启动客户端，确认控制握手完成，并确认日志出现端口跳跃 warning。
4. 在服务端抓包或查看 conntrack，在多个间隔内观察客户端源端口至少变化一次
   （不要求每个 5 秒都变化）。
5. 通过 TCP 映射和 UDP 映射持续传输，确认跳跃期间不断流。
6. 确认 `25053/udp` 不在跳跃范围内，且不在 `ports.allow` /
   `ports.auto_range` 的 UDP 分配范围内；反向 UDP 映射正常。
7. 去掉重定向规则，确认客户端在跳到未开放端口时失败；恢复规则后恢复。
8. Windows/OpenWrt 客户端至少做一次启动和握手验证（如条件允许）。

验收标准：

- 单端口配置行为与 v1.1 完全一致；
- 端口集合客户端在重定向规则存在时能持续传输；
- 跳跃间隔符合配置；
- obfs 开启时仍能握手；
- 文档明确说明服务端手动规则、范围冲突和云防火墙要求。

## 6. 风险与缓解

| 风险 | 缓解 |
| --- | --- |
| 服务端忘记配置重定向，客户端部分端口不通 | 客户端启动 warning + 文档把“先配置服务端规则”放在客户端配置之前；故障排查明确列出 |
| 跳跃范围与 UDP 映射端口重叠 | 文档要求范围避开 `ports.allow`、`ports.auto_range` 和显式 UDP 映射；验收检查 |
| 云安全组只放行单端口 | 文档要求放行整个 UDP 范围 |
| 防火墙规则重启后丢失 | 文档要求持久化，并给出 systemd/firewall 示例 |
| obfs 包装遗漏某个 hop socket | 在 `openInner` 中统一包装，测试覆盖 |
| `udphop` 重复选当前端口，看起来没跳 | 验收改为多个间隔内至少变化一次；文档说明随机选择 |
| 跳跃导致现有连接断开 | 依赖 `udphop` 的透明切换；手工验收持续传输 |
| 单端口用户被新校验误伤或配置被写脏 | 单端口路径保持原逻辑；`Transport` 指针 + `omitempty`；YAML round-trip 测试 |

## 7. 后续阶段

下一阶段再做服务端自动范围监听：

- `listen: ":start-end"` 解析；
- 绑定第一个端口；
- 自动安装/清理 nftables 或 iptables 规则；
- 失败时明确报错；
- 把整个范围加入 `reservedPorts`，避免公网映射冲突；
- 可能增加 Web 只读状态显示。

Mimic 不在本阶段，也不在下一阶段的默认范围；它需要单独设计，并与端口跳跃
互斥。
