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

假宿主按 CPA 真实线上契约工作，不做兼容放宽：`host.http.do` 的结果是 `StatusCode`/`Headers`/`Body`（该 SDK 结构体无 json tag），`host.http.do_stream` 与 `host.http.stream_read` 是 `status_code`/`stream_id`/`payload`/`done`（有显式 tag）。假宿主还实现 `host.http.operation_open` / `host.http.cancel`，并在 `shutdown` 前要求 `callback_failures` 为空——回调里抛出的异常会被记为失败，而不是被 ctypes 静默吞掉后还报 PASS。


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

按控制台真实顺序跑完整用户流程，每一步都断言：

1. `POST /credentials` 添加 key（响应与后续路由都不回显 key）；
2. `GET /upstream-models` 经真实宿主 HTTP 发现上游模型；
3. `POST /models` 保存映射后，`GET /v1/models` 必须立刻出现别名（宿主注册被刷新）；
4. `/v1/chat/completions` 非流式（`tools` 转发 + `usage`）与流式 SSE 多帧，模型别名要还原成客户端名；
5. `disabled:true` 后 `/v1/models` 收回别名，`disabled:false` 后恢复且可再次调用；
6. 跨源重定向（**在删除之前**跑，凭据必须仍然可用）：假上游对 `/models` 回 307 指向另一个测试 origin，观察宿主是否把 `Authorization` 带过去；
7. `DELETE /credentials?id=` 后不可调用、且不应再出现在模型列表；
8. 上游 401 与 429 **各起一个独立 CPA 进程**（真实 401 会让 CPA 冷却凭据，同进程里第二个用例会被宿主以 `auth_unavailable` 挡下、证明不了插件行为），每个用例都断言请求确实到达假上游。

实测结论（CPA v8.0.48）：跨源 307 **会被跟随**，但 `Authorization` **会被剥掉**（`host_http_redirect.verdict = "Authorization stripped across origin"`），即宿主不会把 bearer key 带到别的 origin；`registry.npmjs.org:443` 的 CONNECT 被代理拒绝，证明没有其它出站。

重定向探针有硬前置条件：**首跳必须真的到达假 origin 且带上 bearer key**（`first_hop.authorization_present`、以及目标 origin 出现在 `proxy.connects` 里）。不满足时探针直接记 `status: "FAIL"` 并写明原因，绝不会把「本地就被拒掉」解释成「宿主拒绝跟随重定向」。探针自带 `status` 并纳入总判定，任一 leg 失败整轮即 FAIL。

每个错误用例都在独立进程里跑，因此不存在「先跑 401 把凭据冷却掉、后面用例被宿主拦下」的串扰；报告里每条都带 `reached_upstream`，为 `false` 就说明该用例没测到真东西。

