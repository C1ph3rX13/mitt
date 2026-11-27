<div align="center">

# mitt

**Agent 优先的 HTTP/HTTPS 抓包与重放 CLI**

本机正向代理 + CONNECT MITM 抓包，`send --patch` 字节级改包重放，`diff` 对比响应。
所有命令输出结构化 JSON，自带机器可读的命令契约。

[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=flat-square&logo=go)](https://go.dev)
[![Platform](https://img.shields.io/badge/platform-linux-lightgrey?style=flat-square&logo=linux)](https://github.com/C1ph3rX13/mitt)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue?style=flat-square)](LICENSE)

[English](README.md) · **简体中文**

</div>

---

## ⚠️ 先读这一节

`mitt` 是一个 MITM 代理。它会为它被要求访问的**任意** HTTPS 目标签发叶证书并解密流量。

- **默认监听 `0.0.0.0:8080`**：同主机、同网络命名空间或同网段内任何能访问该端口的进程，都能把 `mitt` 当代理用。这不是"只抓浏览器"的沙箱。
- **必须用 scope 限定目标**：生产、共享主机或不可信网络环境必须显式加 `--allow-targets`。`--deny-targets` 优先级高于 allow。`*.example.com` 只匹配子域，**不**匹配 apex `example.com`。
- **只在本机调试时绑定回环**：`--addr 127.0.0.1:8080` 能挡住其他机器，但**不能**替代 scope——同机其他进程仍可使用该代理。
- **CA 私钥是信任锚**：默认在 `~/.mitt/ca/ca.key.pem`（`0600`）。不要提交进 git、不要打进公开镜像、不要发给他人。持有它就能签发该环境信任的任意站点证书。`mitt ca init` 的 JSON 输出**包含私钥**，不要写进共享日志或 CI 日志。
- **只测试你已获授权的系统**。不要把 `--allow-targets` 设成 `*` 或留空来"先跑起来"。
- **MITM 失败时不会透传加密流量**：连接被断开并记成 `mitm_error`。用 `history --filter mitm_error` 区分"没抓到"和"目标没有流量"。

安全启动模板：

```bash
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "app.example.test,*.app.example.test" \
  --deny-targets "billing.example.test"
```

---

## 特性

| | |
|---|---|
| **抓包** | 正向代理 + HTTPS CONNECT MITM，支持 HTTP/1.1 与 HTTP/2（`--http2-alpn h2,h1`），多端口监听（`--port-range`） |
| **保真** | 记录实际收发的原始字节；`show --raw` 原样吐出，不经过重新序列化 |
| **改包** | RFC 7386 JSON Merge Patch 修改请求文档，`--dry-run` 先验证再发送 |
| **重放** | 以历史条目为基座重放，或从 URL / raw 文件直接构造请求；可伪造 TLS ClientHello 指纹（`--tls-profile chrome\|firefox`） |
| **对比** | `diff` 给状态码、header 增删改、文本 unified diff、二进制 SHA-256 |
| **契约** | `mitt spec` 自描述命令面、约束、错误码与输出 schema；每个错误都是带 `code`/`hint`/`details` 的 JSON |
| **Agent 集成** | Skill 文档内嵌在二进制里，`mitt skills install` 展开到宿主 Agent 的 skill 目录 |

---

## 安装

需要 **Linux**（`internal/ca` 与 `internal/proxy` 带 `//go:build linux`）与 **Go 1.25+**。

**从源码构建**（产出静态二进制，`CGO_ENABLED=0`）：

```bash
git clone https://github.com/C1ph3rX13/mitt.git
cd mitt
make build          # -> ./mitt
```

**`go install`：**

```bash
go install github.com/C1ph3rX13/mitt/cmd/mitt@latest
```

**安装到 `/usr/local/bin`：**

```bash
make build
sudo ./install.sh              # DESTDIR=/tmp/x PREFIX=/usr/local 可覆盖
```

---

## 快速开始：抓包 → 改包 → 重放 → 对比

下面的流程在真实目标上验证过。把 `http://127.0.0.1:18080` 换成你有权测试的目标。

### 1. 初始化并安装 CA

```bash
mitt ca init        # 已有有效 CA 时直接复用
mitt ca install     # 写入系统信任库；检测到 certutil 时同步 NSS
mitt ca status      # 期望 ca_exists=true, installed=true
```

`ca install` 需要写系统信任库的权限（通常 root）。非 Debian/Kali 系统或容器里可能失败，用逃生舱导出 PEM 再手工导入客户端：

```bash
mitt ca install --output /tmp/mitt-ca.pem
```

Linux Firefox 用独立 profile 证书库，不会被自动写入；需要抓 Firefox 时：

```bash
certutil -A -d sql:$HOME/.mozilla/firefox/<profile> -n mitt -t "C,," -i ~/.mitt/ca/ca.pem
```

### 2. 启动代理（终端 A）

```bash
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "127.0.0.1" \
  --events
```

- 前台常驻。`Ctrl-C`（SIGINT，退出码 `130`）或另一终端 `mitt proxy stop` 结束。
- `--events` 让每条 `captured` / `mitm_error` 以 NDJSON 写到 stdout；不加时默认静默。
- 同一数据目录已有存活代理时，再次启动返回 `LOCKED`（`69`）。

### 3. 制造一条流量（终端 B）

```bash
curl -sS \
  -x http://127.0.0.1:8080 \
  --cacert "$HOME/.mitt/ca/ca.pem" \
  -H 'X-Repeater-Context: first-run' \
  http://127.0.0.1:18080/foo >/dev/null
```

`X-Repeater-Context` 只写进历史用于分组，**转发前会被剥离**，不会出现在上游请求里。

### 4. 找到并查看条目

```bash
mitt history --context first-run
mitt history --context first-run -f table

ID=$(mitt history --context first-run --filter captured | jq -r '.id' | head -n1)
mitt show "$ID" --detail
```

`history` 是 NDJSON 记录流，一行一条。`show` 默认给条目视图（`request_line` 是客户端发来的行，`forwarded_request_line` 是下一跳收到的行）；显式 `--detail` 才附带 `request_document` 与 body 路径。无匹配时返回 `NO_MATCH`（`66`）。

### 5. 先预览，再重放

```bash
PATCH='{"headers":[["Host","127.0.0.1:18080"],["Accept","text/html"]]}'

# 不发送、不落盘，只检查最终请求文档与字节预览
mitt send "$ID" --patch "$PATCH" --dry-run

# 真正发送；kind=replayed，parent_id=$ID，并附带 diff_against 摘要
NEW_ID=$(mitt send "$ID" --patch "$PATCH" --diff-against | jq -r '.id')
```

`send` 直接连接目标，**不经过**抓包代理。`headers` 是整体替换，必须把想保留的 header 一起写回。

### 6. 对比响应

```bash
mitt diff "$ID" "$NEW_ID"
```

输出状态码、header 增删改与 body 差异：文本给 unified diff，二进制给 SHA-256，超过 1 MiB 截断并置 `truncated=true`。

---

## 命令面

| 命令 | 作用 |
|---|---|
| `mitt ca init` | 生成根 CA 或复用现有的 |
| `mitt ca install` | 安装到系统 / NSS 信任库；`--output <path>` 改为导出 PEM |
| `mitt ca status` | 报告 CA 是否存在、是否已安装 |
| `mitt proxy start` | 前台运行抓包代理（`--addr` `--allow-targets` `--deny-targets` `--events` `--http2-alpn` `--port-range` `--session-per-port`） |
| `mitt proxy status` | 报告代理是否在运行 |
| `mitt proxy stop` | 停止运行中的代理 |
| `mitt history` | 列出历史条目（`--filter captured\|replayed\|sent\|mitm_error`，`--context`） |
| `mitt show <id>` | 查看一条条目的请求与响应（`--detail` `--raw`） |
| `mitt send [<id>]` | 直接发包（`<id>` / `--url` / `--from-file` 三选一，`--patch` `--dry-run` `--diff-against` `--tls-profile` `--schema`） |
| `mitt diff <idA> <idB>` | 对比两条条目的响应 |
| `mitt skills install` | 展开内嵌的 Agent Skill 文档 |
| `mitt spec` | 打印机器可读的命令契约 |

`send` 的三种基座互斥且必须恰好提供一个，违反返回 `BASE_CONFLICT`（`2`）。`--patch` 支持内联 JSON、`@file` 与 `-`（stdin）。

**请求文档的 body 只有一种表示**：`body`（UTF-8 文本）、`body_base64`（二进制）、`body_file`（路径）至多一个非 `null`。规则同时写在 `send --schema` 发布的 schema 里，所以 `--dry-run` 接受的文档与真正 `--patch` 接受的文档一致。

**历史条目的 body 只有一种解释**：记录声称多少字节，文件就必须正好是那么多字节。零字节和文件缺失都读作空 body；声称 `N>0` 而文件缺失、变短或变长时，`show --raw`、`send <id>`、`diff` 返回 `INTERNAL_ERROR`（`1`），`details` 带 `id`、`direction`、`path`、`store`——需要字节的命令不会把丢失的字节当成空 body 静默发出。

---

## 输出契约

- 默认输出 JSON。**格式**只有 `-f json|table|yaml|md` 一个开关；**形状**由各命令声明的模式 flag 决定（`show --detail`、`proxy start --events`、`send --dry-run`）。
- `history` 与 `proxy start --events` 是 NDJSON 记录流；其余命令输出单个 JSON 对象。
- 错误永远是 `{"error":{...}}` JSON，写到 **stderr**，`details` 携带结构化上下文，`hint` 给出下一步。
- 退出码对齐 sysexits：

| 退出码 | 含义 |
|---:|---|
| `0` | 成功 |
| `1` | 通用错误（含 `UPSTREAM_TLS`、`INTERNAL_ERROR`） |
| `2` | 用法或参数错误（`INVALID_REQUEST`、`INVALID_PATCH`、`BASE_CONFLICT`） |
| `66` | 空输入（`ENTRY_NOT_FOUND`、`NO_MATCH`） |
| `69` | 服务不可用（`NOT_RUNNING`、`LOCKED`、`ADDR_IN_USE`） |
| `73` | 目标已存在且内容冲突（`SKILL_EXISTS`） |
| `75` | 临时失败，可重试（`TEMP_FAILURE`） |
| `77` | 权限或 scope 拒绝（`SCOPE_DENIED`、`CLIENT_CERT_REJECTED`） |
| `78` | 配置错误（`CA_MISSING`、`CERT_INSTALL_FAILED`、`SKILL_INSTALL_FAILED`） |
| `130` | 被 SIGINT 中断 |

**唯一真源是运行时契约**：`mitt spec` 输出的就是实际执行的命令面、约束与错误码。仓库里的 `skills/mitt/reference/commands.md` 是它的人类可读镜像，由测试逐项比对，不允许文档反向定义命令。

---

## 数据目录

默认 `~/.mitt/`，用全局 `--store <dir>` 覆盖（`~` 会展开）。

```
~/.mitt/
├── ca/               # ca.pem（证书）/ ca.key.pem（私钥，0600）
├── bodies/           # 按条目 id 命名的请求 / 响应 body
├── history.jsonl     # 事件流，一行一条
└── proxy.json        # 运行中代理的状态；停止后移除
```

启用 `--session-per-port` 时，每个端口拿到自己的隔离数据目录 `<store>/ports/<port>/`。

`--store` 同时决定默认 CA 路径——覆盖 store 不会回退到真实的 `~/.mitt/ca`。

---

## Agent 集成

`mitt` 为 Agent 使用而设计：结构化输出、稳定错误码、可自省契约。

二进制内嵌了 Agent skill 文档，展开到宿主发现 skill 的位置：

```bash
mitt skills install                          # 默认 ~/.agents/skills/mitt/
mitt skills install --dest /path/to/skills   # 自定义根目录
mitt skills install --force                  # 覆盖被改动过的旧版本
```

内容一致时幂等返回 `unchanged`；目标内容不同或存在多余文件时返回 `SKILL_EXISTS`（`73`），加 `--force` 才整体替换。

---

## 开发

```bash
make build    # CGO_ENABLED=0 静态二进制，-trimpath -ldflags="-w -s"
make test     # go test ./... 带超时与包并行上限
```

`make test` 会先构建二进制，测试唯一的接缝就是这个产物。测试带三层资源保护：单测试超时、包级超时、包并行上限，避免代理与容器测试在一台机器上堆叠。容器相关的信任库场景通过一次性 Docker 容器验证，需要本地 Docker。

```bash
make test GO_TEST_TIMEOUT=20m GO_TEST_PARALLEL=2
```

代码布局：`internal/proxy` 代理与 MITM，`internal/session` 历史存储，`internal/entry` 条目读取，`internal/replay` 发包，`internal/diff` 对比，`internal/spec` 命令契约，`internal/output` 输出与错误码。`scripts/kali/Dockerfile.example` 是一个下游参考样例（不是交付物），演示把二进制烘进 Kali 镜像并在构建期信任 CA。

---

## 许可

[Apache License 2.0](LICENSE)
