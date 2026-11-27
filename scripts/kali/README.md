# Kali 参考镜像

这是一个**下游参考样例**（not a deliverable），不是 mitt 的正式交付物，也不参与 mitt 的发布流程。仓库的正式交付物仍然只有 `mitt` 静态二进制。

样例把静态二进制烘入 Kali 镜像，在构建期用 `mitt skills install` 展开内嵌的 Skill 文档，并执行 `mitt ca init && mitt ca install`，让容器启动后已经有一个受信任的 CA 和被 Agent 发现的 Skill。

## 前置

```bash
make build
docker pull kalilinux/kali-rolling
```

如果本地使用其他 Kali 衍生镜像做验收，可设置 `MITT_KALI_IMAGE=<image>`；`scripts/kali/Dockerfile.example` 的默认基础镜像仍是 `kalilinux/kali-rolling`。

`scripts/kali/Dockerfile.example` 默认以 `kalilinux/kali-rolling` 为基础镜像。Kali rolling 基础镜像不自带 `ca-certificates`，所以样例先安装 `ca-certificates` 和 `libnss3-tools`；后者提供 `certutil`，让 `ca install` 的 NSS best-effort 路径也能执行。

## 构建

在仓库根目录执行：

```bash
docker build \
  -f scripts/kali/Dockerfile.example \
  -t mitt-kali:local \
  .
```

构建期会：

1. 安装 `ca-certificates` / `libnss3-tools`；
2. 把 `mitt` 复制到 `/usr/local/bin/mitt`；
3. 执行 `mitt skills install`，把内嵌的 Skill 展开到 `/root/.agents/skills/mitt/`；
4. 执行 `mitt ca init >/dev/null && mitt ca install`。

`ca init` 的 JSON 输出包含 `ca_key_pem`，样例把它重定向到 `/dev/null`，避免私钥进入构建日志。

## 验证

```bash
docker run --rm mitt-kali:local sh -c 'command -v mitt; test -f /root/.agents/skills/mitt/SKILL.md; mitt skills install; mitt ca status --json'
```

期望：

- `command -v mitt` 输出 `/usr/local/bin/mitt`
- `/root/.agents/skills/mitt/SKILL.md` 存在，且 `skills install` 显示 `unchanged`
- `ca status` 的 `ca_exists` 和 `installed` 都为 `true`
- `install_targets` 同时包含 `system` 和 `nss`
- `/root/.mitt/ca/ca.key.pem` 权限为 `0600`

本地没有 Kali 镜像时，可以临时换基础镜像做构建实验（这不是正式验收路径）：

```bash
docker build \
  --build-arg BASE_IMAGE=debian:bookworm-slim \
  -f scripts/kali/Dockerfile.example \
  -t mitt-kali:local \
  .
```

## 安全边界

- **镜像包含 CA 私钥**：`/root/.mitt/ca/ca.key.pem` 是信任锚。任何拿到该镜像的人都能签发该环境信任的任意站点证书。The CA private key is a trust anchor; anyone with the image can mint certificates trusted by that environment.
- **不要把镜像推到公开 registry**。公开分发是否安全由下游自行控制；本项目仓库只提供 Dockerfile 样例，不提交 CA 私钥，也不发布镜像。Never push this image to a public registry.
- 每次重新构建都会生成新的 CA（除非复用构建缓存）。需要稳定 CA 时，应由下游在镜像构建期显式挂载/注入受控 CA。
- 代理仍默认监听 `0.0.0.0:8080`；启动时请加 `--allow-targets` 或 `--addr 127.0.0.1:8080`。

## 验收

这个样例由 `test/cli/kali_acceptance_test.go` 中的测试覆盖：Dockerfile 与本文档的契约、参考镜像可构建且信任 CA、以及仓库不跟踪 CA 私钥。运行方式见仓库根目录 README 的"开发"一节。
