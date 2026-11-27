# mitt 命令参考

本文件是 `mitt spec` 的人类可读镜像。**唯一真源是运行时契约**：

```bash
mitt spec
```

`test/cli/skill_docs_test.go` 会把本文件的命令路径、flag 类型、默认值、枚举、约束、positional 参数和命令级错误码与 `mitt spec` 逐项比对。改命令面时先改代码，再更新本文件；不要让文档反向定义命令。

## 全局 flag

所有子命令都接受：

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录；影响 `history`、`bodies`、`proxy.json` 和默认 CA 路径。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 简写 `-f`。**唯一**的输出格式开关；不决定输出形状。 |
| `--help` | bool | `false` | — | 简写 `-h`；打印帮助后退出。`mitt spec` 不登记 help flag。 |

`mitt` 默认输出 JSON。格式只有 `-f` 一个开关；**形状**由各命令声明的模式 flag 决定（`show --detail`、`proxy start --events`、`send --dry-run` / `--schema`），`-f table|md` 会自动选择命令声明的表格摘要（`spec`）。`history` 是记录流（JSON 为 NDJSON），`proxy start` 默认静默，其余命令输出单个 JSON 对象。错误永远以 `{"error":{...}}` JSON 写到 stderr。

## 命令面

### `mitt`

Capture, inspect, modify and replay HTTP/HTTPS traffic.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | — |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

不带子命令时打印帮助。

### `mitt ca`

Generate, install and inspect the root CA used for HTTPS interception.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | — |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--dir` | string | `""` | — | CA 目录；默认 `<store>/ca`。子命令继承该 flag。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

### `mitt ca init`

Generate a root CA or reuse the existing one.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `CA_MISSING`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--dir` | string | `""` | — | CA 目录；默认 `<store>/ca`。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

已有有效 CA 时复用并返回现有内容。输出 `ca_pem`、`ca_key_pem`、`subject`、`not_after`。CA 文件存在但损坏时返回 `CA_MISSING`（78），需备份后重建或换 `--dir`。

### `mitt ca install`

Install the root CA into the environment trust stores.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `CA_MISSING`, `CERT_INSTALL_FAILED`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--dir` | string | `""` | — | CA 目录；默认 `<store>/ca`。 |
| `--output` | string | `""` | — | 只导出 PEM 到该路径，不修改系统信任库。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

默认写 `/usr/local/share/ca-certificates/mitt.crt` 并执行 `update-ca-certificates`（通常需要 root）；检测到 `certutil` 时 best-effort 写入 NSS。非 Debian/Kali 系统或系统安装失败时用 `--output` 导出。输出 `installed_to`、`output_path`、`sha256`、`note`。

覆盖范围是 system 与 NSS（Chromium 使用 `~/.pki/nssdb`）。Linux Firefox 使用自己的 profile 证书库，不会被自动写入；抓 Firefox 需要手动导入：

```bash
certutil -A -d sql:$HOME/.mozilla/firefox/<profile> -n mitt -t "C,," -i ~/.mitt/ca/ca.pem
```

`ca status` 的 `installed` 只表示至少一个 target 生效，不代表所有客户端都信任；NSS 目标会比对昵称 `mitt` 下的证书与当前 CA，昵称存在但证书不同不算已安装。

### `mitt ca status`

Report whether the root CA exists and is installed.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `CA_MISSING`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--dir` | string | `""` | — | CA 目录；默认 `<store>/ca`。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

输出 `ca_exists`、`ca_path`、`installed`、`install_targets`。`ca_exists=false` 时退出码 78、`CA_MISSING`。

### `mitt diff`

Compare the responses of two history entries.

| Field | Value |
|---|---|
| Positionals | `<idA>` (required), `<idB>` (required) |
| Constraints | — |
| Error codes | `ENTRY_NOT_FOUND`, `INVALID_REQUEST`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

两条条目都必须有响应。条目不存在返回 `ENTRY_NOT_FOUND`（66）；条目存在但无响应（如 `mitm_error`）返回 `INVALID_REQUEST`（2）。输出 status、headers、body 三段对比。文本 body 给 unified diff，二进制 body 给 SHA-256 与大小，超过 `diff_body_max_bytes`（1 MiB）截断并置 `truncated=true`。

### `mitt history`

List captured and replayed history entries.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `NO_MATCH`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--context` | string | `""` | — | 只看 `X-Repeater-Context` 精确匹配的条目。 |
| `--filter` | string | `""` | `captured`, `replayed`, `sent`, `mitm_error` | 只看一种事件类型。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

每行一条摘要：`id`、`kind`、`ts`、`method`、`url`、`status`、`context`、`req_body_size`、`resp_body_size`。这些字段与 `show` 摘要取自同一份条目视图，取值一致。无匹配返回 `NO_MATCH`（66）。`--filter` 非法枚举返回 `INVALID_REQUEST`（2）。

### `mitt proxy`

Run and inspect the capture proxy.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | — |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

### `mitt proxy start`

Run the capture proxy in the foreground.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | `session-per-port-needs-range` |
| Error codes | `CA_MISSING`, `LOCKED`, `ADDR_IN_USE`, `INVALID_REQUEST`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--addr` | string | `0.0.0.0:8080` | — | 监听地址。默认暴露所有网卡；本机调试用 `127.0.0.1:8080`。 |
| `--allow-targets` | string | `""` | — | 逗号分隔的允许目标 host；`*.example.com` 只匹配子域。 |
| `--ca` | string | `""` | — | CA 目录；默认 `<store>/ca`。 |
| `--deny-targets` | string | `""` | — | 逗号分隔的拒绝目标 host；优先于 allow。 |
| `--http2-alpn` | string | `h1` | `h1`, `h2`, `h2,h1` | CONNECT MITM 宣告的 ALPN 集合。默认只宣告 HTTP/1.1。 |
| `--port-range` | string | `""` | — | 闭区间 `a-b`；每个端口一个监听器，host 取自 `--addr`。 |
| `--events` | bool | `false` | — | 把每条 `captured` / `mitm_error` 事件以 NDJSON 写到 stdout（`event-stream` 模式；只支持 JSON）。 |
| `--session-per-port` | bool | `false` | — | 配合 `--port-range`，每端口使用 `<store>/ports/<port>/`。属于 `session-per-port-needs-range` 约束（`requires`）。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

**安全默认值：`0.0.0.0:8080`。** 共享环境必须显式加 `--allow-targets`；能不用 `0.0.0.0` 就用 `--addr 127.0.0.1:8080`。

行为要点：

- 前台常驻；SIGTERM 后最多 3 秒完成 in-flight 写盘、删除 `proxy.json`、以 0 退出。SIGINT 清理后退出码 130。清理顺序固定：先停 accept 并排空在途交换，再关监听器，再删 `proxy.json`，最后释放数据目录锁。
- `proxy.json` 是 pid 锁。同 store 已有存活代理时返回 `LOCKED`（69）。
- `proxy.json` 只在全部端口绑定成功、数据目录就绪之后才写：`proxy status` 看到 `running` 就等于这次运行已经能服务。记录不出现，运行就还没就绪。
- `--port-range` 参数非法、`--session-per-port` 未配 `--port-range`、非法 `--http2-alpn` 都返回 `INVALID_REQUEST`（2）。
- 任一端口绑定失败会还回本轮已绑定的监听器与数据目录锁、不写 `proxy.json`，返回 `ADDR_IN_USE`（69）；失败后同一端口范围可以直接重试。
- CA 缺失时返回 `CA_MISSING`（78），不隐式生成。
- 默认 `h1` 下遇到 h2-only 目标会写 `mitm_error{reason:"h2_only_target"}`；需要时显式 `--http2-alpn h2,h1` 或 `h2`。
- 请求中的 `X-Repeater-Context` 按大小写不敏感匹配，取第一个非空值写入历史 `context`，随后从发往上游的请求中删除。

### `mitt proxy status`

Report whether the proxy is running.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `NOT_RUNNING`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

输出 `running`、`pid`、`addr`、`started_at`、`ca_fingerprint_sha256`、`listeners`。未运行或记录已失效返回 `NOT_RUNNING`（69），并清理陈旧 `proxy.json`。`details.reason` 给出具体原因：`missing`（没有 `proxy.json`）、`invalid`（无法解析）、`exited`（记录的进程已退出）、`other_process`（pid 已是别的进程）。

### `mitt proxy stop`

Stop the running proxy.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `NOT_RUNNING`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

按 `proxy.json` 的 pid 发送 SIGTERM，最多等待 4 秒。只有记录的 pid 被确认是 `mitt proxy start`（用 CLI 自己的命令树解析 argv）才会发信号，避免 pid 复用误杀。成功输出 `stopped`、`pid`；未运行返回 `NOT_RUNNING`（69），`details.reason` 与 `proxy status` 完全一致——同一份状态不会再给出两个退出码。

### `mitt send`

Send a request directly without the capture proxy.

| Field | Value |
|---|---|
| Positionals | `<id>` (optional) |
| Constraints | `schema-alone`, `base`, `diff-against-needs-id`, `diff-against-bases`, `var-needs-patch` |
| Error codes | `BASE_CONFLICT`, `ENTRY_NOT_FOUND`, `INVALID_REQUEST`, `INVALID_PATCH`, `UPSTREAM_TLS`, `TEMP_FAILURE`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--diff-against` | bool | `false` | — | 仅 `<id>` 基座：附带与父条目响应的差异摘要。不能与 `--dry-run` 同用。 |
| `--dry-run` | bool | `false` | — | 渲染最终请求文档和字节预览，不发送、不落盘。 |
| `--from-file` | string | `""` | — | 基座：从文件读取 raw HTTP/1.x 请求。属于 `base` 约束（`one_of`）。 |
| `--patch` | string | `""` | — | RFC 7386 JSON Merge Patch；值可为内联 JSON、`@path` 或 `-`（stdin）。 |
| `--schema` | bool | `false` | — | 单独使用，输出请求文档 JSON Schema 并退出。 |
| `--tls-profile` | string | `standard` | `standard`, `chrome`, `firefox` | 出站 ClientHello 指纹；只影响本命令。 |
| `--url` | string | `""` | — | 基座：从绝对 http(s) URL 构造请求。属于 `base` 约束（`one_of`）。 |
| `--var` | stringArray | `[]` | — | 可重复；在 patch 原文中替换 `{{K}}`。`K=V` 给值，`K` 读同名环境变量。必须与 `--patch` 同用。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

**基座三选一**：`<id>`、`--url`、`--from-file`。同时给出多个或一个都不给返回 `BASE_CONFLICT`（2）。这条规则是 `send` 的 `base` 约束（`one_of`），`mitt spec` 与运行时校验读的是同一份声明；`--schema` 模式下该规则不适用（`except`）。

其它行为：

- `<id>` 基座产生 `kind=replayed`、`parent_id=<id>`；`--url` / `--from-file` 产生 `kind=sent`，无 `parent_id`。
- `<id>` 不存在返回 `ENTRY_NOT_FOUND`（66）；文件不可读/无法解析、URL 非法、非法 `--tls-profile` 返回 `INVALID_REQUEST`（2）。
- `--patch` 解析或应用失败、未替换的 `{{K}}`、`body` / `body_base64` / `body_file` 同时存在、`body_file` 读取失败都返回 `INVALID_PATCH`（2）。
- patch 后重渲染：分帧只由最终文档决定——声明过 body 或 body 非空时写单个 `Content-Length`（`Transfer-Encoding` 移除），因此 `--patch '{}'` 与不传 `--patch` 发出同样的字节（含「chunked 基座 + 空 body」）；`Host` 默认跟随最终 URL，patch 显式设置时优先；`headers` 整体替换并保序、保重复。
- 连接超时、DNS/连接失败、上游 5xx 返回 `TEMP_FAILURE`（75，可重试），且 5xx 的请求/响应仍写入历史。上游 TLS 握手失败返回 `UPSTREAM_TLS`（1，不可重试）。上游 4xx 是成功响应，退出 0。
- `--from-file` 不带 `--patch` 时按文件字节原样发送；带 `--patch` 时先解析为请求文档再重渲染，与其他基座走同一条渲染路径。
- `--dry-run` 输出的 `request_document` 与 `show --detail` 同形：`<id>` 基座的 body 用 `body_file` 指向存储文件，`--url` / `--from-file` 渲染出来的 body 内联为 `body` 或 `body_base64`。文档与 `request_bytes_preview` 来自同一次渲染，预览也走 `send` 的同一套前置条件（非法 `--tls-profile` 等）与同一套物化规则——预览到的字节就是会发出的字节。
- `send` 的结果就是刚写入的那条条目的视图（与 `show <新 id>` 一致），另加 `request_body_path` / `response_body_path`；`--diff-against` 时再加 `diff_against`。

### `mitt show`

Show one history entry's request and response.

| Field | Value |
|---|---|
| Positionals | `<id>` (required) |
| Constraints | `raw-excludes-detail` |
| Error codes | `ENTRY_NOT_FOUND`, `INVALID_REQUEST`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--detail` | bool | `false` | — | 附带 `request_document`、body 磁盘路径等详细字段（`detail` 模式）。 |
| `--raw` | bool | `false` | — | 输出实际收发的原始字节（`raw` 模式，非 JSON 文档）。属于 `raw-excludes-detail` 约束（`forbids`：不能与 `--detail` 同用）。 |
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

默认输出条目视图：身份、路由字段、有序 header、body 大小、`status`、`timings`，以及两种请求行——`request_line` 是收到的行，`forwarded_request_line` 是下一跳收到的行（代理把 absolute-form 改成 origin-form、或把 h2 流桥到 h1 上游时两者不同）。replayed 条目还带 `mutations`（`--patch` 原文）。

`--detail` 才附带 `request_document`、`request_body_path`、`response_body_path`。`request_document` 与 `send --patch` 的目标文档同构（headers 是 `[["K","V"], ...]` 数组）；存储条目的 body 用 `body_file` 引用，不内联。`history` 的每行、`show` 的摘要、`send` 的结果都是同一份条目视图的不同投影，所以同一字段在各命令里取值一致。

存储 body 的可用性只有一个解释：记录声称多少字节，读到的文件就必须正好是那么多字节。零字节 body 没有表示（`body_file` 不出现），文件缺失也仍读作空 body；记录声称 N>0 字节而文件缺失、变短或变长时，`show --raw`、`send <id>`（包括 `--patch` 没有替换该 body 的情形）与 `diff` 都返回 `INTERNAL_ERROR`（1），`details` 给出 `id`、`direction`（`request` / `response`）、`path`、`store`。`show` 的摘要与 `--detail` 只读元数据，仍可描述这样的条目。`--patch` 自己点名的 `body_file` 读不到仍是 `INVALID_PATCH`（2）。

`--raw` 从事件中的请求行/状态行、有序 header 数组和 body 文件重建字节，请求行取 `forwarded_request_line`（真正写到下一跳的那一行）。非 chunked 消息保持原字节；存储 header 声明 `Transfer-Encoding: chunked` 时用单个 chunk 重新编码并补终止块，保证可解析但不承诺原分块边界一致。`mitm_error` 条目没有请求/响应字节，`--raw` 返回 `INVALID_REQUEST`（2）。

条目不存在时 `ENTRY_NOT_FOUND`（66）：单个 id 在 `details.id`，多个 id（`diff`）在 `details.ids`，两者都带 `details.store`。

### `mitt skills`

Install the agent skill documentation bundled with mitt.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | — |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录；不影响 skill 安装位置。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

不带子命令时打印帮助。skill 安装位置只由 `install --dest` 决定，默认 `~/.agents/skills`。

### `mitt skills install`

Install the bundled agent skills into a skill root.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `INVALID_REQUEST`, `SKILL_EXISTS`, `SKILL_INSTALL_FAILED`, `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--dest` | string | `~/.agents/skills` | — | skill 根目录；其下每个 skill 一个子目录。支持 `~` 展开。 |
| `--force` | bool | `false` | — | 目标已存在且内容不同时整体替换。 |
| `--store` | string | `~/.mitt` | — | 数据目录；与本命令无关。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

把二进制内嵌的每个 skill 目录写到 `<dest>/<name>/`。内容完全一致时返回 `unchanged` 且不写盘；内容不同或存在多余文件时返回 `SKILL_EXISTS`（73），必须加 `--force` 才整体替换。

```json
{"skills_dir":"/root/.agents/skills","skills":[{"name":"mitt","path":"/root/.agents/skills/mitt","action":"installed"}]}
```

`action` 为 `installed`（新装）、`updated`（`--force` 替换）或 `unchanged`（未写盘）。安装后的 `mitt/` 目录由 mitt 独占；`--force` 会删除其中的多余文件。本命令不读取也不写入 `~/.agents/.skill-lock.json`。

### `mitt spec`

Print the machine-readable command contract.

| Field | Value |
|---|---|
| Positionals | — |
| Constraints | — |
| Error codes | `INTERNAL_ERROR` |

| Flag | Type | Default | Enum | Notes |
|---|---|---|---|---|
| `--store` | string | `~/.mitt` | — | 数据目录。 |
| `--format` | string | `json` | `json`, `table`, `yaml`, `md` | 输出格式。 |

输出整棵命令树、`global_flags`、每个命令的 positional 参数、有效 flag（含 `inherited`）、`constraints`、命令级 `error_codes`、`common_error_codes`、成功输出 JSON Schema、全局 `error_codes` / `exit_codes` 表，以及 `schemas` 和 `limits`（`diff_body_max_bytes`、`request_bytes_preview_max_bytes`）。

`constraints` 是命令对调用者的兼容规则，与运行时校验同源：`kind` 为 `one_of`（恰好一个）、`requires`（依赖）、`forbids`（禁忌）或 `alone`（独占）；`when` / `members` 用 `--flag` 或 `<positional>` 表示；`code` 是该规则违反时发出的错误码；可选的 `except` 指出规则在哪个模式下不适用。`--flag` 可以指向命令自己声明的 flag，也可以指向它继承的全局 flag，两者都按 argv 里是否出现来判定。`alone` 的 `members` 为空，运行时展开为该命令自己贡献的全部 flag 与 positional（继承的全局 flag 不参与展开），因此新增 flag 不会被漏掉。

违反约束时错误 `details` 带 `constraint`（规则名）、`kind`（规则种类）以及该种类的现场信息：`one_of` 是 `present`，`requires` 是 `when`/`requires`，`forbids` 是 `when`/`conflicts`。规则名按声明顺序检查，第一个违反的规则决定错误码。

## 错误码与退出码

| code | 退出码 | retryable | 说明 |
|---|---|---|---|
| `ADDR_IN_USE` | 69 | false | 监听地址已被占用。 |
| `BASE_CONFLICT` | 2 | false | `send` 必须恰好有一个基座。 |
| `CA_MISSING` | 78 | false | CA 不存在或不可读。 |
| `CERT_INSTALL_FAILED` | 78 | false | CA 安装到信任库失败。 |
| `CLIENT_CERT_REJECTED` | 77 | false | 客户端拒绝 MITM 证书。Linux Firefox 不在 `ca install` 覆盖范围，需单独导入 profile。 |
| `ENTRY_NOT_FOUND` | 66 | false | 引用的历史条目不存在。 |
| `INTERNAL_ERROR` | 1 | false | 非预期内部错误。 |
| `INVALID_PATCH` | 2 | false | JSON Merge Patch 无法解析或应用。 |
| `INVALID_REQUEST` | 2 | false | 命令行或请求非法。 |
| `LOCKED` | 69 | true | 同一数据目录已有存活代理。 |
| `NOT_RUNNING` | 69 | true | 代理未运行。 |
| `NO_MATCH` | 66 | false | 没有匹配的历史条目。 |
| `SCOPE_DENIED` | 77 | false | 目标不在 scope 内。 |
| `SKILL_EXISTS` | 73 | false | 目标 skill 已存在且内容不同；加 `--force` 替换。 |
| `SKILL_INSTALL_FAILED` | 78 | false | 写入 skill 根目录失败。 |
| `TEMP_FAILURE` | 75 | true | 临时失败，可安全重试。 |
| `UPSTREAM_TLS` | 1 | false | 上游 TLS 握手失败。 |

| 退出码 | 名称 | 语义 |
|---|---|---|
| 0 | SUCCESS | 命令成功。 |
| 1 | ERROR | 通用错误，含 `UPSTREAM_TLS`。 |
| 2 | USAGE | 用法或参数错误。 |
| 66 | NO_INPUT | 空输入：条目不存在或无匹配。 |
| 69 | UNAVAILABLE | 所需服务不可用。 |
| 73 | CANT_CREATE | 目标已存在且内容冲突。 |
| 75 | TEMP_FAILURE | 临时失败，可重试。 |
| 77 | NO_PERMISSION | 权限或 scope 拒绝。 |
| 78 | CONFIG | 配置错误。 |
| 130 | INTERRUPTED | 被 SIGINT 中断。 |

错误对象固定为：

```json
{"error":{"code":"STRING_CODE","message":"human readable","hint":"actionable next step","retryable":false,"details":{}}}
```

## 上限与 Schema

`mitt spec` 的 `limits`：

| 名称 | 值 | 作用 |
|---|---|---|
| `diff_body_max_bytes` | 1048576 | `diff` 对比 body 时最多读取的字节数；超过则 `truncated=true`。 |
| `request_bytes_preview_max_bytes` | 8192 | `send --dry-run` 的请求字节预览上限。 |

`mitt spec` 的 `schemas` 包含：

- `request_document`：`show --detail` 的 `request_document` 与 `send --schema` 输出的同一份 draft 2020-12 Schema。
- `send_dry_run`：`send --dry-run` 的输出结构，与 `mitt send` 节点的 `output_schemas["dry-run"]` 是同一份声明。
- `spec_summary`：`mitt spec -f table|md` 的摘要行结构，与 `mitt spec` 节点的 `output_schemas["summary"]` 是同一份声明。
- `send_diff_against`：`send --diff-against` 的 `diff_against` 结构。

各命令的默认成功输出 Schema 位于对应命令节点的 `output_schema`；`proxy start` 的默认输出是沉默，所以它的 `output_schema` 为 `null`。其它形状位于 `output_schemas`，键是**模式名**，每个模式声明自己的选择器：

| 字段 | 含义 |
|---|---|
| `flag` | 出现该 bool flag 时选择本形状（`--detail`、`--events`、`--dry-run`、`--schema`、`--raw`）。 |
| `tabular` | `-f table` 与 `-f md` 选择本形状（`spec` 的 `summary`）。 |
| `stream` | 本形状是记录流：JSON 为 NDJSON，表格格式为行。 |
| `event_stream` | 记录边产生边写出，只有 `-f json` 能渲染。 |
| `opaque` | 本形状不是 JSON 文档（如 `show --raw` 的报文字节），用 `description` 代替 `schema`。 |

所有输出 Schema 都由实际输出的 Go 结构体派生，`mitt spec` 只负责序列化声明。
