# OpenDesign CPA 插件

把 OpenDesign 的 OpenAI 兼容接口接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI)。

> 本仓库是 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) v0.4.3（commit `8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）的独立 fork，保留 `upstream` 远程用于对照和同步。插件 ID 与 provider 为 `open-design`，当前版本 `0.2.0`。这不是 OpenDesign 官方发布，也不代表 ClinePassBridge 上游。

## 功能

`0.2.0` 的代码、验证与本地交付库已准备完成，本轮尚未 push、发布或部署；生产继续运行 `0.1.0`。

- 添加、编辑、启停和删除 OpenDesign 账户，支持替换推理 API key；编辑时留空保留原 Key。
- 一次点击获取官方 `GET /models` 列表，支持搜索、批量勾选、共享模型映射与未保存草稿保护。新增映射默认使用 `open-design-` 前缀。
- 可选配置独立 control Key，手动查询官方账号钱包（USD）与工作区套餐窗口（credits）。新增和编辑账户均可填写两个可选字段：control Key、Workspace ID。实际剩余量与重置时间直接显示；符合采样条件的整体 credits 趋势默认折叠，不换算为单模型剩余 token 或请求数。详见[额度说明](docs/quota.md)。
- 提供管理接口 `/v0/management/open-design` 和控制台 `/v0/resource/plugins/open-design/console`。
- 通过 CPA 公开的 host HTTP / translation / public RPC 发起请求，支持流式与非流式 Chat Completions。

额度和预测使用 OpenDesign 官方协议；Cline 的计费接口、价格、额度周期和能力过滤不适用于本插件。

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
2. 在账户管理中添加 OpenDesign 推理 API key。标签仅作备注，不承诺将客户端请求固定调度到该账户。
3. 点击账户的“获取模型”，搜索、勾选模型并保存映射；已有别名会保留，新增别名默认加 `open-design-` 前缀。
4. 在客户端填入 CPA 的 API 地址和 CPA 客户端 API key，使用已保存的客户端模型名调用 `/v1/chat/completions`。
5. 如需额度，在新增或编辑账户时展开可选额度配置：同一账号的 control Key 用于查钱包，再填 Workspace ID 可查套餐窗口。保存后点该账户的「刷新额度」；两项都留空可照常推理。查询和趋势估算不改变账号启停或调度。

管理页需要 CPA 管理认证。插件数据目录默认为 `plugins/open-design-data`，可通过 `data_dir` 修改；容器部署请持久化插件目录、CPA 的 `auth-dir` 和该数据目录。

管理接口按 CPA 路由契约提供：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /v0/management/open-design/credentials` | 列出账户（不回显 key） |
| `POST /v0/management/open-design/credentials` | 创建或更新账户，body 为 `{id?, label?, api_key?, disabled?, control_key?, workspace_id?, clear_billing?}` |
| `DELETE /v0/management/open-design/credentials?id=<id>` | 按查询参数删除账户 |
| `GET /v0/management/open-design/status` | 插件状态与模型映射 |
| `GET /v0/management/open-design/upstream-models` | 获取 OpenDesign `GET /models` 候选 |
| `GET` / `POST /v0/management/open-design/models` | 读取或保存模型映射 |
| `GET /v0/management/open-design/quota?credential_id=<id>` | 读取额度缓存；加 `&refresh=1` 手动查询官方接口，最小间隔 60 秒 |

## 安全模型

- 默认固定 OpenDesign 官方生产地址 `https://amr-link.open-design.ai/v1`，不接受自定义上游地址。后续 HTTP 重定向由 CPA 宿主处理；本地 CPA 8.0.48 的跨域 307 测试确认其会跟随跳转并去掉 `Authorization`，这不等于插件禁止所有重定向。
- 推理 API key 和可选 control Key 只保存在 CPA 的 `auth-dir`，通过 `host.auth.save` 写入；插件设置中不保存 Key，管理响应不回显。宿主确认保存成功后才发布新凭据；请求发出后两种 Key 的输入框都会清空。
- 映射保存在插件数据目录；上游模型目录、额度缓存与预测样本只在内存中。日志、截图和提交中不要包含 Key。
- 当前 fork 使用公开 ABI，不修改、不打包也不发布 CPA 宿主。

## 模型映射

管理页从 OpenDesign `GET /models` 获取可用模型。模型映射在插件内共享，用于发现模型的账户选择不绑定推理调度。保存映射后，插件同步刷新 CPA 的凭据模型注册；客户端请求 `model` 填客户端名称，插件将其转换为已保存的上游模型 ID，再调用 `POST /chat/completions`。未保存的模型名不会自动透传。

页面会保留尚未保存的映射草稿；可以显式还原。若映射已落盘但宿主刷新失败，界面会提示已保存的阶段与需要重试的阶段，避免把部分完成误报为完全未保存。

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

发布 tag 使用 `open-design-v0.2.0` 形式，避免与 fork 保留的上游 `v*` tag 冲突。CPA 插件商店的 latest-release 解析要求 tag 以数字开头，因此从自有源安装时需显式指定该 tag/版本，`github-release` 的自动“取最新”不适用于本 fork。

## 当前验证边界

- 2026-11-02 的真实 OpenDesign 验证已成功：发现 13 个模型；官方直连与生产 CPA 插件各完成一次最小 JSON 推理并得到 `OK`。生产插件测试使用 `open-design-minimax-m3.1-flash`，usage 为 219 输入／3 输出／222 总 token。13 个模型只验证发现、持久化与注册，没有逐个付费推理。
- 早期现有推理 Key 对官方控制面钱包、账单汇总接口返回 401。随后，两份独立 control 授权直接查询钱包和各自工作区 funding-check，共四个 GET 均返回 200，字段契约与工作区匹配；直接读取成功不代表已上线新版或完成真实消费预测。额度成功、错误、缓存与趋势算法另有隔离测试覆盖。
- [验证记录](docs/verification.md) 分别列出 Go／Node、Edge 交互与布局、原版 CPA 配合 loopback TLS 假上游的结果，避免把隔离测试写成真实额度成功。

## 文档与来源

- [OpenDesign 插件说明](docs/open-design.md)
- [额度接口、状态与预测计算](docs/quota.md)
- [验证记录与真实上游限制](docs/verification.md)
- `docs/` 下的其他旧文档保留为上游参考，本 fork 未逐篇改写。
- 上游：[xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)
- 协议参考：[AstroQore/opendesign2api](https://github.com/AstroQore/opendesign2api)（独立网关 prior art，仅参考公开协议行为，未复制源码）
- OpenDesign 官方生产配置参考：[nexu-io/open-design vela.ts](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/apps/daemon/src/integrations/vela.ts#L819-L842)

## 许可

[MIT License](LICENSE)。原项目版权归 xiao-qiu-qiu；本 fork 的修改部分版权归 ruruamour。
