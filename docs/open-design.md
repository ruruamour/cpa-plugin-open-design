# OpenDesign 插件说明

本仓库是 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) v0.4.3（commit `8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）的独立 fork。插件 ID 与 provider 为 `open-design`，当前版本 `0.2.0`。本 fork 不是 OpenDesign 官方发布，也不是 ClinePassBridge 的官方下游。

## 范围

- 实际 Provider 位于 `internal/opendesign`。
- `internal/bridge` 是保留的上游参考实现，不接入本插件的新二进制。
- 使用 CPA 公开的 host HTTP、translation 和 public RPC；不修改、不打包、不发布 CPA 宿主。
- 额度查询按 OpenDesign 官方 control API 实现，可选配置独立 control Key；不使用 Cline 的额度、价格或周期规则。详见[额度说明](quota.md)。
- 管理页的账号选择用于模型发现，映射为插件共享；实际请求的账号选择与冷却遵循 CPA 公开接口。

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

- 推理 API key 与可选 control Key 保存在 CPA 的 `auth-dir`，通过 `host.auth.save` 写入；插件设置不保存 Key，管理接口不回显。
- 管理接口按 CPA 精确路由契约提供：`GET /credentials` 列出账户；`POST /credentials` 创建或更新，body 为 `{id?, label?, api_key?, disabled?, control_key?, workspace_id?, clear_billing?}`；`DELETE /credentials?id=<id>` 按查询参数删除，不使用 `/credentials/{id}`。编辑时 Key 留空保留原值，`clear_billing:true` 显式清除额度授权和工作区。
- 模型列表由管理页调用 OpenDesign `GET /models` 获取，支持搜索与可见项批量勾选。新增客户端别名默认以 `open-design-` 开头，已保存和手动编辑的别名保留。账户刷新不覆盖未保存的映射草稿。
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

- 2026-11-02 已用有效推理 Key 发现 13 个真实上游模型。官方直连和生产 CPA 插件各完成一次最小非流式请求，均返回 `OK`；生产路径使用 `open-design-minimax-m3.1-flash`，usage 219／3／222。未逐个调用全部模型。
- 新增额度使用官方 control API。现有推理 Key 查询控制面返回 401 后，另用两份独立 control 授权直接查询钱包和对应工作区 funding-check，四次 GET 均为 200。2026-11-02 上线 `0.2.0` 后，od1 授权也已通过生产插件查询成功；账户归属以完整推理 Key 在服务器内精确匹配确认。消费趋势预测尚未实测。
- 本地测试使用原版 CPA 8.0.48 与 loopback CONNECT/TLS 假上游，不修改系统信任库或 hosts；只允许推理、控制面与重定向测试主机。
- 具体 Go／Node、Edge、真实宿主回归结果见[验证记录](verification.md)；[验证脚本说明](../scripts/README.md)区分 ctypes、原版宿主与假上游的边界。

## 发布 tag

- 发布 tag 使用 `open-design-v0.2.0`，不创建或覆盖上游 `v*` tag。
- 工作流触发 `open-design-v*`，去掉前缀后与 `internal/opendesign` 的 `Version` 和 registry 版本比对。
- CPA 插件商店解析 GitHub latest release 时要求 tag 以数字开头，因此从自有源安装时需显式指定该 tag/版本。

## 上游与许可

- 上游仓库：[xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)
- 许可证：[MIT](../LICENSE)，保留原版权声明并附带本 fork 的派生版权声明。
- `docs/` 下的其他文档保留为上游参考，本 fork 未逐篇改写。
