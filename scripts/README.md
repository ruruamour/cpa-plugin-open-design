# scripts/ 本地验证脚本

本目录只放本地离线验证脚本，不属于插件产物，也不进入发布包。

`0.4.0` 的 ctypes 与原版 CPA 假上游冒烟同时覆盖日志／统计、`/config`、`/runtime`、`/model-test`、`/model-coverage` 和 `/error-rules`。`--cpa-live` 还包含一段双账号真实宿主轮换验证：假上游按凭据返回生产 400 `tier_limit_exceeded`，宿主必须按规则换到另一账号并返回 200，冷却后跳过失败账号，`stop` 规则下不换号。所有账户与请求均为本地 fixture，验收记录见 `docs/verification.md`。

## `check-console-browser.py` — 紧凑凭据卡片的真实浏览器检查

需要本机 Python `playwright` 与 Microsoft Edge／Chromium。默认优先使用 `microsoft-edge`，也可显式指定路径：

```bash
python3 scripts/check-console-browser.py
python3 scripts/check-console-browser.py --browser /usr/bin/google-chrome --out /tmp/open-design-console-browser
```

脚本将 `ui.go` 同样拼接的控制台交给隔离浏览器，所有网络请求均拦截为本地合成 fixture，不读真实凭据、不请求生产或官方接口。覆盖 1440px／390px、明暗主题、默认折叠、键盘展开、重绘及翻页保留、串行 pending／loading、离开停止、60 秒快照跳过、真实零／100% 窗口、失败保留旧值、无横向溢出及 JS 错误。截图和 `result.json` 写到 `--out`，不属于插件发布包。

## `smoke-abi.py` — 原生 C ABI v1 冒烟

用 `ctypes` 真正 `dlopen` `dist/open-design.so`，按 CPA `internal/pluginhost/loader_unix.go` 的方式调用唯一导出 `cliproxy_plugin_init`，并向插件提供宿主回调函数表：

| 宿主回调 | 覆盖内容 |
| --- | --- |
| `host.auth.save` / `host.auth.list` | 凭据写入假 `auth-dir`、导出凭据列表 |
| `host.http.do` | 缓冲式请求（`GET /models`、非流式 `POST /chat/completions`）；结果字段按 CPA 真实大小写 `StatusCode`/`Headers`/`Body` |
| `host.http.do_stream` / `stream_read` / `stream_close` | 流式请求，多 chunk，含拆包的 SSE 帧；结果字段为 CPA 的 `status_code`/`stream_id`/`payload`/`done`（这两类 RPC 在 CPA 里有显式 json tag，与 `host.http.do` 不同） |
| `host.stream.emit` / `host.stream.close` | 下游帧输出与终止 |

上游是 `127.0.0.1` 上的假 OpenAI 兼容服务，**不访问外网**。`host.http.*` 收到的 URL 只允许两个固定 origin：推理 `https://amr-link.open-design.ai/v1/`（插件不允许改 base_url）与额度 `https://amr-api.open-design.ai/`；假宿主只在本机 TCP 上按 `Host` 把这两个 origin 改投到 loopback 端口。

断言重点：

- ABI v1 初始化、`plugin.register` 身份（`OpenDesign` / 从 `internal/opendesign/types.go` 动态读取的当前 `Version`）、`management.register` 路由（含 `/error-rules` 四条路由与额度暂停路由）；
- 假 Key `sk-amr-test-fixture` 只经 `host.auth.save` 落到 auth-dir，`/status`、`/credentials`、错误信封都不回显；
- `auth.parse`、`model.for_auth` 的凭据导入与模型映射；
- `executor.execute`：上游收到 `model=deepseek-v4.1-flash`（别名映射生效）、`Authorization: Bearer <key>`、`tools` 未被丢弃，返回 JSON 含 `usage`；
- `executor.execute_stream`：4 个多 chunk 帧、`delta`/`finish_reason`/`usage` 完整，`Metadata.request_path=/v1/chat/completions` 时输出**裸 JSON 帧**（不加 `data:` 前缀，对应 CPA Chat Completions 语义）；
- 同一条流在 `request_path=/v1/messages` 时输出**带 `data: ...\n\n` 的 SSE 帧**（对应 CPA Claude 路由语义）；
- 上游 401/429 经 RPC 信封保留真实 `http_status`，且不泄漏 Key；
- 流结束后 `host.http.stream_close` 与 `host.stream.close` 都被调用，`shutdown` 幂等；quiesce 后动态路由均为 503，ABI shutdown 后调用安全失败，清空后的日志快照为 `[]`；
- `management_v03`／`host_management_v03`：错误规则默认策略读取、严格校验（未知字段／重复键／无效正则／超量一律 400）、保存后落盘 `data/error-rules.json` 并逐账号写入 `auths/<id>.json` 的 `request_scoped_errors`、`sync` 与 `reset` 往返一致；配置严格校验与往返保存、固定上游与宿主代理／路由只读、runtime 与日志读取不发上游请求、官方模型级覆盖传真实 `modelId` 且与共享额度分开缓存、60 秒节流、显式模型测试只发一次有 16 token 上限的非流式请求、只返回 usage 与元数据、日志 allowlist／null usage／统计／清空；
- 额度（`quota_host`）：未配置 controlKey 的凭据 GET/refresh 都是 `not_configured` 且**零**上游请求；配置后普通 GET 只读缓存，`refresh=1` 才各取一次 `wallet/balance` 与 `workspace/{id}/billing/funding-check`（带 control bearer），响应形状按官方 `WorkspaceBillingPreflight` 严格逐字段校验（含 `modelId:null`、`updatedAt:null`、窗口 `forecast=insufficient_samples`）；立即再 refresh 命中 60 秒节流、不联网；`clear_billing` 后旧缓存不可用且不再联网，推理 key 保留。

额度断言只做「首次样本 = `insufficient_samples`」这类不需要等待的判断；真正的预测要两个相隔 ≥60 秒的成功样本，由 Go 侧 fake clock 单测覆盖，冒烟脚本不会为了它 sleep 一分钟。

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
2. 代理只允许 `amr-link.open-design.ai:443` 与 `amr-api.open-design.ai:443`，其余一律 403 并记入 `connect_refused`；重定向探针只在探针执行期间临时放行 `redirect-probe.example.test:443`，结束后恢复原 allowlist（报告里 `allowlist_restored` 为硬性要求）；
3. CONNECT 隧道接到本机 TLS 假上游，证书由一次性 CA 签发（SAN 含两个 OpenDesign 主机名与探针主机名，脚本用 `openssl x509` 读回并逐项校验）；
4. `SSL_CERT_FILE` 只设在 CPA 子进程环境里，**不动** `/etc/hosts`、系统信任库和系统代理；
5. 发请求用 CPA 客户端 key（`/v1/*`）和管理 key（`/v0/*`），凭据仍是 `sk-amr-test-fixture`。

按控制台真实顺序跑完整用户流程，每一步都断言：

1. `POST /credentials` 添加 key（响应与后续路由都不回显 key）；
2. `GET /upstream-models` 经真实宿主 HTTP 发现上游模型；
3. `POST /models` 保存映射后，`GET /v1/models` 必须立刻出现别名（宿主注册被刷新）；
4. `/v1/chat/completions` 非流式（`tools` 转发 + `usage`）与流式 SSE 多帧，模型别名要还原成客户端名；
5. `disabled:true` 后 `/v1/models` 收回别名，`disabled:false` 后恢复且可再次调用；
6. 跨源重定向（**在删除之前**跑，凭据必须仍然可用）：假上游对 `/models` 回 307 指向另一个测试 origin，观察宿主是否把 `Authorization` 带过去；
7. 额度流程（`host_quota`，在删除之前跑）：旧凭据 `not_configured` 零联网 → 空 `label`/`api_key` 更新保留原值 → 写入 controlKey/workspaceId（只落 auth-dir，status/credentials/models/console/`/v1/models`/quota 及客户端响应都不回显）→ 首次 `refresh=1` 校验官网形状与真实数字/窗口并断言只发生 2 次、均带 control bearer 且经 CONNECT 与 TLS、不带 `modelId` → 普通 GET 与立即 refresh 走缓存且 `next_refresh_at` 保持 → `clear_billing` 后缓存清空、不再联网，推理 key 与别名仍可用（再次真实推理返回 200）→ 另外用独立临时凭据覆盖 401/403/畸形响应被分别归类为 `unauthorized`/`forbidden`/`malformed`，`data`/`fetched_at` 保持 null，不宽松接受；
8. `DELETE /credentials?id=` 后不可调用、且不应再出现在模型列表，`GET /quota` 对已删除凭据返回 404；
9. 上游 401 与 429 **各起一个独立 CPA 进程**（真实 401 会让 CPA 冷却凭据，同进程里第二个用例会被宿主以 `auth_unavailable` 挡下、证明不了插件行为），每个用例都断言请求确实到达假上游；
10. 并发额度暂停（`host_quota_concurrent`，独立进程）：同一推理 key 两个别名指向两个上游模型，假上游把慢请求（`glm-5.3-flash`）挂住、让快请求（`deepseek-v4.1-flash`）先回结构化 402 建立暂停。在途窗口内官方 `funding-check` 对两个模型都回 `available`，但 `POST /quota-pause/recheck` 必须 409（`在途`）且**零**次官方查询、暂停仍在；放行慢请求后，它的迟到 402 必须**合并**出 `model_ids=[deepseek-v4.1-flash, glm-5.3-flash]`、`multiple_models=true`。随后一模型 `available`／另一模型 `insufficient` 的混合答案仍必须 409 并保持暂停（且两个模型都被查过），只有两个模型都新鲜 `available` 才 200 解除并清空磁盘 block。证据等待 `quotaRecoveryProven` 的 5 秒证据边界，不放宽插件规则；

实测结论（CPA v8.0.48）：跨源 307 **会被跟随**，但 `Authorization` **会被剥掉**（`host_http_redirect.verdict = "Authorization stripped across origin"`），即宿主不会把 bearer key 带到别的 origin；`registry.npmjs.org:443` 的 CONNECT 被代理拒绝，证明没有其它出站。

重定向探针有硬前置条件：**首跳必须真的到达假 origin 且带上 bearer key**（`first_hop.authorization_present`、以及目标 origin 出现在 `proxy.connects` 里）。不满足时探针直接记 `status: "FAIL"` 并写明原因，绝不会把「本地就被拒掉」解释成「宿主拒绝跟随重定向」。探针自带 `status` 并纳入总判定，任一 leg 失败整轮即 FAIL。

每个错误用例都在独立进程里跑，因此不存在「先跑 401 把凭据冷却掉、后面用例被宿主拦下」的串扰；报告里每条都带 `reached_upstream`，为 `false` 就说明该用例没测到真东西。

## `--cpa-quota-snapshot` — 额度暂停的旧文件回放

```bash
python3 scripts/smoke-abi.py dist/open-design.so --cpa-quota-snapshot=/tmp/cpa-host-stock-quota-check
```

只跑隔离的 auth 文件有序回放，不启动常规 `--cpa-live` 流程。脚本把旧 `pre-pause` 快照和旧 `paused` 快照按顺序写进临时 auth-dir，断言暂停不会复活、解除修订不会丢失、重启后仍是未暂停。回放用临时 fixture auth-dir 的权限短暂门闩住修复写入，恢复权限后要求收敛；该模式不测试 CPA 原生并发写归属，不代表生产验收。

`--cpa-snapshot-ungated` 改用不带权限门闩的原子替换（必须与 `--cpa-quota-snapshot` 同用）；如果插件的修复先于观察写入，结果可能不确定。两种模式都只用 `sk-amr-test-fixture`／`test-od-control-key-fixture-*`，只访问 `127.0.0.1` 假上游。

