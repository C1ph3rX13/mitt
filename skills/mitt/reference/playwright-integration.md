# Playwright 集成指南

`mitt` 是普通 HTTP 正向代理。Playwright 只需把浏览器的代理指向 `http://127.0.0.1:8080`，再给每个 browser context 注入 `X-Repeater-Context`，就能用 `mitt history --context <name>` 把流量按测试场景隔离。

> 安全边界不因 Playwright 而改变：代理默认监听 `0.0.0.0:8080`，**必须**用 `--allow-targets` 限定目标，或把 `--addr` 改为 `127.0.0.1:8080`。不要把整个网段或 `*` 放进 scope。

## 1. 拓扑

### 1.1 同环境（推荐）

Playwright、浏览器和 `mitt` 在同一个容器/主机、同一个 network namespace：

```
┌──────────────────────────────────────────────┐
│ 同一环境（容器或主机）                         │
│                                              │
│  Playwright ──proxy 127.0.0.1:8080──> mitt   │
│       │                                  │   │
│       └────────── 浏览器流量 ─────────────┘   │
│                                          │   │
└──────────────────────────────────────────┼───┘
                                           ▼
                                        target
```

启动：

```bash
# 终端 A：直接执行，不需要 docker exec
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "app.example.test,*.app.example.test" \
  --events
```

Playwright 侧统一使用：

```text
http://127.0.0.1:8080
```

优点：CA、网络、文件系统都在同一环境，最容易排障。优先选这个拓扑。

### 1.2 共享 network namespace（备选）

当 Playwright 与 `mitt` 必须跑在不同容器时，让两个容器共享 network namespace。此时 Playwright 容器里的 `127.0.0.1:8080` 就是 `mitt` 容器里的 `127.0.0.1:8080`：

```
┌──────────────── shared network namespace ────────────────┐
│                                                          │
│  Playwright 容器 ──127.0.0.1:8080──> mitt 容器            │
│                                            │             │
└────────────────────────────────────────────┼─────────────┘
                                             ▼
                                          target
```

Docker 示例（仅演示编排；实际命令仍是 `mitt proxy start`）：

```bash
# mitt 容器内直接执行；外部编排器可以用 docker exec 注入这条命令
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "app.example.test" \
  --events

# Playwright 容器共享 mitt 容器的 netns
docker run --rm \
  --network container:<mitt-container> \
  -v "$PWD":/work -w /work \
  mcr.microsoft.com/playwright:v1.49.0-jammy \
  node test.js
```

Compose 等价写法是把 Playwright 服务的 `network_mode` 设为 `service:<mitt-service>`。

注意：

- 不共享 netns 时，`127.0.0.1` 指向 Playwright 容器自己，必须改用 `mitt` 容器的 IP，并且 `mitt` 要监听 `0.0.0.0`；这会扩大暴露面，必须配 scope。
- 共享 netns 不共享文件系统。两个容器各自有 `~/.mitt` 时，`history` 要在**运行 mitt 的那个容器**里读，或者给 `mitt` 用共享卷 `--store /shared/mitt`。
- 共享 netns 也不共享信任库。CA 必须安装到 Playwright 容器实际使用的浏览器信任库，见 §2。

### 1.3 多 context 隔离

两种做法，按需要选：

| 方式 | 命令 | 隔离粒度 | 适用 |
|---|---|---|---|
| 单端口 + `X-Repeater-Context` | `mitt proxy start --addr 127.0.0.1:8080 ...` | 同 store 内按标签过滤 | 一个测试进程内多个 browser context |
| 多端口 + `--session-per-port` | `mitt proxy start --addr 127.0.0.1:8080 --port-range 8080-8083 --session-per-port ...` | 每端口独立数据目录 | 多个 Playwright 进程/任务并行，互不污染 |

单端口打标：

```js
const context = await browser.newContext({
  proxy: { server: 'http://127.0.0.1:8080' },
  extraHTTPHeaders: { 'X-Repeater-Context': 'checkout-a' },
});
```

```bash
mitt history --context checkout-a
```

多端口 + 每端口独立 session：

```bash
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --port-range 8080-8083 \
  --session-per-port \
  --allow-targets "app.example.test" \
  --events
```

```text
context A -> http://127.0.0.1:8080   -> <store>/ports/8080/
context B -> http://127.0.0.1:8081   -> <store>/ports/8081/
context C -> http://127.0.0.1:8082   -> <store>/ports/8082/
```

读取某个端口的历史：

```bash
mitt --store ~/.mitt/ports/8081 history
mitt --store ~/.mitt/ports/8081 history --filter replayed
```

根 store 的 `mitt proxy status` 会列出所有 listener；`mitt proxy stop` 停止全部监听器。`--session-per-port` 必须与 `--port-range` 一起用，否则 `INVALID_REQUEST`（2）。

## 2. 前置：代理与 CA

1. 初始化并安装 CA：

   ```bash
   mitt ca init
   mitt ca install
   mitt ca status
   ```

2. 确认 `ca status` 输出 `ca_exists=true`。若浏览器和 mitt 在不同容器，把 CA 导出并导入浏览器所在环境：

   ```bash
   mitt ca install --output /shared/mitt-ca.pem
   ```

   在 Playwright 容器里把该 PEM 加进系统/NSS 信任库，或仅对隔离测试环境的 browser context 设 `ignoreHTTPSErrors: true`（会关闭证书校验，生产浏览器不要用）。

3. 启动代理时必须带 scope：

   ```bash
   mitt proxy start \
     --addr 127.0.0.1:8080 \
     --allow-targets "app.example.test" \
     --deny-targets "billing.app.example.test" \
     --events
   ```

4. 先跑一个最小页面确认链路：

   ```bash
   curl -sS -x http://127.0.0.1:8080 \
     --cacert ~/.mitt/ca/ca.pem \
     -H 'X-Repeater-Context: smoke' \
     'https://app.example.test/' >/dev/null
   mitt history --context smoke
   ```

## 3. Node.js

```js
// npm i playwright
const { chromium } = require('playwright');

const PROXY = 'http://127.0.0.1:8080';
const CONTEXT = 'pw-node-lab';

(async () => {
  const browser = await chromium.launch({
    headless: true,
    proxy: { server: PROXY },
  });

  // extraHTTPHeaders 是最简单的 context 标签注入方式。
  const context = await browser.newContext({
    extraHTTPHeaders: { 'X-Repeater-Context': CONTEXT },
    serviceWorkers: 'block',
  });

  const page = await context.newPage();
  await page.goto('https://app.example.test/');
  await page.fill('input[name="q"]', 'widget');
  await page.press('input[name="q"]', 'Enter');
  await page.waitForLoadState('networkidle');

  await browser.close();
})();
```

验证：

```bash
ID=$(mitt history --context pw-node-lab --filter captured | jq -r '.id' | head -n1)
mitt show "$ID" --detail
```

需要在一个 context 内按请求动态换标签时，用 `context.route()`（记得保留原 headers）：

```js
const context = await browser.newContext({
  proxy: { server: PROXY },
  serviceWorkers: 'block',
});

await context.route('**/*', async (route) => {
  const label = route.request().url().includes('/admin/') ? 'admin' : 'public';
  const headers = { ...route.request().headers(), 'x-repeater-context': label };
  await route.continue({ headers });
});
```

注意：`route.continue()` 的 `headers` 会**整体替换**原请求 headers，所以必须先展开 `route.request().headers()`。`mitt` 在转发前会剥离 `X-Repeater-Context`，目标服务器收不到它。

## 4. Python

同步 API：

```python
# pip install playwright && playwright install chromium
from playwright.sync_api import sync_playwright

PROXY = "http://127.0.0.1:8080"
CONTEXT = "pw-python-lab"

with sync_playwright() as p:
    browser = p.chromium.launch(
        headless=True,
        proxy={"server": PROXY},
    )
    context = browser.new_context(
        extra_http_headers={"X-Repeater-Context": CONTEXT},
        service_workers="block",
    )
    page = context.new_page()
    page.goto("https://app.example.test/")
    page.fill('input[name="q"]', "widget")
    page.press('input[name="q"]', "Enter")
    page.wait_for_load_state("networkidle")
    browser.close()
```

动态注入（保留原 headers）：

```python
def tag_context(route):
    headers = route.request.headers
    headers["x-repeater-context"] = "pw-python-lab"
    route.continue_(headers=headers)

context.route("**/*", tag_context)
```

验证：

```bash
ID=$(mitt history --context pw-python-lab --filter captured | jq -r '.id' | head -n1)
mitt show "$ID" --detail
```

## 5. Go

使用 `github.com/playwright-community/playwright-go`：

```go
package main

import (
	"log"

	"github.com/playwright-community/playwright-go"
)

const (
	proxyServer = "http://127.0.0.1:8080"
	contextName = "pw-go-lab"
)

func main() {
	pw, err := playwright.Run()
	if err != nil {
		log.Fatal(err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
		Proxy:    &playwright.Proxy{Server: proxyServer},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer browser.Close()

	context, err := browser.NewContext(playwright.BrowserNewContextOptions{
		ExtraHttpHeaders: map[string]string{"X-Repeater-Context": contextName},
		ServiceWorkers:   playwright.ServiceWorkerPolicyBlock,
	})
	if err != nil {
		log.Fatal(err)
	}

	page, err := context.NewPage()
	if err != nil {
		log.Fatal(err)
	}
	if _, err := page.Goto("https://app.example.test/"); err != nil {
		log.Fatal(err)
	}
	page.Fill(`input[name="q"]`, "widget")
	page.Press(`input[name="q"]`, "Enter")
	page.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
		State: playwright.LoadStateNetworkidle,
	})
}
```

动态注入（保留原 headers）：

```go
err = page.Route("**/*", func(route playwright.Route) {
	headers := route.Request().Headers()
	headers["x-repeater-context"] = contextName
	if err := route.Continue(playwright.RouteContinueOptions{Headers: headers}); err != nil {
		log.Printf("continue route: %v", err)
	}
})
if err != nil {
	log.Fatal(err)
}
```

验证：

```bash
ID=$(mitt history --context pw-go-lab --filter captured | jq -r '.id' | head -n1)
mitt show "$ID" --detail
```

## 6. `X-Repeater-Context` 注入方式

三种方式任选其一：

| 方式 | 代码位置 | 适合 |
|---|---|---|
| `extraHTTPHeaders` / `extra_http_headers` / `ExtraHttpHeaders` | `newContext` / `new_context` / `NewContext` | 一个 context 一个固定标签，最简单 |
| `context.route()` / `context.route` / context 路由 | context 级 | context 内按 URL/流程动态换标签 |
| 反向代理或客户端统一加 header | Playwright 之外 | 所有请求统一打标，不推荐替代前两种 |

行为细节：

- header 名大小写不敏感；`mitt` 取第一个非空值写入 `context`。
- 该 header 只用于历史标记，转发给上游前会被删除。
- 没有该 header 的流量 `context` 为空；`--context ""` 等同不按 context 过滤，不能用它筛出“无标签”条目。
- 用 `history --context <name>` 精确匹配，不是前缀或通配。

## 7. 闭环验证

Playwright 跑完一个场景后：

```bash
# 1. 找到该 context 的流量
mitt history --context pw-node-lab --filter captured

# 2. 查看请求/响应和 request_document
ID=$(mitt history --context pw-node-lab --filter captured | jq -r '.id' | head -n1)
mitt show "$ID" --detail

# 3. 预览改包
mitt send "$ID" --patch '{"headers":[["Accept","application/json"],["X-Test","playwright"]]}' --dry-run

# 4. 重放并对比父条目
OUT=$(mitt send "$ID" --patch '{"headers":[["Accept","application/json"],["X-Test","playwright"]]}' --diff-against)
NEW_ID=$(echo "$OUT" | jq -r '.id')
echo "$OUT" | jq '{status, changed: .diff_against.changed}'

# 5. 完整 diff
mitt diff "$ID" "$NEW_ID"
```

## 8. 常见坑

| 现象 | 原因 | 处理 |
|---|---|---|
| `ERR_CERT_AUTHORITY_INVALID` / 证书不受信任 | 浏览器信任库没有 mitt CA；Playwright 容器与 mitt 容器不共享信任库 | 在同一环境执行 `mitt ca install`；跨容器用 `mitt ca install --output` 导入浏览器环境；隔离 CI 可临时 `ignoreHTTPSErrors: true` |
| `history` 只有 `NO_MATCH`（66） | 代理没启动、流量没走代理、context 标签没注入或拼错 | 先 `mitt proxy status`；不缩窄过滤跑 `mitt history`；检查 `extraHTTPHeaders` / route 是否生效 |
| `history --filter mitm_error` 出现 `scope_denied` | 目标不在 `--allow-targets`，或命中 `--deny-targets` | 补全 host；`*.example.com` 不匹配 apex；deny 优先 |
| 某些站点抓不到，其他站点正常 | 默认 `--http2-alpn h1`，目标可能 h2-only | 用 `mitt show <mitm-error-id> --detail` 看 `reason`；重启代理加 `--http2-alpn h2,h1` |
| 代理启动报 `LOCKED`（69） | 同一 store 已有存活代理 | 复用现有代理；并行任务用不同 `--store`，或多端口 `--session-per-port` |
| 目标服务器看不到 `X-Repeater-Context` | 正常行为：mitt 会在转发前剥离 | 用 `mitt history --context` 查，不要指望上游回显 |
| 页面请求绕过代理 | 启动参数把 proxy 放错层，或 `bypass` 包含目标 | proxy 放在 `launch` 或 `newContext`；不要给目标设 bypass；确认没有覆盖 context 的代理配置 |
| Service Worker 拦截后请求不进代理 | 浏览器从 Service Worker 缓存返回响应 | context 设置 `serviceWorkers: 'block'`（Node/Python）或 `ServiceWorkers: playwright.ServiceWorkerPolicyBlock`（Go） |
| 多 context 历史混在一起 | 所有 context 用同一个标签，或都没打标签 | 每个 context 唯一标签，或改用 `--port-range` + `--session-per-port` |
| 多端口下读不到历史 | 历史在每端口独立目录 | `mitt --store <store>/ports/<port> history`；根 store 的 `proxy status` 只列 listener |
| 代理启动后其他机器也能连 | 默认 `0.0.0.0:8080` | 本机用 `--addr 127.0.0.1:8080`；共享环境必须 `--allow-targets` |
| 抓不到 HTTP/3 / QUIC 流量 | mitt 不实现 HTTP/3 MITM | 测试目标强制 HTTP/1.1 或 HTTP/2；必要时在 Chromium 加 `--disable-quic` |

## 9. 可选：外部编排

如果 Playwright 与 `mitt` 不在同一执行环境，编排层可以用 `docker exec <container> mitt ...` 或 SSH 包装命令。但这只是调用方式的包装；本指南的示例默认直接执行 `mitt ...`，代理协议和 Playwright 配置不因编排方式改变。
