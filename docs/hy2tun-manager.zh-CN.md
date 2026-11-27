# hy2tun-manager 运维指南

[English](hy2tun-manager.md) | **简体中文**

`hy2tun-manager` 是面向已有 `hy2tund` 服务端的可选多服务端控制台。它通过
各服务端现有的 Web/API 聚合状态，让一个管理员在一个页面里管理客户端、映射、
服务端设置和凭据。

它不替代 `hy2tund`/`hy2tun` 数据面：客户端仍然只连接 `hy2tund`，不连接
manager；每台服务端仍然是其客户端和映射的权威来源。

manager 只保存：

- 服务端清单（名称、base URL、上游管理员用户名、TLS 设置）
- 上游管理员密码的 AES-256-GCM 密文
- manager 自己的单管理员 bcrypt 哈希
- 不含敏感值的追加式审计日志

## 范围与限制

第一版支持：

- 服务端清单的新增、测试、刷新、删除
- 服务端状态、版本、模式、在线客户端数、活动映射数
- 客户端列表与详情
- 映射新增、修改、删除、启用/禁用
- `public` / `local` 暴露范围
- 服务端传输模式、端口策略、Web 管理员、trust-proxy
- 服务端凭据轮换与撤销

第一版不包含：

- 修改客户端本机配置（Web、proxy、failover、TLS、日志、`servers:` 列表）
- 新增/删除客户端到服务端的连接关系
- 离线客户端映射推送
- 服务端重启、二进制升级或批量部署
- 多用户、角色权限
- 在 manager 内嵌原始 `hy2tund` 控制台

## 下载

manager 使用独立版本和发布标签：

<https://github.com/ayeaaaa/hy2tun/releases/tag/hy2tun-manager-v0.1.2>

归档包含：

- `hy2tun-manager` — 静态二进制
- `manager.example.yaml` — 示例配置
- `hy2tun-manager.service` — systemd 示例
- `README.md` / `README.zh-CN.md` — 本指南

仅支持 Linux amd64 和 arm64。

## 安装

```sh
tar -xzf hy2tun-manager_0.1.2_linux_amd64.tar.gz
sudo useradd --system --user-group --home-dir /var/lib/hy2tun-manager \
  --shell /usr/sbin/nologin hy2tun-manager
sudo install -m 0755 hy2tun-manager /usr/local/bin/hy2tun-manager
sudo install -d -m 0755 /etc/hy2tun
sudo install -m 0644 manager.example.yaml /etc/hy2tun/manager.yaml
sudo install -m 0644 hy2tun-manager.service /etc/systemd/system/hy2tun-manager.service
sudo systemctl daemon-reload
```

启动前检查 `/etc/hy2tun/manager.yaml`，然后：

```sh
sudo systemctl enable --now hy2tun-manager
```

默认监听 `127.0.0.1:7600`。manager 自身不终止 TLS，应放在 HTTPS 反向代理、
SSH 隧道或可信 VPN 后面。

## 配置

```yaml
listen: "127.0.0.1:7600"
base_path: "/"
state_file: "/var/lib/hy2tun-manager/state.json"
key_file: "/var/lib/hy2tun-manager/manager.key"
audit_file: "/var/lib/hy2tun-manager/audit.jsonl"
refresh_interval: "30s"
trust_proxy: false
```

- `base_path` 支持反向代理前缀，例如 `/hy2tun-manager/`。
- `trust_proxy` 仅在受信反向代理会覆盖 `X-Forwarded-For` 和 `X-Real-IP` 时启用。
- `state_file` 与 `key_file` 必须成对备份；丢失 `manager.key` 会导致上游密码无法解密。

## Docker

仓库的 Dockerfile 提供 `manager` target，Compose 文件提供 `hy2tun-manager`
服务。镜像以非 root UID/GID `65532` 运行，并使用 host 网络，与原生安装默认的
loopback 监听一致。

```sh
docker compose build hy2tun-manager
mkdir -p manager-config manager-state
cp examples/manager.yaml manager-config/manager.yaml
sudo chown -R 65532:65532 manager-config manager-state
docker compose up -d hy2tun-manager
```

配置在 `manager-config/manager.yaml`；状态、密钥、审计日志和单实例锁在
`manager-state/`。请成对备份状态和密钥。manager 仍在宿主机 loopback 的
`127.0.0.1:7600` 监听，应由宿主机的 HTTPS 反向代理或 VPN 对外发布。

Compose 服务使用 host 网络，manager 也只发布 Linux 版本；不要用同一个状态目录
运行多个 manager 容器，进程锁只覆盖单个容器。

## 首次登录

首次登录用户名和密码都是 `admin`。首次启动只会把 `admin` 的 bcrypt 哈希写入
`state.json`；请首次登录后立即按下面的重置流程修改密码，不要长期保留默认密码。

查看服务状态：

```sh
sudo systemctl status hy2tun-manager
sudo journalctl -u hy2tun-manager -n 50
```

也可以首次启动前通过 systemd 环境变量
`HY2TUN_MANAGER_INITIAL_PASSWORD` 指定其他初始密码；已有状态时该变量会被忽略。

## 添加上游服务端

打开 manager，进入“服务器清单”添加服务端：

- 显示名称
- base URL（例如 `https://us.example.net/hy2tund`）
- 上游 `hy2tund` Web 管理员用户名和密码
- 可选 TLS 跳过校验（仅用于测试或自签代理）

“测试连接”会先登录并读取 `/api/overview`，成功后才保存。manager 会加密保存
上游密码，并在后续请求中用现有会话/CSRF 模型访问服务端 API。

## 反向代理示例

```nginx
location /hy2tun-manager/ {
    proxy_pass http://127.0.0.1:7600/;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

若使用 `base_path`，manager 页面和静态资源的链接会自动带前缀。反向代理应保留
该前缀，不要把路径前缀剥掉后再转发。

## 日常使用

- 总览页查看所有服务端的状态、版本、模式、在线客户端和活动映射。
- 服务端详情页查看客户端列表；在线客户端可以编辑映射。
- 离线客户端映射只读；暴露范围仍可修改，因为它是服务端状态。
- 映射保存使用完整列表和已读取的 base revision；冲突会提示重新加载，不会静默覆盖。
- 服务端设置页可修改模式、端口策略、Web 管理员和 trust-proxy。
- 凭据页可轮换/撤销服务端凭据，并查看每台在线客户端的同步结果。

## 重置管理员密码

运行中的 manager 持有状态锁，必须先停止服务：

```sh
sudo systemctl stop hy2tun-manager
sudo -u hy2tun-manager /usr/local/bin/hy2tun-manager \
  -config /etc/hy2tun/manager.yaml -reset-admin
sudo systemctl start hy2tun-manager
```

`reset-admin` 在服务运行时会被拒绝，不能覆盖运行中进程的旧状态。

## 安全与备份

- 把 manager 当成高敏感系统：它持有所有上游服务端的管理员凭据。
- 仅通过 HTTPS/VPN 暴露；默认只监听 loopback。
- manager 使用独立 cookie `hy2tun_manager_session`，不会与 `hy2tund` 的会话冲突。
- 审计写入失败时写操作会 fail-closed，不会继续执行。
- 备份 `state.json`、`manager.key` 和 `audit.jsonl`；不要只备份 state。
- 上游密码和 HY2 凭据不会出现在 manager 页面、日志或审计中。

## 构建与发布

```sh
make build-manager
VERSION=0.1.2 ./scripts/release-manager.sh
```

只构建 Linux amd64/arm64。发布脚本不会修改 `hy2tund`/`hy2tun` 的现有发布矩阵。