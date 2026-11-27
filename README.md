<div align="center">

# mitt

**An agent-first HTTP/HTTPS capture and replay CLI**

Local forward proxy with CONNECT MITM capture, byte-level request rewriting via `send --patch`, and response comparison via `diff`.
Every command emits structured JSON and ships with a machine-readable command contract.

[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=flat-square&logo=go)](https://go.dev)
[![Platform](https://img.shields.io/badge/platform-linux-lightgrey?style=flat-square&logo=linux)](https://github.com/C1ph3rX13/mitt)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue?style=flat-square)](LICENSE)

**English** · [简体中文](README_CN.md)

</div>

---

## ⚠️ Read this first

`mitt` is a MITM proxy. It mints leaf certificates for **any** HTTPS target it is asked to reach and decrypts that traffic.

- **Defaults to `0.0.0.0:8080`**: any process on the same host, network namespace, or subnet that can reach the port can use `mitt` as a proxy. This is not a "capture the browser only" sandbox.
- **Always scope your targets**: on production, shared hosts, or any untrusted network, pass `--allow-targets` explicitly. `--deny-targets` takes priority over allow. `*.example.com` matches subdomains only — it does **not** match the apex `example.com`.
- **Bind to loopback for local work**: `--addr 127.0.0.1:8080` blocks other machines, but does **not** replace scope — other processes on the same host can still use the proxy.
- **The CA private key is a trust anchor**: default location `~/.mitt/ca/ca.key.pem` (mode `0600`). Never commit it, never bake it into a public image, never share it. Whoever holds it can mint certificates trusted by that environment. The JSON output of `mitt ca init` **contains the private key** — keep it out of shared logs and CI logs.
- **Only test systems you are authorized to test.** Do not set `--allow-targets` to `*` or leave it empty to "get it running".
- **MITM failures never fall through to encrypted passthrough**: the connection is dropped and recorded as `mitm_error`. Use `history --filter mitm_error` to distinguish "nothing captured" from "the target had no traffic".

Safe startup template:

```bash
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "app.example.test,*.app.example.test" \
  --deny-targets "billing.example.test"
```

---

## Features

| | |
|---|---|
| **Capture** | Forward proxy with HTTPS CONNECT MITM; HTTP/1.1 and HTTP/2 (`--http2-alpn h2,h1`); multi-port listening (`--port-range`) |
| **Fidelity** | Records the exact bytes sent and received; `show --raw` replays them verbatim without re-serialization |
| **Rewrite** | RFC 7386 JSON Merge Patch against the request document; `--dry-run` validates before anything is sent |
| **Replay** | Replay from a history entry, or construct a request from a URL / raw file; spoof the TLS ClientHello fingerprint (`--tls-profile chrome\|firefox`) |
| **Compare** | `diff` reports status, header additions/removals/changes, unified diff for text, SHA-256 for binary |
| **Contract** | `mitt spec` self-describes the command surface, constraints, error codes, and output schemas; every error is JSON with `code` / `hint` / `details` |
| **Agent integration** | Skill docs are embedded in the binary; `mitt skills install` unpacks them into the host agent's skill directory |

---

## Install

Requires **Linux** (`internal/ca` and `internal/proxy` carry `//go:build linux`) and **Go 1.25+**.

**Build from source** (produces a static binary, `CGO_ENABLED=0`):

```bash
git clone https://github.com/C1ph3rX13/mitt.git
cd mitt
make build          # -> ./mitt
```

**`go install`:**

```bash
go install github.com/C1ph3rX13/mitt/cmd/mitt@latest
```

**Install to `/usr/local/bin`:**

```bash
make build
sudo ./install.sh              # override with DESTDIR=/tmp/x PREFIX=/usr/local
```

---

## Quick start: capture → rewrite → replay → compare

The flow below was verified against a real target. Replace `http://127.0.0.1:18080` with a target you are authorized to test.

### 1. Initialize and install the CA

```bash
mitt ca init        # reuses an existing valid CA
mitt ca install     # writes to the system trust store; syncs NSS when certutil is present
mitt ca status      # expect ca_exists=true, installed=true
```

`ca install` needs permission to write the system trust store (usually root). On non-Debian/Kali systems or inside containers it may fail; export the PEM instead and import it into the client manually:

```bash
mitt ca install --output /tmp/mitt-ca.pem
```

Linux Firefox uses its own per-profile certificate store and is not written automatically. To capture Firefox traffic:

```bash
certutil -A -d sql:$HOME/.mozilla/firefox/<profile> -n mitt -t "C,," -i ~/.mitt/ca/ca.pem
```

### 2. Start the proxy (terminal A)

```bash
mitt proxy start \
  --addr 127.0.0.1:8080 \
  --allow-targets "127.0.0.1" \
  --events
```

- Runs in the foreground. Stop it with `Ctrl-C` (SIGINT, exit code `130`) or `mitt proxy stop` from another terminal.
- `--events` streams each `captured` / `mitm_error` event to stdout as NDJSON; without it the proxy is silent.
- Starting a second proxy against the same data directory returns `LOCKED` (`69`).

### 3. Generate some traffic (terminal B)

```bash
curl -sS \
  -x http://127.0.0.1:8080 \
  --cacert "$HOME/.mitt/ca/ca.pem" \
  -H 'X-Repeater-Context: first-run' \
  http://127.0.0.1:18080/foo >/dev/null
```

`X-Repeater-Context` is recorded in history for grouping and **stripped before forwarding**, so it never reaches the upstream request.

### 4. Find and inspect the entry

```bash
mitt history --context first-run
mitt history --context first-run -f table

ID=$(mitt history --context first-run --filter captured | jq -r '.id' | head -n1)
mitt show "$ID" --detail
```

`history` is an NDJSON record stream, one entry per line. `show` returns the entry view by default (`request_line` is the line the client sent, `forwarded_request_line` is the line the next hop received); only an explicit `--detail` adds `request_document` and the body paths. No matches returns `NO_MATCH` (`66`).

### 5. Preview, then replay

```bash
PATCH='{"headers":[["Host","127.0.0.1:18080"],["Accept","text/html"]]}'

# Send nothing, write nothing: just render the final request document and a byte preview
mitt send "$ID" --patch "$PATCH" --dry-run

# Actually send; kind=replayed, parent_id=$ID, plus a diff_against summary
NEW_ID=$(mitt send "$ID" --patch "$PATCH" --diff-against | jq -r '.id')
```

`send` connects to the target directly and does **not** go through the capture proxy. `headers` is replaced wholesale, so write back every header you intend to keep.

### 6. Compare the responses

```bash
mitt diff "$ID" "$NEW_ID"
```

Reports status, header additions/removals/changes, and body differences: a unified diff for text, SHA-256 for binary, truncated past 1 MiB with `truncated=true`.

---

## Command surface

| Command | Purpose |
|---|---|
| `mitt ca init` | Generate a root CA or reuse the existing one |
| `mitt ca install` | Install into the system / NSS trust store; `--output <path>` exports the PEM instead |
| `mitt ca status` | Report whether the CA exists and is installed |
| `mitt proxy start` | Run the capture proxy in the foreground (`--addr` `--allow-targets` `--deny-targets` `--events` `--http2-alpn` `--port-range` `--session-per-port`) |
| `mitt proxy status` | Report whether the proxy is running |
| `mitt proxy stop` | Stop the running proxy |
| `mitt history` | List history entries (`--filter captured\|replayed\|sent\|mitm_error`, `--context`) |
| `mitt show <id>` | Show one entry's request and response (`--detail` `--raw`) |
| `mitt send [<id>]` | Send a request directly (exactly one of `<id>` / `--url` / `--from-file`; `--patch` `--dry-run` `--diff-against` `--tls-profile` `--schema`) |
| `mitt diff <idA> <idB>` | Compare the responses of two entries |
| `mitt skills install` | Unpack the embedded agent skill documentation |
| `mitt spec` | Print the machine-readable command contract |

The three `send` bases are mutually exclusive and exactly one is required; violating this returns `BASE_CONFLICT` (`2`). `--patch` accepts inline JSON, `@file`, or `-` (stdin).

**A request document has exactly one body representation**: at most one of `body` (UTF-8 text), `body_base64` (binary), and `body_file` (path) may be non-`null`. The same rule is published in the schema from `send --schema`, so a document accepted by `--dry-run` is the document `--patch` accepts.

**A history entry's body has exactly one interpretation**: the file must be exactly as many bytes as the record claims. Zero bytes and a missing file both read as an empty body; when the record claims `N>0` but the file is missing, short, or long, `show --raw`, `send <id>`, and `diff` return `INTERNAL_ERROR` (`1`) with `id`, `direction`, `path`, and `store` in `details` — commands that need bytes never silently treat lost bytes as an empty body.

---

## Output contract

- JSON by default. **Format** is a single switch, `-f json|table|yaml|md`; **shape** is selected by each command's declared mode flag (`show --detail`, `proxy start --events`, `send --dry-run`).
- `history` and `proxy start --events` are NDJSON record streams; every other command emits a single JSON object.
- Errors are always `{"error":{...}}` JSON on **stderr**, with structured context in `details` and a next step in `hint`.
- Exit codes follow sysexits:

| Exit | Meaning |
|---:|---|
| `0` | Success |
| `1` | Generic error (includes `UPSTREAM_TLS`, `INTERNAL_ERROR`) |
| `2` | Usage or argument error (`INVALID_REQUEST`, `INVALID_PATCH`, `BASE_CONFLICT`) |
| `66` | No input (`ENTRY_NOT_FOUND`, `NO_MATCH`) |
| `69` | Service unavailable (`NOT_RUNNING`, `LOCKED`, `ADDR_IN_USE`) |
| `73` | Destination exists with conflicting content (`SKILL_EXISTS`) |
| `75` | Temporary failure; safe to retry (`TEMP_FAILURE`) |
| `77` | Permission or scope denial (`SCOPE_DENIED`, `CLIENT_CERT_REJECTED`) |
| `78` | Configuration error (`CA_MISSING`, `CERT_INSTALL_FAILED`, `SKILL_INSTALL_FAILED`) |
| `130` | Interrupted by SIGINT |

**The single source of truth is the runtime contract**: what `mitt spec` prints is exactly the command surface, constraints, and error codes that execute. `skills/mitt/reference/commands.md` is a human-readable mirror of it, compared field by field in tests — documentation never defines the command surface in reverse.

---

## Data directory

Defaults to `~/.mitt/`, overridden by the global `--store <dir>` (`~` is expanded).

```
~/.mitt/
├── ca/               # ca.pem (certificate) / ca.key.pem (private key, 0600)
├── bodies/           # request / response bodies named by entry id
├── history.jsonl     # event stream, one entry per line
└── proxy.json        # runtime state of a running proxy; removed on stop
```

With `--session-per-port`, each port gets its own isolated data directory at `<store>/ports/<port>/`.

`--store` also determines the default CA path — overriding the store never falls back to the real `~/.mitt/ca`.

---

## Agent integration

`mitt` is designed for agent use: structured output, stable error codes, an introspectable contract.

The binary embeds agent skill documentation and unpacks it where the host discovers skills:

```bash
mitt skills install                          # default ~/.agents/skills/mitt/
mitt skills install --dest /path/to/skills   # custom skill root
mitt skills install --force                  # overwrite a modified older version
```

Identical content returns `unchanged` idempotently; differing content or extra files returns `SKILL_EXISTS` (`73`), and only `--force` replaces the whole tree.

---

## Development

```bash
make build    # static CGO_ENABLED=0 binary, -trimpath -ldflags="-w -s"
make test     # go test ./... with timeouts and a package parallelism cap
```

`make test` builds the binary first — the compiled artifact is the only seam the tests use. The test run carries three layers of resource protection: a per-test timeout, a package-level timeout, and a package parallelism cap, so proxy and container tests cannot stack up on one machine. Trust-store scenarios involving containers run through throwaway Docker containers and need a local Docker daemon.

```bash
make test GO_TEST_TIMEOUT=20m GO_TEST_PARALLEL=2
```

Code layout: `internal/proxy` proxy and MITM, `internal/session` history storage, `internal/entry` entry reading, `internal/replay` sending, `internal/diff` comparison, `internal/spec` command contract, `internal/output` output and error codes. `scripts/kali/Dockerfile.example` is a downstream reference sample (not a deliverable) showing how to bake the binary into a Kali image and trust the CA at build time.

---

## License

[Apache License 2.0](LICENSE)
