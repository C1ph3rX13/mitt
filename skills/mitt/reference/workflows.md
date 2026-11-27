# 典型工作流

这些工作流展示如何把 `history`、`show`、`send --patch`、`diff` 组合成可重复的安全测试闭环。所有示例默认直接执行 `mitt ...`。

> **先设 scope。** 下文的代理启动命令都带 `--allow-targets`。只在获得授权的测试环境里发送攻击载荷；不要对生产目标执行 SQLi、JWT 绕过或 IDOR 探测。

## 0. 公共准备

```bash
export STORE="${HOME}/.mitt"
export TARGET="app.example.test"          # 替换为授权目标 host
export CTX="lab-$(date +%s)"              # 本次测试的 context 标签

mitt --store "$STORE" ca init
mitt --store "$STORE" ca install
# 非 Debian/Kali 系统若系统安装失败，用逃生舱导出后手工导入：
# mitt --store "$STORE" ca install --output /tmp/mitt-ca.pem

# 终端 A：前台代理；同环境 Playwright/curl 用 127.0.0.1:8080
mitt proxy start \
  --store "$STORE" \
  --addr 127.0.0.1:8080 \
  --allow-targets "$TARGET" \
  --events
```

每个工作流都用 `X-Repeater-Context` 给流量打标签。`mitt` 把它写入 `history.jsonl` 的 `context` 字段，并在转发前剥离，不会污染目标服务器。取最近一条匹配流量：

```bash
ID=$(mitt history --store "$STORE" --context "$CTX" --filter captured \
  | jq -r '.id' | head -n1)
mitt show --store "$STORE" "$ID"
mitt show --store "$STORE" "$ID" --detail
```

如果 `history` 返回 `NO_MATCH`（66），说明标签没带上、代理没启动、scope 拒绝了目标，或浏览器没走代理。先看错误事件：

```bash
mitt history --store "$STORE" --filter mitm_error
mitt show --store "$STORE" <mitm-error-id> --detail
```

---

## 1. SQLi：从基线响应到注入响应

### 目标

捕获一个带查询参数的搜索请求，改查询参数或 body 为 SQLi 载荷，重放后用 `diff` 判断响应是否出现错误、长度变化或数据泄露特征。

### 1.1 捕获基线

用 Playwright 或 curl 发起一次正常搜索。curl 版本：

```bash
curl -sS \
  -x http://127.0.0.1:8080 \
  --cacert "$STORE/ca/ca.pem" \
  -H "X-Repeater-Context: $CTX" \
  "https://$TARGET/search?q=widget" >/dev/null
```

Playwright 版本见 `playwright-integration.md`；在 context 上设置 `extraHTTPHeaders: { "X-Repeater-Context": CTX }` 即可。

找到条目并确认请求文档：

```bash
BASELINE=$(mitt history --store "$STORE" --context "$CTX" --filter captured \
  | jq -r '.id' | head -n1)
mitt show --store "$STORE" "$BASELINE" --detail
```

重点看 `request_document.method`、`url`、`query`、`headers` 和 body 路径。`headers` 是有序的 `[["K","V"], ...]`，重复 header 会保留。

### 1.2 构造注入 patch

用 `@path` 避免 shell 引号地狱。下面的例子同时替换 URL 并把请求方法改为 POST，body 用 JSON 字符串表达：

```bash
cat > /tmp/sqli.patch.json <<PATCH
{
  "method": "POST",
  "url": "https://$TARGET/search?q=widget",
  "headers": [
    ["Content-Type", "application/json"],
    ["Accept", "application/json"]
  ],
  "body": "{\"q\":\"widget' OR 1=1--\"}"
}
PATCH
```

> `headers` 是**整体替换**，不是合并。必须把鉴权、Cookie、Content-Type 等需要的 header 都写回；只写 `Content-Type` 会丢掉原来的登录态。更安全的做法是只 patch 需要改的字段，先不碰 headers。

只想改查询参数时，可以只 patch URL。`query` 未出现时跟随最终 URL 的查询串：

```bash
cat > /tmp/sqli-url.patch.json <<PATCH
{
  "url": "https://$TARGET/search?q=widget%27%20OR%201%3D1--"
}
PATCH
```

先预览，确认 `Content-Length` 重算、`Host` 跟随最终 URL：

```bash
mitt send --store "$STORE" "$BASELINE" --patch @/tmp/sqli.patch.json --dry-run
```

`--dry-run` 不发送、不落盘。检查 `request_document` 和 `request_bytes_preview`（base64）后再发送。

### 1.3 重放并对比

```bash
RESULT=$(mitt send --store "$STORE" "$BASELINE" \
  --patch @/tmp/sqli.patch.json \
  --diff-against)
echo "$RESULT" | jq '{id, kind, parent_id, status, diff_against}'

INJECTED=$(echo "$RESULT" | jq -r '.id')

# 独立、完整的响应对比
mitt diff --store "$STORE" "$BASELINE" "$INJECTED"
```

`send --diff-against` 的输出适合快速判断：

- `diff_against.status.same=false`：状态码变化。
- `diff_against.body.similarity`：文本 body 的共享内容比例；明显下降通常值得看。
- `diff_against.headers.added/removed/changed`：header 层面的变化。
- `diff_against.changed`：status、headers、body 任一不同。

然后按需读取原始字节：

```bash
mitt show --store "$STORE" "$INJECTED" --raw
mitt show --store "$STORE" "$INJECTED" --detail
```

### 1.4 常见判断

- 500 + SQL 错误字样：最直接的注入信号，但仍需确认是否由目标真实逻辑触发。
- 200 但 body 长度/结构变化：盲注或布尔条件的迹象；用多个载荷重复 `send` + `diff` 建立对照。
- 两次响应完全相同：可能被 WAF 拦截、参数未生效、或目标用了参数化查询。换参数名/编码方式，不要只重复同一载荷。

---

## 2. JWT：替换 token 与声明

### 目标

捕获一个带 JWT 的已认证请求，分别测试无效签名、`alg:none`、过期 token、篡改声明等变体，并对比响应。

### 2.1 捕获已认证请求

先让浏览器/客户端正常登录，再访问一个受保护 API。用独立 context 便于筛选：

```bash
CTX="jwt-$(date +%s)"

curl -sS \
  -x http://127.0.0.1:8080 \
  --cacert "$STORE/ca/ca.pem" \
  -H "X-Repeater-Context: $CTX" \
  -H "Authorization: Bearer $ORIGINAL_JWT" \
  "https://$TARGET/api/me" >/dev/null

ID=$(mitt history --store "$STORE" --context "$CTX" --filter captured \
  | jq -r '.id' | head -n1)
mitt show --store "$STORE" "$ID" --detail
```

确认 `request_document.headers` 中确实有 `Authorization`。如果 token 放在 Cookie 或其他 header，同样在 patch 中替换对应项。

### 2.2 用环境变量注入 token

不要把 token 写进命令行历史。`--var TOKEN`（不带 `=`）从环境变量读取：

```bash
export CANDIDATE_JWT='eyJhbGciOiJub25lIn0.eyJzdWIiOiJhZG1pbiJ9.'

cat > /tmp/jwt.patch.json <<'PATCH'
{
  "headers": [
    ["Authorization", "Bearer {{TOKEN}}"],
    ["Accept", "application/json"]
  ]
}
PATCH

mitt send --store "$STORE" "$ID" \
  --patch @/tmp/jwt.patch.json \
  --var TOKEN="$CANDIDATE_JWT" \
  --dry-run
```

同样注意：`headers` 整体替换会丢掉原来的其它 header。如果目标依赖 Cookie、`X-Requested-With`、CSRF header，需要一起写回；或者只 patch `body`/`url`，让原 headers 保留。

### 2.3 测试变体

每次替换 token 后重放并保存新 id：

```bash
try_jwt() {
  local label="$1" token="$2"
  local out
  out=$(mitt send --store "$STORE" "$ID" \
    --patch @/tmp/jwt.patch.json \
    --var TOKEN="$token" \
    --diff-against)
  local new_id
  new_id=$(echo "$out" | jq -r '.id')
  echo "$label -> $new_id"
  echo "$out" | jq '{status, diff_against: {status: .diff_against.status, changed: .diff_against.changed}}'
  mitt diff --store "$STORE" "$ID" "$new_id" > "/tmp/jwt-$label.diff.json"
}

# 1) 无效签名
try_jwt invalid-signature "${ORIGINAL_JWT%????}AAAA"

# 2) alg:none
try_jwt alg-none 'eyJhbGciOiJub25lIn0.eyJzdWIiOiJhZG1pbiJ9.'

# 3) 篡改 role/sub 后重新签名的 token
try_jwt role-admin "$TAMPERED_JWT"

# 4) 过期 token
try_jwt expired "$EXPIRED_JWT"
```

### 2.4 读取结果

```bash
mitt history --store "$STORE" --filter replayed
mitt show --store "$STORE" "$NEW_ID" --detail
mitt diff --store "$STORE" "$ID" "$NEW_ID"
```

关注：

- `status` 从 200 变 401/403：服务端在验证签名或声明。
- 仍为 200 且 body 包含更高权限数据：JWT 验证或授权逻辑可能有缺陷。
- `WWW-Authenticate` 等响应 header 变化：可帮助判断拒绝原因。

> JWT 测试容易误判：某些服务在 token 无效时回退到匿名身份并返回 200。不要只看状态码，要对比 body 中的身份字段和实际数据。

---

## 3. IDOR：替换对象标识

### 目标

以用户 A 的身份捕获请求，把 URL/body 中的对象 id 换成用户 B 的对象 id，观察是否仍返回 B 的数据。

### 3.1 准备两个身份

用两个 Playwright context 或两个 curl 会话分别登录 A、B，并用不同 context 标签抓包：

```bash
# 用户 A：对象 1001
curl -sS -x http://127.0.0.1:8080 --cacert "$STORE/ca/ca.pem" \
  -H "X-Repeater-Context: idor-user-a" \
  -H "Cookie: session=$USER_A_SESSION" \
  "https://$TARGET/api/invoices/1001" >/dev/null

# 用户 B：对象 2001
curl -sS -x http://127.0.0.1:8080 --cacert "$STORE/ca/ca.pem" \
  -H "X-Repeater-Context: idor-user-b" \
  -H "Cookie: session=$USER_B_SESSION" \
  "https://$TARGET/api/invoices/2001" >/dev/null
```

取 A 的条目作为基座：

```bash
A_ID=$(mitt history --store "$STORE" --context idor-user-a --filter captured \
  | jq -r '.id' | head -n1)
B_ID=$(mitt history --store "$STORE" --context idor-user-b --filter captured \
  | jq -r '.id' | head -n1)

mitt show --store "$STORE" "$A_ID" --detail
mitt show --store "$STORE" "$B_ID" --detail
```

### 3.2 用 A 的凭据请求 B 的对象

只改 URL，保留 A 的 Cookie 等 headers：

```bash
cat > /tmp/idor.patch.json <<PATCH
{
  "url": "https://$TARGET/api/invoices/2001"
}
PATCH

mitt send --store "$STORE" "$A_ID" \
  --patch @/tmp/idor.patch.json \
  --dry-run
```

预览确认 `Host`、`Cookie` 和 `Content-Length` 符合预期，然后发送：

```bash
OUT=$(mitt send --store "$STORE" "$A_ID" \
  --patch @/tmp/idor.patch.json \
  --diff-against)
echo "$OUT" | jq '{id, status, diff_against: {status: .diff_against.status, body: .diff_against.body, changed: .diff_against.changed}}'
CROSS_ID=$(echo "$OUT" | jq -r '.id')
```

### 3.3 判定

```bash
# A 的原始对象 vs A 越权请求 B 的对象
mitt diff --store "$STORE" "$A_ID" "$CROSS_ID"

# 合法 B 的响应 vs A 越权请求 B 的响应
mitt diff --store "$STORE" "$B_ID" "$CROSS_ID"
```

- 第一组差异很大、第二组几乎相同：A 拿到了 B 的数据，典型 IDOR。
- 两组都返回 403/404：服务端做了对象级授权，换其它对象 id 或其它端点继续验证。
- 第一组和第二组都相同：可能请求没带对身份，或对象不存在；先检查 `mitt show --detail` 的 headers 和响应 body。

### 3.4 批量对象 id

对多个 id 重复调用即可，`send` 每次产生一条 `replayed`，`parent_id` 指向 `A_ID`，历史可回溯：

```bash
for OBJ in 2001 2002 2003; do
  mitt send --store "$STORE" "$A_ID" \
    --patch "{\"url\":\"https://$TARGET/api/invoices/$OBJ\"}" \
    --diff-against > "/tmp/idor-$OBJ.json"
done

mitt history --store "$STORE" --filter replayed
```

---

## 4. 排障清单

| 现象 | 先检查 | 处理 |
|---|---|---|
| `history` 返回 `NO_MATCH`（66） | 代理是否运行、目标是否走代理、`--context` 是否拼对 | `mitt proxy status`；用 `history` 不加过滤确认有流量 |
| 上游答了，`history` 里却没有 | 客户端是否在响应到达前取消或关闭了连接（浏览器停止、页面跳转） | 预期行为：只有代理观察到完整交付的交换才成记录；换 `curl` 这类会读完响应的客户端重试 |
| `history --filter mitm_error` 有 `scope_denied` | `--allow-targets` / `--deny-targets` | `*.example.com` 不匹配 apex；deny 优先 |
| 浏览器报证书错误 | `mitt ca status`；是否在启动浏览器前安装 CA | `mitt ca install`，重启浏览器；非 Debian 用 `--output` 手工导入 |
| 目标只支持 h2，抓不到 | `mitt show <mitm-error-id> --detail` 的 `reason` | 重启代理加 `--http2-alpn h2,h1` |
| 同一 store 启动第二个代理返回 `LOCKED`（69） | 是否已有代理 | 复用现有代理或 `mitt proxy stop` |
| `send` 返回 `TEMP_FAILURE`（75） | 上游是否超时/5xx | `retryable=true`，可安全重试 |
| `send` 返回 `UPSTREAM_TLS`（1） | 上游证书/pinning | 重试无用；换测试目标或策略 |
| `send` 返回 `INVALID_PATCH`（2） | patch JSON、`{{K}}` 是否都替换 | 先用 `--dry-run` 验证最终文档 |

## 5. 用 raw 文件复现外部请求

已有 Burp/curl 导出的原始 HTTP 请求时：

```bash
mitt send --store "$STORE" --from-file request.http --dry-run
mitt send --store "$STORE" --from-file request.http
```

不带 `--patch` 时按文件字节原样发送；需要结构化修改时加 `--patch`，此时会先解析成请求文档再重渲染，chunked 请求会被解码并以 `Content-Length` 发送。
