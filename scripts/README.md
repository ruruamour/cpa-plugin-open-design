# scripts/ 本地验证脚本

本目录只放本地离线验证脚本，不属于插件产物，也不进入发布包。

## `smoke-abi.py` — 原生 C ABI v1 冒烟

用 `ctypes` 真正 `dlopen` `dist/open-design.so`，按 CPA `internal/pluginhost/loader_unix.go` 的方式调用唯一导出 `cliproxy_plugin_init`，并向插件提供宿主回调函数表：

| 宿主回调 | 覆盖内容 |
| --- | --- |
| `host.auth.save` / `host.auth.list` | 凭据写入假 `auth-dir`、导出凭据列表 |
| `host.http.do` | 缓冲式请求（`GET /models`、非流式 `POST /chat/completions`）；结果字段按 CPA 真实大小写 `StatusCode`/`Headers`/`Body` |
| `host.http.do_stream` / `stream_read` / `stream_close` | 流式请求，多 chunk，含拆包的 SSE 帧；结果字段为 CPA 的 `status_code`/`stream_id`/`payload`/`done`（这两类 RPC 在 CPA 里有显式 json tag，与 `host.http.do` 不同） |
| `host.stream.emit` / `host.stream.close` | 下游帧输出与终止 |

上游是 `127.0.0.1` 上的假 OpenAI 兼容服务，**不访问外网**。`host.http.*` 收到的 URL 必须仍是固定 origin `https://amr-link.open-design.ai/v1/`（插件不允许改 base_url），假宿主只在本机 TCP 上把该 origin 改投到 loopback 端口。

断言重点：

- ABI v1 初始化、`plugin.register` 身份（`OpenDesign` / `0.1.0`）、`management.register` 路由；
- 假 Key `sk-amr-test-fixture` 只经 `host.auth.save` 落到 auth-dir，`/status`、`/credentials`、错误信封都不回显；
- `auth.parse`、`model.for_auth` 的凭据导入与模型映射；
- `executor.execute`：上游收到 `model=deepseek-v4.1-flash`（别名映射生效）、`Authorization: Bearer <key>`、`tools` 未被丢弃，返回 JSON 含 `usage`；
- `executor.execute_stream`：4 个多 chunk 帧、`delta`/`finish_reason`/`usage` 完整，`Metadata.request_path=/v1/chat/completions` 时输出**裸 JSON 帧**（不加 `data:` 前缀，对应 CPA Chat Completions 语义）；
- 同一条流在 `request_path=/v1/messages` 时输出**带 `data: ...\n\n` 的 SSE 帧**（对应 CPA Claude 路由语义）；
- 上游 401/429 经 RPC 信封保留真实 `http_status`，且不泄漏 Key；
- 流结束后 `host.http.stream_close` 与 `host.stream.close` 都被调用，`shutdown` 幂等。

用法：

```bash
make build   # 或 CGO_ENABLED=1 go build -buildmode=c-shared -o dist/open-design.so ./cmd/passbridge
python3 scripts/smoke-abi.py dist/open-design.so
python3 scripts/smoke-abi.py dist/open-design.so --json-report /tmp/open-design-abi.json
```

**大小写必须与 CPA 一致，否则就是自证。** CPA `sdk/pluginapi/types.go` 的 `HTTPResponse` 没有 json tag，所以 `host.http.do` 线上是 `StatusCode`/`Headers`/`Body`；而 `internal/pluginhost/host_callbacks.go` 的 `rpcHostHTTPStreamResponse`、`rpcHostHTTPStreamReadResponse` 有显式 tag，是 `status_code`/`stream_id`/`payload`/`done`。本脚本默认按真实字段发，因此能暴露字段名写错的问题；`--allow-snake-case-host` 会退回插件自己的字段名，用于在 core 修复期间继续验证 ABI 其余部分（此时结果不再代表真实宿主行为）。

退出码 0 为 PASS，非 0 为 FAIL（断言信息即失败原因）。临时 data/auth 目录用 `mkdtemp` 创建、`shutil.rmtree` 清理，不使用 `rm -rf`；不发 SSH、不读生产配置、不使用真实 Key 或真实推理。

## `--cpa-binary` — 真实本地 CPA 宿主（离线路由）

```bash
python3 scripts/smoke-abi.py dist/open-design.so --cpa-binary=/tmp/cpa-v8.0.48-audit.vMdl5Y/cli-proxy-api
```

在临时目录中生成一次性 `config.yaml`（随机端口、`management.secret-key`、独立 `auth-dir` 和插件 `data_dir`），把 `.so` 复制到 `plugins/linux/amd64/`，启动原版 CPA 并只走**无外网副作用**的路径：

- 真实 `dlopen` 加载 + `plugin.register`，读 `/v0/management/plugins` 确认 `registered` 与 `effective_enabled`；
- `GET /status` 与 `X-Cpa-Version`（实测 8.0.48）；
- `POST /credentials` → key 实际落盘到 CPA `auth-dir`；
- `POST /models` → `/status` 读回映射；
- `DELETE /credentials?id=...` → auth-dir 内文件被删除。

`/upstream-models`、`executor.execute`、`executor.execute_stream` 会真正打到 `amr-link.open-design.ai`，因此该模式**故意跳过**（报告里列在 `skipped_online`）；这三条仍由默认的 ctypes + loopback 假上游模式覆盖。

已知本机可用二进制：

| 路径 | 版本 |
| --- | --- |
| `/tmp/cpa-v8.0.48-audit.vMdl5Y/cli-proxy-api` | 8.0.48，commit `dbb70621`（默认值） |
| `/tmp/cpa-audit-gny4ig42/v8.0.48/CLIProxyAPI-8.0.48/` | 对应源码树，用于核对 ABI/RPC 契约 |

两处都在 `/tmp`，属于本地临时产物，可能被清理；`--cpa-binary=` 可指向任意支持动态库插件的原版 CPA。

## `--cpa-live` — 真实宿主 + 本地假上游（端到端、仍无外网）

```bash
python3 scripts/smoke-abi.py dist/open-design.so --cpa-live
```

`--cpa-binary` 只能覆盖不联网的路径，因为 `/upstream-models` 和 `/chat/completions` 会真的打到 `amr-link.open-design.ai`。本模式不改 core（base_url 仍固定在官方 origin），而是纯用 CPA 自己的配置把出站流量接到本机：

1. `requests.proxy-url: http://127.0.0.1:<port>` —— 只监听 loopback 的 HTTP CONNECT 代理；
2. 代理只允许 `amr-link.open-design.ai:443`（重定向探针额外放行 `redirect-probe.example.test:443`），其余一律 403 并记入 `connect_refused`；
3. CONNECT 隧道接到本机 TLS 假上游，证书由一次性 CA 签发（SAN 含上述两个主机名）；
4. `SSL_CERT_FILE` 只设在 CPA 子进程环境里，**不动** `/etc/hosts`、系统信任库和系统代理；
5. 发请求用 CPA 客户端 key（`/v1/*`）和管理 key（`/v0/*`），凭据仍是 `sk-amr-test-fixture`。

覆盖：`GET /models` → 上游模型列表；`POST /v1/chat/completions` 非流式 200（`tools` 转发 + `usage`）；流式 SSE 多帧；上游 401/429 是否透传；以及**重定向行为**——让假上游对 `/models` 回 307 指向另一个 origin，观察宿主的跨源重定向策略。

实测结论（CPA v8.0.48）：跨源 307 **会被跟随**，但 `Authorization` **会被剥掉**（`host_http_redirect.verdict`），即宿主不会把 bearer key 带到别的 origin；`registry.npmjs.org:443` 的 CONNECT 被代理拒绝，证明没有其它出站。

注意：一次 401 会让 CPA 冷却该凭据，因此 401 放在最后；如果前面的主链路被 core bug 挡住（`core_blocked_legs` 非空），状态码这一段会记为 `SKIPPED` 而不是假通过。

## 当前已知的 core 问题（脚本会如实报出来）

`host.http.do` 的结果被插件按 `status_code` 解码，而 CPA 实际发的是 `StatusCode`，于是状态永远为 0、`Body` 也拿不到。表现：

```bash
$ python3 scripts/smoke-abi.py dist/open-design.so
FAIL: ... the plugin is not decoding CPA's host.http.* result casing ...
upstream-models returned 502: b'{"error":"{\"object\": \"list\", ...}"}'
```

即上游返回的 200 正文被原样包进 502 —— 请求链路本身是通的，只是结果没解出来。真宿主模式同样复现（`--cpa-live` 的 `host_upstream_models` 记为 `BLOCKED_BY_CORE_BUG`，`origin_returned_200: true` 与 `origin_saw_bearer: true` 说明假上游确实收到并正确鉴权）。

受影响的是所有走 `host.http.do` 的路径：`GET /models`（上游模型列表）和非流式的 `executor.execute`。`host.http.do_stream` 系用 snake_case 是对的，因此纯流式路径不受影响。`.so` 里 `upstreamCall` 结构需要改成 PascalCase（或同时接受两种），由 core owner 处理；本脚本不修 core。
