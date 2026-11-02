# OpenDesign 0.1.0 验证记录

## 测试环境与范围

- 基于 ClinePassBridge v0.4.3（`8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）派生，保留原历史与 MIT 版权。
- 新 Provider 位于 `internal/opendesign`，经原生 C shared ABI v1 接入；不需要 CPA 宿主补丁。
- 本地集成宿主：原版 CPA 8.0.48，commit `dbb70621`，Linux amd64。
- 构建工具：Go 1.26.4、CGO、GCC；Linux amd64 交付库在 Debian 12 构建，所需最高 GLIBC 符号版本为 2.34。其他系统的构建由发布工作流定义，本记录不将其计为本地已验证。

## 可重复的本地检查

```bash
go test -race -timeout 120s ./...
go vet ./...
node --test tests/*.test.cjs
make build
python3 scripts/smoke-abi.py dist/open-design.so
python3 scripts/smoke-abi.py dist/open-design.so --cpa-binary=/path/to/cli-proxy-api
python3 scripts/smoke-abi.py dist/open-design.so --cpa-live=/path/to/cli-proxy-api \
  --json-report /tmp/open-design-abi.json
```

已通过：Go 测试（包括新 Provider 的 80 个测试）、race 检查、vet、Node 测试 79/79（新管理页 16 项）、原生共享库构建。`--cpa-binary` 与 `--cpa-live` 均返回 PASS。

两个 unlink 权限失败测试在非 root Linux 下实际执行并通过；Debian 12 root 容器下先探测系统权限行为，因 root 可以绕过目录权限而精确跳过这两项，其余用例通过。

`--cpa-live` 使用 loopback CONNECT 代理、一次性 CA 和本地 TLS 假上游。证书信任仅传给 CPA 测试子进程；代理拒绝非测试主机，不修改系统信任库或 hosts。以下模型 ID 与响应均为测试夹具，不代表真实 OpenDesign 账户可用的模型。

真实宿主回归覆盖：

- 按管理页顺序添加凭据 → 获取上游模型 → 保存映射 → `/v1/models` 出现客户端别名。
- 非流式 JSON 与 SSE，模型别名转换、工具调用参数与 usage 保留。
- 停用时撤回模型，重新启用后恢复且可调用。
- 删除后 auth-dir 文件消失、模型列表收回别名、后续调用被拒绝。
- 401 与 429 在各自独立宿主进程中到达假上游并保留响应状态，避免宿主冷却掩盖测试。
- 跨域 307 的首跳确实携带 Bearer 且经过测试代理；CPA 8.0.48 跟随跳转，并在跨域跳转中剥离 `Authorization`。

原生 ABI 检查使用真实 CPA 字段大小写，回调异常会使检查失败。单元测试另覆盖同步 `auth.parse` 重入、卸载时取消阻塞请求、多个 choice 的 SSE 截断、配置并发保存与迟到模型响应。

## 管理页浏览器检查

使用独立临时 Microsoft Edge profile、本地假管理 API，对 resource console 与 management console 两条路径共完成 146 项检查，覆盖登录、账户增删启停、获取和保存映射、Key 输入清空、文本注入防护以及异步账户切换。

浏览器检查不是生产 CPA iframe 或真实 OpenDesign 服务测试；控制台 API 与宿主的衔接另由上述真实 CPA 集成检查覆盖。

## 真实 OpenDesign 验证

对 `https://amr-link.open-design.ai/v1/models` 使用提供的测试凭据请求两次，均返回 `401 invalid_api_key`，消息为 `api key not found`。

真实 `POST /chat/completions` 未验证。当前可以确认插件的原版宿主兼容性及本地完整请求链路，不能将本地假上游的 PASS 写成真实 OpenDesign 推理成功；真实验收需要有效凭据。
