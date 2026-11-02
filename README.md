# OpenDesign CPA 插件

把 OpenDesign 的 OpenAI 兼容接口接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI)。

> 本仓库是 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) v0.4.3（commit `8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）的独立 fork，保留 `upstream` 远程用于对照和同步。插件 ID 与 provider 为 `open-design`，当前版本 `0.1.0`。这不是 OpenDesign 官方发布，也不代表 ClinePassBridge 上游。

## 功能

- 管理 OpenDesign API key，并从 CPA 的 `auth-dir` 读取和保存凭据。
- 通过管理页获取 `GET /models` 的模型列表，选择后保存为客户端模型到上游模型的映射。
- 提供管理接口 `/v0/management/open-design` 和控制台 `/v0/resource/plugins/open-design/console`。
- 通过 CPA 公开的 host HTTP / translation / public RPC 发起请求，支持流式与非流式 Chat Completions。
- 不包含 Cline quota、pricing、providers、`has` 能力过滤和自动推理探测等上游专有逻辑。

实际 Provider 实现在 `internal/opendesign`。`internal/bridge` 是保留的上游参考实现，不接入本插件的新二进制。

## 安装

需要支持动态库插件和 C shared ABI v1 的原版 CPA。先在 CPA 配置中启用插件，并按插件 ID 打开配置项：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    open-design:
      enabled: true
      # 可选，默认 plugins/open-design-data
      data_dir: plugins/open-design-data
```

`plugins.enabled` 只打开插件系统，实际加载仍需 `plugins.configs.open-design.enabled: true`。

把构建产物安装到建议位置：

```bash
install -Dm755 dist/open-design.so plugins/linux/amd64/open-design.so
```

然后：

1. 打开 `/v0/resource/plugins/open-design/console`。
2. 在凭据管理中添加 OpenDesign API key。
3. 获取模型列表，勾选模型并保存映射。
4. 在客户端填入 CPA 的 API 地址和 CPA 客户端 API key，使用已保存的客户端模型名调用 `/v1/chat/completions`。

管理页需要 CPA 管理认证。插件数据目录默认为 `plugins/open-design-data`，可通过 `data_dir` 修改；容器部署请持久化插件目录、CPA 的 `auth-dir` 和该数据目录。

管理接口按 CPA 路由契约提供：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /v0/management/open-design/credentials` | 列出账户（不回显 key） |
| `POST /v0/management/open-design/credentials` | 创建或更新账户，body 为 `{id?, label?, api_key?, disabled?}` |
| `DELETE /v0/management/open-design/credentials?id=<id>` | 按查询参数删除账户 |
| `GET /v0/management/open-design/status` | 插件状态与模型映射 |
| `GET /v0/management/open-design/upstream-models` | 获取 OpenDesign `GET /models` 候选 |
| `GET` / `POST /v0/management/open-design/models` | 读取或保存模型映射 |

## 安全模型

- 默认固定 OpenDesign 官方生产地址 `https://amr-link.open-design.ai/v1`，不接受自定义上游地址。后续 HTTP 重定向由 CPA 宿主处理；本地 CPA 8.0.48 的跨域 307 测试确认其会跟随跳转并去掉 `Authorization`，这不等于插件禁止所有重定向。
- API key 只保存在 CPA 的 `auth-dir`，通过 `host.auth.save` 写入；插件设置中不保存 key，状态接口不回显 key。
- 模型列表和映射保存在插件数据目录。日志、截图和提交中不要包含 key。
- 当前 fork 使用公开 ABI，不修改、不打包也不发布 CPA 宿主。

## 模型映射

管理页从 OpenDesign `GET /models` 获取可用模型。保存映射后，插件同步刷新 CPA 的凭据模型注册；客户端请求 `model` 填客户端名称，插件将其转换为已保存的上游模型 ID，再调用 `POST /chat/completions`。未保存的模型名不会自动透传。

## 构建

需要 Go 1.26、CGO 和 C 编译器。仓库提供 Makefile：

```bash
make test-js   # 管理页与保留的上游 Node 测试
make test      # Node + Go 测试
make build     # 生成 dist/open-design.so
make smoke     # 构建后运行原生 ABI 与本地假上游验证
make docker    # 使用 Dockerfile.build 构建 Linux amd64 共享库
```

也可以直接构建：

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -buildmode=c-shared \
  -ldflags='-s -w' -o dist/open-design.so ./cmd/passbridge
```

产物是 `dist/open-design.so`（Linux amd64，C shared ABI v1）。

发布 tag 使用 `open-design-v0.1.0` 形式，避免与 fork 保留的上游 `v*` tag 冲突。CPA 插件商店的 latest-release 解析要求 tag 以数字开头，因此从自有源安装时需显式指定该 tag/版本，`github-release` 的自动“取最新”不适用于本 fork。

## 当前验证边界

- 真实上游验证仍受有效凭据限制：目前对生产地址只执行过两次 `GET /models`，均返回 `401 invalid_api_key`（`api key not found`）。
- 尚未完成真实 `POST /chat/completions` 推理验证，不能据此标记“真实推理通过”。
- `go test -race ./...`、`go vet ./...` 通过；Node 测试 79 项通过，其中 16 项覆盖新管理页。
- 独立临时 Edge 浏览器对两条控制台路径完成 146 项检查，覆盖账户增删启停、模型映射、密钥输入清空、文本注入防护和迟到响应。
- 本地 CPA 8.0.48 已加载共享库；通过仅允许测试主机的 loopback CONNECT 代理和临时 CA，验证模型发现、JSON/SSE、工具调用及 usage。所有这些请求使用假凭据和本地假上游，操作方法见 [验证脚本](scripts/README.md)。

## 文档与来源

- [OpenDesign 插件说明](docs/open-design.md)
- `docs/` 下的其他旧文档保留为上游参考，本 fork 未逐篇改写。
- 上游：[xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)
- 协议参考：[AstroQore/opendesign2api](https://github.com/AstroQore/opendesign2api)（独立网关 prior art，仅参考公开协议行为，未复制源码）
- OpenDesign 官方生产配置参考：[nexu-io/open-design vela.ts](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/apps/daemon/src/integrations/vela.ts#L819-L842)

## 许可

[MIT License](LICENSE)。原项目版权归 xiao-qiu-qiu；本 fork 的修改部分版权归 ruruamour。
