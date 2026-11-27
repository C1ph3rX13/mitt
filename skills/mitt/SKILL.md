---
name: mitt
description: Drive the mitt CLI to capture, inspect, modify and replay HTTP/HTTPS traffic through a local MITM proxy. Use for Burp Repeater-style work and Playwright-driven web security testing.
---

# mitt

`mitt` 是一个 Agent 优先的 HTTP/HTTPS 抓包与重放 CLI。它在本机启动正向代理，对 HTTPS 执行 CONNECT MITM，把流量写成可检索的 `captured` 事件；再用 `send --patch` 做字节级可控的改包重放，用 `diff` 对比响应。

## ⚠️ 安全提醒

**在启动代理前先读完这一节。**

- **默认监听 `0.0.0.0:8080`**：同主机、同网络命名空间或同网段内任何能访问该端口的进程，都可以把 `mitt` 当代理使用；`mitt` 会为它请求的任意 HTTPS 目标签发叶证书并解密流量。这不是“只抓浏览器”的沙箱。
- **必须用 scope 限定目标范围**：生产、共享主机或任何不可信网络环境都应显式加 `--allow-targets "<in-scope-host>"`。`--deny-targets` 优先级高于 allow。`*.example.com` 只匹配子域，**不**匹配 apex `example.com`。
- **只在本机调试时绑定回环地址**：用 `--addr 127.0.0.1:8080` 避免其他机器连入；这不能替代 scope，同机其他进程仍可使用该代理。
- **只测试已获授权的系统**。不要把 `--allow-targets` 设成 `*` 或留空来“先跑起来”。
- **CA 私钥是信任锚**：默认位于 `~/.mitt/ca/ca.key.pem`（权限 `0600`）。不要提交到 git、不要打进公开镜像、不要发给他人。持有它能签发该环境信任的任意站点证书。`mitt ca init` 的 JSON 输出也包含 `ca_key_pem`，不要把该输出写进共享日志或 CI 日志。
- **MITM 失败时 `mitt` 不透传加密流量**：连接会被断开并写成 `mitm_error`。请从 `history --filter mitm_error` 判断是没抓到，而不是误以为目标没有流量。

安全启动模板：

```bash
# 推荐：只监听本机，只允许授权目标
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "app.example.test,*.app.example.test" \
  --deny-targets "billing.example.test"
```

## 前置假设

- `mitt` 已在 `PATH` 中：`command -v mitt`。
- 目标平台是 Linux。数据目录默认 `~/.mitt/`，可用全局 `--store <dir>` 覆盖。
- 浏览器或 HTTP 客户端把代理指向 `http://127.0.0.1:8080`（或你显式选择的地址）。
- 需要 `curl`；示例中的 JSON 提取使用 `jq`（没有 jq 时可用 Python）。

## 安装 skill 文档

如果宿主 Agent 从 `~/.agents/skills/` 发现 skill，可把二进制内嵌的文档展开到本机：

```bash
mitt skills install
# 自定义 skill 根目录；--force 覆盖被改动过的旧版本
mitt skills install --dest /path/to/skills --force
```

默认安装到 `~/.agents/skills/mitt/`。内容一致时幂等返回 `unchanged`；目标内容不同或存在多余文件时返回 `SKILL_EXISTS`（73），加 `--force` 才整体替换。

## 快速开始：抓包 → 改包 → 重放 → 对比

以下命令全部直接执行 `mitt ...`，不需要容器包装。示例目标是 `https://httpbin.org`，它会回显请求 header，便于观察改包效果。请替换成你有权测试的目标。

### 1. 初始化并安装 CA

```bash
mitt ca init              # 已有有效 CA 时直接复用
mitt ca install           # 写入系统信任库；检测到 certutil 时同步 NSS
mitt ca status            # 期望 ca_exists=true, installed=true
```

`ca install` 需要写入系统信任库的权限（通常需要 root）。在非 Debian/Kali 系统上也可能失败。此时用逃生舱导出 PEM，再手工导入客户端信任库：

```bash
mitt ca install --output /tmp/mitt-ca.pem
```

`ca install` 覆盖 system 与 NSS（Chromium）。Linux Firefox 使用自己的 profile 证书库，不会被自动写入；需要抓 Firefox 时，安装 `libnss3-tools` 后导入并重启 Firefox：

```bash
certutil -A -d sql:$HOME/.mozilla/firefox/<profile> -n mitt -t "C,," -i ~/.mitt/ca/ca.pem
```

profile 目录可从 `~/.mozilla/firefox/profiles.ini` 找到。`ca status` 的 `installed=true` 只表示至少一个目标生效，不代表所有客户端都信任。

### 2. 启动代理（终端 A）

```bash
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "httpbin.org" \
  --events
```

- 前台常驻，用 `Ctrl-C`（SIGINT，退出码 130）或另一终端 `mitt proxy stop` 结束。
- `--events` 让每条 `captured` / `mitm_error` 事件以 NDJSON 写到 stdout；不加时默认静默。
- 同一数据目录已有存活代理时，再次启动返回 `LOCKED`（69）。

### 3. 制造一条被抓取的流量（终端 B）

```bash
STORE="${HOME}/.mitt"

curl -sS \
  -x http://127.0.0.1:8080 \
  --cacert "$STORE/ca/ca.pem" \
  -H 'X-Repeater-Context: first-run' \
  'https://httpbin.org/get?hello=mitt' >/dev/null
```

`curl` 直接信任 mitt CA；浏览器在 `ca install` 后应信任系统信任库。`X-Repeater-Context` 只写入历史，转发前会被剥离，不会出现在上游请求里。

### 4. 找到并查看条目

```bash
mitt history --context first-run
mitt history --context first-run --filter captured

ID=$(mitt history --context first-run --filter captured | jq -r '.id' | head -n1)
mitt show "$ID" --detail
```

没有 `jq` 时可用：

```bash
ID=$(mitt history --context first-run --filter captured \
  | python3 -c 'import json,sys; print(json.loads(sys.stdin.readline())["id"])')
```

注意：`show` 默认输出条目视图（含两种请求行：`request_line` 是收到的行，`forwarded_request_line` 是下一跳收到的行；replayed 条目另带 `mutations`）。**显式 `--detail`** 才附带 `request_document`、`request_body_path`、`response_body_path`，供下一层改包使用；`history` 的每行、`show`、`send` 的结果都是同一份视图的不同投影，同一字段取值一致。无匹配时 `history` 返回 `NO_MATCH`（66）。

### 5. 先预览改包，再重放

用 JSON Merge Patch（RFC 7386）修改请求文档。`headers` 是整体替换，必须把想保留的 header 一起写回。

```bash
PATCH='{"headers":[["Accept","application/json"],["X-Demo","mitt"]]}'

# 不发送、不落盘，只检查最终请求文档与字节预览
mitt send "$ID" --patch "$PATCH" --dry-run

# 真正发送；kind=replayed，parent_id=$ID
mitt send "$ID" --patch "$PATCH" --diff-against
```

`send` 直接连接目标，不经过抓包代理。`--diff-against` 只在 `<id>` 基座下可用，输出里带 `diff_against` 摘要。取新条目 id：

```bash
NEW_ID=$(mitt send "$ID" --patch "$PATCH" --diff-against | jq -r '.id')
```

### 6. 对比两次响应

```bash
mitt diff "$ID" "$NEW_ID"
```

`diff` 输出 status、header 增删、body 差异。文本 body 给 unified diff；二进制 body 给 SHA-256 与大小；超过 1 MiB 截断并置 `truncated=true`。

到这里闭环已经跑通：`history` 找到流量 → `show` 看清请求 → `send --patch` 改包重放 → `diff` 判断效果。

## 核心循环

```bash
# 看有哪些流量（NDJSON，一行一条）
mitt history [--filter captured|replayed|sent|mitm_error] [--context <name>]

# 看一条请求/响应；--raw 输出实际收发的字节
mitt show <id>
mitt show <id> --detail
mitt show <id> --raw

# 以历史条目为基座重放，或从 URL / raw 文件直接发包
mitt send <id> --patch @patch.json --diff-against
mitt send --url 'https://target.test/api' --patch '{"method":"POST","body":"{\"a\":1}"}'
mitt send --from-file request.http

# 对比两条有响应的条目
mitt diff <idA> <idB>
```

三种 `send` 基座互斥，必须恰好提供一个：`<id>`、`--url`、`--from-file`。同时给多个或一个都不给返回 `BASE_CONFLICT`（2）。这条 `base` 约束与 `mitt spec` 的 `constraints` 同源；`--schema` 模式下不适用。

命令级兼容规则都能从 `mitt spec` 的 `constraints` 读到，读到的就是运行时执行的：`show --raw` 不能与 `--detail` 同用（`raw-excludes-detail`），`proxy start --session-per-port` 必须配 `--port-range`（`session-per-port-needs-range`）。违反时错误 `details` 带约束名、`kind` 与冲突项。

`--patch` 的值支持三种来源：

```bash
--patch '{"method":"POST"}'     # 内联 JSON
--patch @patch.json             # 从文件读取
--patch -                       # 从 stdin 读取
```

请求文档的 body 最多有一种表示：`body`（UTF-8 文本）、`body_base64`（二进制）与 `body_file`（路径）三者只能有一个非 `null`，显式 `null` 与不写等价。这条规则同时写在 `send --schema` 发布的 schema 里（`allOf` 的 `not` 子句），所以先用 `--dry-run` 校验的文档与真正 `--patch` 时会接受的文档一致；多写一种表示返回 `INVALID_PATCH`。schema 无法检查的两项——`body_file` 指向的文件是否存在、`body_base64` 是否可解码——只有运行时判定。

历史条目的 body 只有一种解释：记录声称多少字节，文件就必须正好是那么多字节。零字节 body 与文件缺失都读作空 body；记录声称 N>0 字节而文件缺失、变短或变长时，`show --raw`、`send <id>`、`diff` 返回 `INTERNAL_ERROR`（1），`details` 带 `id`、`direction`、`path`、`store`——需要字节的命令不会把丢失的字节当成空 body 静默发出或比较。

需要注入动态值时用 `--var`，它在 JSON 解析前对 patch 原文做纯字符串替换：

```bash
export TOKEN='eyJ...'
mitt send "$ID" \
  --patch '{"headers":[["Authorization","Bearer {{TOKEN}}"],["Accept","application/json"]]}' \
  --var TOKEN
```

`--var TOKEN`（不带 `=`）从同名环境变量读取，避免 token 进入命令行历史；`--var K=V` 直接给值。未替换的 `{{K}}` 返回 `INVALID_PATCH`（2）。

## 输出与错误契约

- **默认输出 JSON**。`history` 是 NDJSON，一行一条摘要；其余命令输出单个 JSON 对象。
- `proxy start` 默认不写 stdout；加 `--events` 才输出事件流 NDJSON。
- `-f table|yaml|md` 只改变**格式**；输出**形状**由命令自己的模式 flag 选择（`show --detail`、`proxy start --events`、`send --dry-run`/`--schema`）。
- stdout 只放数据。错误永远是 JSON 对象，写到 **stderr**：

```json
{"error":{"code":"NOT_RUNNING","message":"proxy is not running","hint":"start it with `mitt proxy start`","retryable":true,"details":{}}}
```

- 退出码对齐 sysexits：`0` 成功、`1` 通用错误、`2` 用法错误、`66` 空输入、`69` 服务不可用、`73` 目标已存在且内容冲突、`75` 可重试失败、`77` 权限/scope、`78` 配置错误、`130` SIGINT。
- 下游分支优先看 `error.retryable`，不必背退出码表。

## 错误码表

| code | 退出码 | retryable | 含义与下一步 |
|---|---|---|---|
| `ADDR_IN_USE` | 69 | false | 监听地址已被占用。换端口或停掉占用进程后重启代理。 |
| `BASE_CONFLICT` | 2 | false | `send` 必须恰好有一个基座：`<id>`、`--url`、`--from-file`。 |
| `CA_MISSING` | 78 | false | CA 不存在或损坏。先 `mitt ca init`；损坏时备份后重建或换 `--dir`。 |
| `CERT_INSTALL_FAILED` | 78 | false | CA 写入系统信任库失败。用 `--output` 导出并手工导入。 |
| `CLIENT_CERT_REJECTED` | 77 | false | 客户端拒绝 MITM 证书（常见于证书 pinning）。检查客户端信任库或换测试目标；Linux Firefox 不在 `ca install` 覆盖范围，需单独导入 profile（见「初始化并安装 CA」）。 |
| `ENTRY_NOT_FOUND` | 66 | false | 引用的历史条目不存在。先 `mitt history` 拿有效 id。 |
| `INTERNAL_ERROR` | 1 | false | 非预期内部错误。重跑；持续失败再报告。 |
| `INVALID_PATCH` | 2 | false | patch 无法解析或应用后产生非法请求。先用 `--dry-run` 修正 patch。 |
| `INVALID_REQUEST` | 2 | false | 命令行或请求非法（非法枚举、互斥冲突、HTTPS 未走 CONNECT 等）。看 `hint`。 |
| `LOCKED` | 69 | true | 同一数据目录已有存活代理。复用现有代理或先 `mitt proxy stop`。 |
| `NOT_RUNNING` | 69 | true | 代理未运行。`mitt proxy start` 后重试。 |
| `NO_MATCH` | 66 | false | `history` 没有匹配条目。换 `--filter` / `--context`，或先抓流量。 |
| `SCOPE_DENIED` | 77 | false | 目标不在 `--allow-targets` 内或命中 `--deny-targets`。检查 scope 与通配符语义。 |
| `SKILL_EXISTS` | 73 | false | 目标 skill 已存在且内容不同。加 `--force` 替换，或用 `--dest` 换安装位置。 |
| `SKILL_INSTALL_FAILED` | 78 | false | 写入 skill 根目录失败。检查目录权限，或用 `--dest` 换位置。 |
| `TEMP_FAILURE` | 75 | true | 临时失败（连接超时、上游 5xx）。**可以安全重试**。 |
| `UPSTREAM_TLS` | 1 | false | 与上游 TLS 握手失败（pinning、证书错误）。重试无用，换策略。 |

代理单条连接的 MITM 失败只写 `mitm_error` 历史事件，不会让代理进程退出。用 `mitt history --filter mitm_error` 查看 `reason`：`scope_denied`、`client_cert_rejected`、`upstream_tls`、`not_tls`、`h2_only_target` 等。

## 数据目录

默认 `~/.mitt/`：

```
~/.mitt/
  ca/ca.pem, ca/ca.key.pem      # 根 CA；私钥 0600
  history.jsonl                 # 事件流水，一行一条
  bodies/<id>-req.bin           # 实际转发的请求 body
  bodies/<id>-resp.bin          # 上游响应 body
  proxy.json                    # 运行期 pid/addr/listeners
  ports/<port>/...              # --session-per-port 时每个端口独立目录
```

`bodies/`、数据目录根与 `ca/` 里的 `.mitt-*.tmp` 是提交前的临时文件：进程崩溃会留下，`proxy start` 与 `send` 开工前会清掉“无人持锁且超过 1 小时”的残留，并往 stderr 打 `mitt: warning: removed N stale temporary file(s) from <目录>`。并发写入中的文件由文件锁保护，不会被清理；history、最终 `.bin`、`proxy.json`、CA 文件与用户自己的点文件永不改动。看到这条 warning 说明上次运行发生过崩溃，不需要额外处理。

`--store <dir>` 换数据目录；`ca --dir` / `proxy start --ca` 单独覆盖 CA 目录。默认 CA 始终是 `<store>/ca`，不会在隔离 store 中误用真实 `~/.mitt/ca`。

## 参考文档

- [`reference/commands.md`](reference/commands.md) — 全命令面、flag、枚举、约束与错误码；与 `mitt spec` 逐项对齐。
- [`reference/workflows.md`](reference/workflows.md) — SQLi、JWT、IDOR 等典型工作流。
- [`reference/playwright-integration.md`](reference/playwright-integration.md) — Node.js / Python / Go 代理配置、`X-Repeater-Context`、拓扑与隔离。

命令面的唯一真源是运行时契约：

```bash
mitt spec
```

新增或修改命令时先改代码，再更新本 Skill；`test/cli/skill_docs_test.go` 会拿文档与 `mitt spec` 对照，漂移即失败。

## 可选：外部编排

如果 Agent 本身不在运行 `mitt` 的环境内，可由外部编排器把命令转发进去，例如 `docker exec <container> mitt history`。这只是调用方式的包装；本 Skill 的默认示例始终直接执行 `mitt ...`，不依赖容器或 `docker exec`。
