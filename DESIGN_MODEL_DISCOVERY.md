# cpa-router 面板模型候选获取被过滤问题 —— 技术设计规格

## 问题根因

`intercept.go` 的 `response.intercept_after` 对**所有** `SourceFormat == "openai"` 且 body 可被证明为模型清单的响应执行过滤，白名单仅含本插件启用的组调用名。

这导致两类消费者受损：
1. **cpa-router 面板 (`panel.html`)**：`loadModelNames()` 目前优先调用宿主管理端点 `/model-prices/runtime-models`，若该端点内部也以 OpenAI model-list 格式拼装响应，则会被本插件拦截为空或仅剩组名。
2. **CPAMP 后台**：后台在展示全部模型时可能直接调用 `/v1/models`，同样被过滤。

`ResponseInterceptRequest` 中没有原始请求路径字段，只有 `RequestHeaders`（以及 body、status 等），因此无法通过 URL 区分「普通客户端拉模型列表」与「管理后台拉模型列表」。必须在拦截器内通过 header 特征识别内部请求，并辅以专用管理端点兜底。

## 方案设计（双轨制）

### 轨道 1：拦截器内部请求放行（解决 CPAMP 后台 / 面板 iframe）

在 `filterModelList` 开头增加 **internal request 识别**，命中则直接返回 `nil, false`（不修改 body）。

识别规则（按优先级）：
1. `RequestHeaders.Get("Referer")` 包含 `/management`、`/v0/resource/plugins/` 或 `/v0/management/` 时视为内部请求。
2. `RequestHeaders.Get("User-Agent")` 包含 `cpamp-panel` 或 `cpa-router-panel` 时视为内部请求（预留，目前 CPA 宿主未打此标记，但未来可扩展）。

理由：
- 面板以 iframe 形式嵌入 `/v0/resource/plugins/cpa-router/panel`，浏览器加载页面资源时会携带 Referer，其值必含 `/v0/resource/plugins/cpa-router/panel`。
- CPAMP 管理后台页面地址含 `/management`，其内部 API 调用（xhr/fetch）的 Referer 同样指向管理域名下的 `/management` 路径。
- 仅检查 Referer 即可覆盖当前全部场景，User-Agent 规则作为未来可配置扩展点。

**影响面**：只改 `intercept.go`，不触及 `filter_models` 配置开关的语义。`filter_models: false` 时仍全局放行；`filter_models: true` 时仅对上述内部请求放行。

### 轨道 2：专用管理端点 `/plugins/cpa-router/models`（解决面板成员选择器）

在 `management.go` 新增路由：

```go
{"Method": "GET", "Path": "/plugins/cpa-router/models"}
```

**实现策略**：

插件没有「直接拿到宿主全量模型清单」的宿主回调，但有 `host.auth.list` + `host.auth.get` 可枚举全部凭证并解析其 JSON。通过枚举凭证中的 `models` 字段，可补全所有上游渠道模型名。

处理流程：
1. 调用 `host.Call(pluginabi.MethodHostAuthList, {})` 获取全部 `HostAuthFileEntry`。
2. 对每个 entry，调用 `host.Call(pluginabi.MethodHostAuthGet, {"auth_index": entry.AuthIndex})` 获取凭证原始 JSON。
3. 解析 JSON 中的模型列表（`models` 字段，支持 `string` 或 `{name, alias}` 对象数组）。
4. 同时把本插件当前启用的组调用名（`callNameSnapshot` 结果）加入列表。
5. 去重、排序后返回：
   ```json
   {"models": ["gpt-4o", "claude-3-5-sonnet", "r/free-stack", "r/free", ...]}
   ```

**性能考虑**：该端点仅在面板打开「编辑组」弹窗时懒加载一次（`ensureModelNames()`），调用频率极低，因此串行 `auth.list` + N 次 `auth.get` 可接受。若未来凭证数量巨大，可加并发或缓存，当前版本不需要。

**面板修改**：
- `panel.html` 的 `loadModelNames()` 第一优先改为调用 `/plugins/cpa-router/models`。
- 原 `/model-prices/runtime-models` 与 `/config` fallback 降级为第二、第三备选，保证兼容。

### 轨道 3：面板 fallback 保持 `/config` 解析（兜底）

保留现有 `/config` fallback 逻辑不变，作为第三级兜底。若轨道 2 端点因凭证解析异常失败，面板仍可回到 `/config` 解析路径。

## 接口契约

### 新增管理路由

| Method | Path | 说明 |
|--------|------|------|
| GET | `/plugins/cpa-router/models` | 返回宿主全部可用模型名 + 本插件组调用名 |

请求：无 body，标准 management 鉴权（由宿主 middleware 处理）。

成功响应 200：
```json
{
  "models": ["gpt-4o", "claude-3-5-sonnet", "r/free-stack", "r/free"]
}
```

失败响应 500（宿主回调异常时）：
```json
{"ok": false, "error": "..."}
```

### 拦截器内部放行规则（新增）

`rpcResponseInterceptRequest.RequestHeaders` 满足以下任一条件时，`filterModelList` 直接返回 `nil, false`：
- `Referer` 非空且包含子串 `/management`
- `Referer` 非空且包含子串 `/v0/resource/plugins/`
- `Referer` 非空且包含子串 `/v0/management/`

不新增配置字段，避免过度工程化。若未来需要更灵活匹配，可再引入 `filter_models_bypass_referers` 配置。

## 文件变更清单

| 文件 | 变更内容 |
|------|----------|
| `intercept.go` | `filterModelList` 开头增加 `isInternalRequest(reqHeaders)` 检查；新增 `isInternalRequest` 函数 |
| `management.go` | `managementRegistrationJSON` 增加新路由声明；新增 `handleModelsList` 函数；`handleManagement` switch 增加匹配分支 |
| `panel.html` | `loadModelNames()` 第一优先改为 `api("/plugins/cpa-router/models","GET")` |
| `intercept_test.go` | 新增测试：`TestInterceptSkipsInternalRequests`（Referer 含 management/resource 时放行） |
| `management_test.go` | 新增测试：`TestHandleModelsList`、`TestModelsListIncludesGroupCallNames` |
| `assets_test.go` | 更新面板测试断言：验证 `loadModelNames` 优先调用 `/plugins/cpa-router/models` |
| `fakes_test.go` | fakeHost 支持 `host.auth.list` / `host.auth.get` handler 脚本 |

## 验收标准

1. `go test -race ./...` 全绿。
2. `make build` 产出 `bin/cpa-router.so`。
3. 单测覆盖：
   - 内部请求（Referer 含 management/resource/management）不被过滤，body 原样透传。
   - 普通客户端 `/v1/models`（无特殊 Referer）仍被正常过滤。
   - `/plugins/cpa-router/models` 返回的 JSON 含凭证中的模型名及本插件组调用名。
   - 凭证 `models` 字段为字符串数组或对象数组时均正确解析。
4. 面板 `loadModelNames()` 调用顺序：先 `/plugins/cpa-router/models`，fallback `/model-prices/runtime-models`，再 fallback `/config`。
