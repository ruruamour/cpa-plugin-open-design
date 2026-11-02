# OpenDesign 插件说明

本仓库是 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) v0.4.3（commit `8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）的独立 fork。插件 ID 与 provider 为 `open-design`，当前版本 `0.1.0`。本 fork 不是 OpenDesign 官方发布，也不是 ClinePassBridge 的官方下游。

## 范围

- 实际 Provider 位于 `internal/opendesign`。
- `internal/bridge` 是保留的上游参考实现，不接入本插件的新二进制。
- 使用 CPA 公开的 host HTTP、translation 和 public RPC；不修改、不打包、不发布 CPA 宿主。
- 不包含 Cline quota、pricing、providers、`has` 能力过滤和自动推理探测等上游专有逻辑。

## 协议

OpenDesign 生产接口：

| 项目 | 值 |
| --- | --- |
| Base URL | `https://amr-link.open-design.ai/v1` |
| 认证 | `Authorization: Bearer <key>` |
| 模型列表 | `GET /models` |
| 对话 | `POST /chat/completions` |

插件固定上述官方 origin，拒绝自定义初始请求地址；后续重定向由 CPA 宿主处理。CPA 8.0.48 本地跨域 307 测试确认跳转会被跟随、`Authorization` 会被剥离，不能据此声称插件禁止所有重定向。请求使用 OpenAI 兼容的 Chat Completions 结构。

协议行为参考：

- [AstroQore/opendesign2api](https://github.com/AstroQore/opendesign2api)：独立网关 prior art。仅参考公开协议行为，未复制源码。
- [nexu-io/open-design 的生产配置](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/apps/daemon/src/integrations/vela.ts#L819-L842)：用于核对官方生产地址。

## 插件接口与数据

| 项目 | 值 |
| --- | --- |
| 管理接口 | `/v0/management/open-design` |
| 控制台 | `/v0/resource/plugins/open-design/console` |
| 默认数据目录 | `plugins/open-design-data` |
| 数据目录配置 | `data_dir` |

- API key 保存在 CPA 的 `auth-dir`，通过 `host.auth.save` 写入；插件设置不保存 key，状态接口不回显 key。
- 管理接口按 CPA 精确路由契约提供：`GET /credentials` 列出账户；`POST /credentials` 创建或更新，body 为 `{id?, label?, api_key?, disabled?}`；`DELETE /credentials?id=<id>` 按查询参数删除，不使用 `/credentials/{id}`。
- 模型列表由管理页调用 OpenDesign `GET /models` 获取，用户选择后保存为客户端模型到上游模型的映射。
- 模型映射保存在插件数据目录；获取的上游模型列表不持久化。容器部署时应持久化 CPA 插件目录、CPA 的 `auth-dir` 和该数据目录。
- 管理页沿用 CPA 管理认证。手动输入的管理密钥按 CPA 管理页的现有方式处理，不写入 OpenDesign key 存储。

## 模型映射

1. 在控制台添加 OpenDesign API key。
2. 点击获取模型，读取生产地址的 `GET /models`。
3. 勾选模型，保存客户端模型名与上游模型 ID 的映射。
4. 客户端向 CPA 的 `/v1/chat/completions` 发送请求，`model` 使用已保存的客户端模型名。

未配置的客户端模型名不会自动透传为上游模型 ID。

## 构建与安装

需要 Go 1.26、CGO 和 C 编译器。

```bash
# 运行管理页及保留的上游 Node 测试
make test-js

# 运行 Node + Go 测试
make test

# 生成 dist/open-design.so（Linux amd64，C shared ABI v1）
make build

# 构建并运行原生 ABI、本地假上游验证
make smoke

# 或使用 Dockerfile.build
make docker
```

安装到建议位置：

```bash
install -Dm755 dist/open-design.so plugins/linux/amd64/open-design.so
```

使用前在 CPA 配置中启用插件系统，并启用本插件：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    open-design:
      enabled: true
      data_dir: plugins/open-design-data
```

只设置 `plugins.enabled` 不会加载本插件，`plugins.configs.open-design.enabled` 必须为 `true`。

## 当前验证边界

- 真实上游验证仍受有效凭据限制：对生产地址仅执行过两次 `GET /models`，均返回 `401 invalid_api_key`（`api key not found`）。
- 尚未完成真实 `POST /chat/completions` 推理验证，因此不能标记“真实推理通过”。
- `go test -race ./...` 和 `go vet ./...` 已通过；Node 测试 79 项通过，含新管理页 16 项。
- 使用本地 CPA 8.0.48 和 loopback CONNECT/TLS 假上游验证插件加载、模型发现、JSON/SSE、工具调用及 usage；临时 CA 只传入测试子进程，不修改系统信任库或 hosts。控制台两条路径另有 146 项临时 Edge 浏览器检查。
- [验证脚本说明](../scripts/README.md) 区分 ctypes 假宿主、真实 CPA 管理路径、真实 CPA 配合本地假上游三种模式。它们验证宿主兼容性，真实 OpenDesign 的模型与推理仍需有效凭据。

## 发布 tag

- 发布 tag 使用 `open-design-v0.1.0`，不创建或覆盖上游 `v*` tag。
- 工作流触发 `open-design-v*`，去掉前缀后与 `internal/opendesign` 的 `Version` 和 registry 版本比对。
- CPA 插件商店解析 GitHub latest release 时要求 tag 以数字开头，因此从自有源安装时需显式指定该 tag/版本。

## 上游与许可

- 上游仓库：[xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)
- 许可证：[MIT](../LICENSE)，保留原版权声明并附带本 fork 的派生版权声明。
- `docs/` 下的其他文档保留为上游参考，本 fork 未逐篇改写。
