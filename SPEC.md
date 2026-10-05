# cpa-router —— CPA(CLIProxyAPI) 模型路由插件 需求文档 v1

> 本文件是实现需求，**逐节编号**。请严格按节实现；每节末尾的「验收」都要自己跑一遍并贴出真实输出。
> 不要编造命令输出。跑不通就如实说，并说明卡在哪一步。

---

## 0. 背景与目标

给 CLIProxyAPI(下称 CPA)写一个 **C ABI 动态库插件**(`.so`)，提供类似 9Router "combo" 的**命名模型组**能力：

1. **命名组**：一组模型可以起一个自己的名字，客户端直接用这个名字当 `model` 调用。
2. **两种策略**：
   - `round-robin`（轮询组）：在组成员之间轮流分发；
   - `fallback`（降级组）：按声明顺序依次尝试，成功即止。
3. **可嵌套**：组的成员可以是另一个组（任意层，需检测环）。
4. **429 移除 + 指数退避**：某个成员返回 429 时把它从可用池里**摘掉**（冷却），冷却时长按指数退避增长；冷却中的成员不会被选中（除非整组都在冷却，见 7.4）。
5. **UI**：CPA 管理面板里出现一个「模型路由」菜单，可视化增删改查组、看冷却/统计、发测试请求。

**必须复用宿主既有执行链路**：不要自己保存密钥、不要自己发上游 HTTP 请求。做法是插件在 `model.route` 里把匹配到的组请求接管到自己的 executor，executor 通过宿主回调 `host.model.execute` / `host.model.execute_stream` 用**成员的真实模型名**重新进入宿主正常链路（这样渠道解析、别名映射、密钥选择、用量统计、日志全部仍由宿主负责）。

---

## 1. 运行环境（已核实，按此实现）

- 宿主：`CLIProxyAPI v8.0.13`（容器 `cpamp-cli-proxy-api-1`，Debian，glibc，root，工作目录 `/CLIProxyAPI`）。
- 插件目录：`/CLIProxyAPI/plugins`（**可写**），宿主按 `plugins/<GOOS>/<GOARCH>` → `plugins` 顺序查找；插件 ID = `.so` 文件名去掉扩展名。
- SDK 模块：`github.com/router-for-me/CLIProxyAPI/v8 v8.0.13`（公开模块，`go.mod` 需要 `go 1.26.0`）。
  - 本机 Go 会自动下载 1.26 工具链（已实测可用）。
  - 宿主与插件**必须同为 linux/amd64**，构建用 `CGO_ENABLED=1 go build -buildmode=c-shared`。
- ABI：`sdk/pluginabi` 的 `ABIVersion = 1`、`SchemaVersion = 6`。
- 插件 ID：`cpa-router`（必须匹配 `[A-Za-z0-9][A-Za-z0-9._-]{0,127}`）。

### 验收
- `go env GOMODCACHE` 下的 `.../CLIProxyAPI/v8@v8.0.13/` 存在，且能 `go build`。
- 编译产物 `bin/cpa-router.so` 前 4 字节是 ELF（`head -c 4 bin/cpa-router.so | od -An -c` 显示 `177 E L F`）。
- `nm -D bin/cpa-router.so | grep cliproxy` 能看到 `cliproxy_plugin_init`、`cliproxyPluginCall`、`cliproxyPluginFree`、`cliproxyPluginShutdown`。

---

## 2. 代码结构与硬性约束

```
cpa-router/
├── go.mod                module github.com/swysgh/cpa-router
├── Makefile              build / test / vet / fmt / clean
├── README.md             安装、配置、组语法、UI、限制
├── abi.go                C ABI 粘合层（唯一出现 import "C" 的文件之一）
├── bridge.go             宿主回调桥（import "C"）
├── host.go               hostCaller 接口 + 错误类型（纯 Go）
├── plugin.go             能力注册、方法分发、生命周期
├── config.go             插件配置解析与校验
├── group.go              组/成员模型、调用名索引、校验、环检测
├── store.go              状态文件读写 + mtime 热加载
├── runtime.go            运行时状态：冷却/退避/统计/轮询指针
├── route.go              model.route 决策
├── executor.go           executor.execute（非流式）
├── stream.go             executor.execute_stream（流式）
├── management.go         管理路由与资源页注册、请求处理
├── panel.html            资源页（单文件，go:embed 内嵌）
├── assets.go             go:embed
└── *_test.go             单元测试
```

**硬性约束（违反即视为不合格）**

1. **业务逻辑必须是纯 Go 且可单测**：除了 `abi.go` / `bridge.go`，其他文件**不得** `import "C"`。
   所有对宿主的调用都必须经过 `hostCaller` 接口（见 §9），测试里用假实现替换。
2. 不新增任何第三方依赖，除了 `gopkg.in/yaml.v3`。
3. 日志里**不得**出现密钥、token、凭证 JSON、用户请求体全文（可以记长度、模型名、状态码、耗时）。
4. 不改动 CPA 仓库、不改 `config.yaml` 之外的宿主文件；本插件只写自己的状态文件。
5. 不要实现需求之外的功能（不做权重、不做配额查询、不做 token 计数估算）。

---

## 3. 插件注册（`plugin.register` / `plugin.reconfigure`）

请求体字段：`{"config_yaml": "<base64>"}`（`plugin.register`、`plugin.reconfigure` 都是这个形状）。

处理流程：

1. 解析 `config_yaml`（YAML），按 §4 校验；校验失败 → **返回错误信封**，错误信息用中文写明是哪个字段为什么不合法。
2. 记录生效配置（原样打印一遍到宿主日志，见 §11）。
3. 加载状态文件（§5）；失败只记警告日志，不阻断注册（组为空）。
4. 返回注册结果。

注册结果 JSON（字段名必须完全一致）：

```json
{
  "schema_version": 6,
  "metadata": {
    "Name": "cpa-router",
    "Version": "0.1.0",
    "Author": "swysgh",
    "GitHubRepository": "https://github.com/swysgh/cpa-router",
    "Logo": "",
    "ConfigFields": [
      {"Name": "state_file", "Type": "string", "Description": "组配置状态文件路径（相对进程工作目录）"},
      {"Name": "name_prefix", "Type": "string", "Description": "组调用名前缀，默认空"},
      {"Name": "reload_interval", "Type": "string", "Description": "状态文件热加载检查间隔，如 2s"},
      {"Name": "max_attempts", "Type": "number", "Description": "单请求最多尝试的成员数，0 表示不限制"},
      {"Name": "attempt_timeout", "Type": "string", "Description": "单次上游尝试的超时"},
      {"Name": "total_timeout", "Type": "string", "Description": "单请求总时间预算"},
      {"Name": "all_cooling_policy", "Type": "enum", "EnumValues": ["wait", "first", "error"], "Description": "整组冷却时的策略"},
      {"Name": "max_wait", "Type": "string", "Description": "all_cooling_policy=wait 时的最长等待"},
      {"Name": "log_level", "Type": "string", "Description": "debug|info|warn|error"}
    ]
  },
  "capabilities": {
    "model_router": true,
    "model_registrar": true,
    "executor": true,
    "executor_model_scope": "static",
    "executor_input_formats": ["openai", "openai-response", "claude", "gemini", "codex", "antigravity", "interactions"],
    "executor_output_formats": ["openai", "openai-response", "claude", "gemini", "codex", "antigravity", "interactions"],
    "management_api": true
  }
}
```

> `ConfigField.Type` 只允许 `string`/`number`/`boolean`/`enum`/`array`（枚举值按 SDK 常量，实际用字符串字面量即可）。

`plugin.reconfigure` 除注册结果外还要：重新加载状态文件、按新配置更新运行时参数（保留已有冷却/统计）。

### 验收
- 用一个最小配置跑 `configure()`（单测），断言返回的注册 JSON 能被 `json.Unmarshal` 且 `capabilities.model_router == true`、`schema_version == 6`。
- 非法配置（如 `reload_interval: "abc"`）断言返回错误信封且消息含中文原因。

---

## 4. 插件配置（`plugins.configs.cpa-router`）

宿主只解析 `enabled` / `priority`，其余原样透传。默认值如下（**用户没写时用默认值**）：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-router:
      enabled: true          # 宿主字段
      priority: 20           # 宿主字段：必须高于 qoder(5)，保证路由插件先被询问
      state_file: "plugins/cpa-router/groups.yaml"   # 相对进程工作目录(/CLIProxyAPI)
      name_prefix: ""        # 组调用名前缀，例如 "r/" 时调用名是 "r/<组名>"
      reload_interval: "2s"  # 状态文件 mtime 检查间隔；<=0 表示不热加载
      max_attempts: 6        # 单请求最多尝试的成员数（展开后的模型个数），0=不限
      attempt_timeout: "120s" # 单次 host.model.* 调用的超时
      total_timeout: "600s"   # 单请求总预算，超了返回 504
      all_cooling_policy: "wait" # wait | first | error
      max_wait: "15s"        # wait 策略的最长等待
      log_level: "info"      # debug | info | warn | error
      cooldown:
        disable: false
        statuses: [429]              # 触发指数退避的状态码
        base: "30s"
        factor: 2.0
        max: "30m"
        jitter: 0.2                  # ±20% 随机抖动，0 表示不抖动
        transient_statuses: [500, 502, 503, 504]
        transient_base: "15s"
        transient_factor: 2.0
        transient_max: "5m"
```

校验规则（任一不满足 → 返回中文错误信封，格式 `配置错误: <字段>: <原因>`）：

- 所有时长字段必须是合法 Go duration 字符串且 `> 0`（`reload_interval` 允许 `0` 表示关闭热加载）。
- `max_attempts >= 0`；`factor >= 1`；`0 <= jitter < 1`。
- `all_cooling_policy` 只能是 `wait|first|error`。
- `log_level` 只能是 `debug|info|warn|error`。
- `cooldown.max >= cooldown.base`，`cooldown.transient_max >= cooldown.transient_base`。
- `statuses` / `transient_statuses` 里每个值必须是 100..599 的整数。
- `name_prefix` 只能包含 `[A-Za-z0-9._:/-]`。
- `state_file` 为空 → 允许（纯内存模式，UI 改动不落盘，并在 UI 上提示）。

### 验收
- 单测：默认值；每个非法字段各一条用例，断言错误信息包含字段名。
- 单测：`name_prefix: "r/"` 时调用名解析正确。

---

## 5. 状态文件（组配置）

默认路径 `plugins/cpa-router/groups.yaml`（相对进程工作目录；注册时把**解析后的绝对路径**打进日志）。格式：

```yaml
version: 1
groups:
  - name: "free-stack"          # 组名（调用名 = name_prefix + name）
    strategy: "round-robin"     # round-robin | fallback
    enabled: true
    description: "免费额度优先"   # 可选
    aliases: ["free"]           # 可选，额外调用名（同样会加 name_prefix）
    members:
      - model: "wb2api-deepseek-v4.1-flash"
        enabled: true
      - group: "cheap"          # 嵌套另一个组
        enabled: true
```

校验（失败 → 加载时记中文警告日志并保留上一份可用配置；管理 API 保存时返回 400 + 中文原因）：

1. `version` 必须为 `1`。
2. `name` 非空、唯一，匹配 `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`。
3. `strategy` 只能是 `round-robin` 或 `fallback`（缺省 `fallback`）。
4. `aliases` 同样要过 2 的字符校验，且**不得与任何组名或其它组的别名冲突**。
5. 每个成员必须**恰好**有 `model` 或 `group` 之一（不能都有、不能都没有），值非空且过字符校验。
6. `group` 引用的组必须存在。
7. **环检测**：DFS 检测 `group` 引用形成的环，发现环时报错并打印环路径，例如
   `组配置错误: 检测到循环嵌套: a -> b -> c -> a`。
8. 运行时还要有深度上限 `max_nesting_depth`（**硬编码常量 8**，不放进配置），展开超过深度 → 该组本次请求返回错误。

写盘要求：管理 API 保存时用「临时文件 + `os.Rename`」原子替换，并保留 0600 权限（目录 0755）。

### 验收
- 单测：正常解析；缺 `version`；重名；别名冲突；成员同时给 model/group；引用不存在的组；自引用环 `a -> a`；三节点环 `a -> b -> c -> a`（断言错误信息里含环路径）。
- 单测：写盘 → 读回 → 内容等价（`reflect.DeepEqual`）。

---

## 6. `model.route` 决策

请求体对应 `pluginapi.ModelRouteRequest`，只用到：`SourceFormat`、`RequestedModel`、`Stream`、`Headers`、`Query`、`Body`、`AvailableProviders`。

逻辑：

1. 取 `RequestedModel`，按 §7.1 解析成组（含 `name_prefix`、别名）。
2. 解析不到 / 组被禁用 / `model_route_enabled == false` → 返回 `{"Handled": false}`。
3. 否则返回：

```json
{"Handled": true, "TargetKind": "self", "Reason": "group:free-stack:round-robin"}
```

- **不要**设置 `TargetModel`（`self` 会被忽略）。
- 决策必须**快**：只读内存（状态文件 mtime 检查按 `reload_interval` 节流），不做任何网络/磁盘重活。
- 解析失败时不得 panic（宿主会因 panic 熔断插件）。

### 验收
- 单测：命中组 / 未命中 / 组被禁用 / 带前缀 / 别名 五种输入的响应体断言。
- 单测：状态文件 mtime 变化后，`reload_interval` 内不重载、超过后重载（注入假时钟）。

---

## 7. 组展开与选择算法（核心）

### 7.1 调用名解析

- 调用名 = `name_prefix + name`，以及 `name_prefix + alias`。
- 建索引 `map[调用名]组名`；命中后取组。
- 未加前缀的裸组名**不**命中（`name_prefix` 非空时）。

### 7.2 展开（把组展开成有序的模型名列表）

```
expand(groupName, depth, visited):
  if depth > 8            -> 报错 "嵌套层数超过 8"
  if groupName in visited -> 报错 "循环嵌套"   (双保险)
  g = groups[groupName]
  if g == nil or !g.enabled -> 返回空
  eligible = [m for m in g.members if m.enabled and (m 是 model 时该模型不在冷却中)]
  if strategy == "round-robin":
      start = pointer[groupName] % len(eligible)      # eligible 非空时
      eligible = eligible[start:] + eligible[:start]
      pointer[groupName] = start + 1
  # fallback: 保持声明顺序，pointer 不变
  out = []
  for m in eligible:
      if m 是 model: out.append(m.model)
      else:          out.extend(expand(m.group, depth+1, visited + {groupName}))
  return out
```

要点：
- **轮询指针按「组」维护，每次展开该组就前进 1**（`start+1`），所以每个请求都会换一个首选成员；同一个请求内部失败降级时仍按本次展开的顺序走。
- **冷却中的成员在展开阶段就被摘掉**（这就是需求里的「移除 429 的请求」）。
- 去重：同一个模型名在一次展开结果里只保留**第一次**出现（避免同一请求重复打同一个上游）。
- `max_attempts > 0` 时截断到该长度。

### 7.3 冷却（指数退避）

状态按**模型名**全局维护（同一模型被多个组引用时共享冷却）：

```
cooldown[model] = {until, level, lastStatus, lastError, lastAt}
```

- 命中 `cooldown.statuses`（默认 429）时：
  `level += 1`；`delay = min(base * factor^(level-1), max)`；`delay *= 1 + rand(-jitter, +jitter)`；
  `until = now + delay`。
  例（默认参数）：30s → 60s → 120s → 240s → … → 封顶 30m。
- 命中 `transient_statuses`（5xx）或**无状态码的调用错误/超时**时：用 `transient_*` 参数做同样的退避。
- 其它 4xx（400/401/403/404/405 等）：**只记统计，不冷却**，继续尝试下一个成员。
- 成功（2xx）时：`level = 0`、清除冷却、`until = 0`。
- 冷却是否生效看 `now < until`；`cooldown.disable == true` 时永不冷却（仍会尝试下一个成员）。
- 冷却时长取整到秒写进日志与 UI。

### 7.4 整组冷却时的策略（`all_cooling_policy`）

当一次展开得到的候选列表**为空**（全部在冷却，或组为空）：

- `wait`：等到**最早**一个冷却到期（不超过 `max_wait`），再重新展开一次；仍为空 → 返回 429 错误（中文消息，见 §8.3）。只等一轮，不循环。
- `first`：忽略冷却，直接用「冷却到期时间最早」的那个成员（会触发一次真实请求）。
- `error`：立即返回 429 错误。

### 验收（单测，全部用假时钟/假宿主）
- fallback 顺序 = 声明顺序；round-robin 连发 3 次请求，首选成员依次为 m1、m2、m3、m1。
- 冷却中的成员被摘掉：让 m1 429 后，下一次展开列表里没有 m1。
- 嵌套：`outer(fallback) = [a, inner(rr), b]`，`inner = [c, d]`；断言展开顺序与指针前进。
- 深度/环：构造 9 层嵌套 → 报错；`a -> b -> a` → 报错。
- 退避序列：连续 429 的 `until-now` 依次约为 30s/60s/120s（抖动设 0 时严格相等）；超过 max 后封顶；成功后 level 归零。
- 三种 `all_cooling_policy` 行为各一条用例（wait 用假时钟，断言等待时间 = min(最早到期, max_wait)）。
- 去重与 `max_attempts` 截断。

---

## 8. executor：真正执行

### 8.1 `executor.identifier`

返回 `{"identifier": "cpa-router"}`（必须非空，否则宿主认为执行器不可用）。

### 8.2 通用准备

从 `pluginapi.ExecutorRequest` 取：`Model`（= 调用名）、`SourceFormat`、`Stream`、`Headers`、`Query`、`Alt`、`OriginalRequest`、`Payload`、`Metadata`、`HostCallbackID`（RPC 扩展字段）。

- **协议透传**：`entry_protocol = exit_protocol = SourceFormat`（为空时用 `openai`）。这样宿主按客户端原本的协议解析请求体、并按客户端协议返回响应。
- **请求体**：优先用 `OriginalRequest`，为空时用 `Payload`。
- **模型名改写**：如果请求体是 JSON 且顶层有字符串字段 `model`，把它改成**本次选中的成员模型名**再转发；不是 JSON / 没有该字段（如 Gemini 把模型放在 URL 里）→ 原样透传。
- **转发 `host_callback_id`**：调用 `host.model.*` 时必须把宿主给的 `host_callback_id` 原样带上（宿主据此跳过本插件自己的拦截器，避免递归）。
- 每次尝试都独立计时，超时用 `attempt_timeout`；整请求超过 `total_timeout` → 返回 504。

### 8.3 非流式（`executor.execute`）

```
1. 解析调用名 → 组；失败 → 错误信封 code=group_not_found, message 中文, http_status=400
2. attempts = expand(...)   (含 §7.4 整组冷却处理)
   attempts 为空 → 错误信封 code=group_exhausted, http_status=429,
      message 例: "组 free-stack 的成员全部处于冷却中，最早 12s 后恢复"
3. for each member in attempts:
     resp, status, err = hostModelExecute(member, body)
     if err != nil or status >= 400:
        记录统计 + 冷却判定(§7.3)
        lastErr = ...
        continue
     记录成功；返回 ExecutorResponse{Payload: resp.Body, Headers: resp.Headers}
4. 全部失败 → 错误信封 code=group_exhausted,
     http_status = 最后一个失败的状态码（没有则 502）,
     message 中文，包含每个成员的「模型名 + 状态码/错误」，例:
     "组 free-stack 全部成员失败: a: 429(冷却 30s); b: 503(冷却 15s)"
```

返回错误信封时必须带上 `http_status`（`pluginabi.NewErrorEnvelope(code, msg, status)` 的第三个参数），宿主会把它作为客户端看到的 HTTP 状态码。

### 8.4 流式（`executor.execute_stream`）

请求体是 `{"ExecutorRequest 全部字段..., "stream_id": "...", "host_callback_id": "..."}`。

**要求：先同步预检，再异步转发**（这样整组失败时客户端能拿到真正的 HTTP 状态码，而不是一个 200 之后断掉的 SSE）：

```
1. 取 stream_id；为空 → 错误信封 "stream_id is required for executor.execute_stream"
2. 解析组 → attempts（同 8.3）
3. 同步预检：依次对成员调用 host.model.execute_stream
     - 返回错误 或 StatusCode >= 400 → 关闭该 host 流，冷却/统计，继续下一个成员
     - StatusCode 2xx 且 stream_id 非空 → 命中，跳出
   全部失败 → 返回错误信封（http_status 同 8.3），**不碰插件流**
4. 命中后：
   - 立刻返回 {"headers": {"Content-Type": ["text/event-stream"]}}（无 chunks）
   - 起一个 goroutine 转发：
       loop:
         chunk = host.model.stream_read(命中流的 stream_id)
         if chunk.Error != "" :
             if 还没转发过任何 payload  -> 视为本次失败：关闭该流，冷却/统计，回到预检循环尝试下一个成员
             else                       -> host.stream.emit(插件流, error) 后 host.stream.close(插件流, error)，结束
         if len(chunk.Payload) > 0   -> host.stream.emit(插件流, payload)，标记「已转发」
         if chunk.Done               -> 结束
       结束（正常或错误）都要：
         host.model.stream_close(命中流)
         host.stream.close(插件流, "") 或 host.stream.close(插件流, errMsg)
   - goroutine 里必须 recover panic，panic 时 host.stream.close(插件流, "stream orchestration panic: ...")
```

- 「已转发第一个 payload 之后不再降级」是硬要求（客户端已经看到内容了）。
- 关闭流一律显式调用（不要依赖宿主隐式清理）。
- 插件流 id 与宿主模型流 id 是**两个不同的 id**，不要混用。

### 验收
- 单测（假宿主）：
  - 非流式：m1 返回 429、m2 返回 200 → 返回 m2 的 body，且 m1 进入冷却、统计正确。
  - 非流式：全部 429 → 错误信封，`http_status == 429`，消息里含每个成员与状态码。
  - 非流式：JSON body 的 `model` 被改写成选中成员；非 JSON body 原样。
  - 流式：预检 m1 429 → m2 200 → 返回 `headers` 且异步 emit 了 m2 的 chunks，最后 `host.stream.close` 无 error。
  - 流式：预检全失败 → 错误信封带状态码，且**没有**调用 `host.stream.emit`。
  - 流式：m1 200 但第一个读返回 error（未转发任何 payload）→ 自动降级到 m2。
  - 流式：已转发 payload 后出错 → emit error + close error，不再降级。

---

## 9. 宿主回调桥（`hostCaller`）

```go
// host.go —— 纯 Go
type hostCaller interface {
    // Call 执行一次宿主回调；err 为 nil 表示宿主返回 ok 信封。
    Call(method string, payload any) (json.RawMessage, error)
}

type hostError struct { Code, Message string; HTTPStatus int }
func (e *hostError) Error() string
func (e *hostError) StatusCode() int   // 返回 HTTPStatus，供 HTTPStatusFromError 风格判断
```

`bridge.go` 里的真实实现：

1. `json.Marshal(payload)` → `C.CBytes` → `call_host_api(method, req, len, &resp)`。
2. 取回 buffer → 解析 `{"ok":bool,"result":...,"error":{code,message,http_status}}`。
3. `!ok` → 返回 `*hostError`（**必须解析 `http_status` 字段**，这是判断 429 的正规途径；不要只做字符串匹配）。
4. 释放宿主 buffer（`free_host_buffer`）。

需要用到的方法名常量（直接用 `pluginabi.Method*`）：
`MethodHostModelExecute`、`MethodHostModelExecuteStream`、`MethodHostModelStreamRead`、`MethodHostModelStreamClose`、`MethodHostStreamEmit`、`MethodHostStreamClose`、`MethodHostLog`。

`host.log` 的请求体：`{"level":"info|warn|error|debug","message":"...","fields":{...},"host_callback_id":"..."}`。
日志必须先按 `log_level` 过滤，再发给宿主。

### 验收
- 单测：`hostError` 实现 `StatusCode()`，且从信封 JSON（含 `http_status: 429`）解析正确。
- 单测：ok 信封解析出 result；错误信封返回 hostError 且 message 正确。

---

## 10. 管理 API 与资源页

### 10.1 注册

`management.register` 返回：

```json
{
  "resources": [
    {"Path": "/panel", "Menu": "模型路由", "Description": "命名模型组：轮询/降级/嵌套，含 429 冷却与测试"}
  ],
  "routes": [
    {"Method": "GET",    "Path": "/plugins/cpa-router/state"},
    {"Method": "PUT",    "Path": "/plugins/cpa-router/groups"},
    {"Method": "POST",   "Path": "/plugins/cpa-router/groups"},
    {"Method": "DELETE", "Path": "/plugins/cpa-router/groups"},
    {"Method": "POST",   "Path": "/plugins/cpa-router/reset"},
    {"Method": "POST",   "Path": "/plugins/cpa-router/test"}
  ]
}
```

- 资源页最终地址：`/v0/resource/plugins/cpa-router/panel`（**无需鉴权**，注意：**资源页的 GET 不能有副作用**）。
- `/v0/management/...` 下的插件路由由宿主做管理密钥鉴权。
- 路径不能含空白、`:`、`*`、`..`。

### 10.2 各路由语义

| 路由 | 请求 | 响应 |
|---|---|---|
| `GET /plugins/cpa-router/state` | — | `{version, plugin:{version,name}, config:{生效配置摘要}, state_file:{path, exists, mtime}, groups:[...], models:{<模型名>:{cooldown_until, cooldown_level, last_status, last_error, total, ok, fail, status_429, status_5xx, last_used_at, avg_latency_ms}}, now}` |
| `PUT /plugins/cpa-router/groups` | `{"groups":[...]}` 全量替换 | 成功 `{"ok":true,"groups":N}`；校验失败 400 `{"ok":false,"error":"中文原因"}` |
| `POST /plugins/cpa-router/groups` | `{"group":{...}}` 新增/按 name 覆盖 | 同上 |
| `DELETE /plugins/cpa-router/groups?name=x` | — | 成功 `{"ok":true}`；不存在 404 |
| `POST /plugins/cpa-router/reset` | `{"name":"<组名或模型名，可空>","what":"cooldown\|stats\|all"}` | `{"ok":true,"cleared":N}` |
| `POST /plugins/cpa-router/test` | `{"group":"<调用名>","prompt":"...","model":"<可选，覆盖>","stream":false}` | `{"ok":bool,"status":int,"latency_ms":int,"attempts":[{"member":"...","status":int,"error":"...","cooldown_s":int}],"body_preview":"<最多 2000 字符>","error":"中文原因"}` |

- 所有响应 `Content-Type: application/json; charset=utf-8`。
- 管理 API 的写操作成功后：落盘状态文件 + 立刻生效（内存里的组定义同步替换）+ 返回最新 state。
- `POST /plugins/cpa-router/test` 走**真实的** §8.3 执行路径（这是验收工具，不要造假数据）；`prompt` 为空时用 `"只回复两个字:收到"`。

### 10.3 资源页（`panel.html`）

单文件、无外部依赖（不加载任何第三方脚本/字体/CDN）。内嵌 `go:embed`。

**管理密钥获取（零配置，按顺序）**——照抄下面这段（与官方插件面板同一套，已验证可用）：

```js
const ENC_PREFIX = "enc::v1::";
const SECRET_SALT = "cli-proxy-api-webui::secure-storage";
const PANEL_STORE = "cli-proxy-auth";
const SS_KEY = "cpa-router-mgmt-key";
function _enc(t){return new TextEncoder().encode(t)}
function _dec(b){return new TextDecoder().decode(b)}
function _keyBytes(){try{return _enc(SECRET_SALT+"|"+window.location.host+"|"+navigator.userAgent)}catch(e){return _enc(SECRET_SALT)}}
function _xor(d,k){const r=new Uint8Array(d.length);for(let i=0;i<d.length;i++)r[i]=d[i]^k[i%k.length];return r}
function _b64d(s){const bin=atob(s);const b=new Uint8Array(bin.length);for(let i=0;i<bin.length;i++)b[i]=bin.charCodeAt(i);return b}
function deobfuscate(p){if(!p||!p.startsWith(ENC_PREFIX))return p;try{return _dec(_xor(_b64d(p.slice(ENC_PREFIX.length)),_keyBytes()))}catch(e){return p}}
function isEmbedded(){try{return window.self!==window.top}catch(e){return false}}
function readPanelKey(){
  if(!isEmbedded())return null;
  let raw;try{raw=localStorage.getItem(PANEL_STORE)}catch(e){return null}
  if(!raw)return null;
  try{
    const parsed=JSON.parse(deobfuscate(raw));
    const st=(parsed&&parsed.state)||parsed||{};
    return typeof st.managementKey==="string"&&st.managementKey?st.managementKey:null;
  }catch(e){return null}
}
function readUrlKey(){const m=new URLSearchParams(window.location.search).get("key");if(m){history.replaceState(null,"",window.location.pathname)}return m}
function getKey(){return sessionStorage.getItem(SS_KEY)||readPanelKey()||readUrlKey()}
```

- 取不到 key 时显示一个输入框（只存 `sessionStorage`），并提示「在 CPA 管理面板中打开本页可自动获取密钥」。
- 页面必须同时支持「被 CPA 主面板 iframe 内嵌」和「直接打开」两种情形（直接打开时 `isEmbedded()` 为 false，只能手填 key）。
- 主题：跟随宿主 CSS 变量，使用 `var(--foreground)`、`var(--muted-foreground)`、`var(--border)`、`var(--card)`、`var(--accent)` 等，**不要**自己设字体/背景。

**页面功能**（够用即可，不要堆砌）：

1. 顶部：插件版本、状态文件路径、生效配置摘要（折叠）、密钥状态、「刷新」按钮。
2. 组卡片列表：组名 + 调用名、策略徽标（轮询/降级）、启用开关、成员列表（每个成员显示：类型图标、名称、是否冷却 + 剩余秒数、该模型的统计小字）、组级统计（请求/成功/失败）、按钮：`编辑`、`删除`、`重置冷却`、`测试`。
3. 新建/编辑弹窗：`name`、`aliases`（逗号分隔）、`strategy`（下拉）、`description`、`enabled`、成员行（类型下拉 `模型|组` + 名称输入（用 `<datalist>` 提示：已有模型名 + 已有组名）+ 启用勾选 + 删除行）、`添加成员` 按钮。保存 → `POST /plugins/cpa-router/groups`。
4. 测试面板：选组 + prompt 输入 + 「发送」→ `POST /plugins/cpa-router/test`，展示状态码、耗时、每次尝试的成员/状态/冷却、响应前 500 字符。
5. 保存成功后：调用一次 `PATCH /v0/management/plugins/cpa-router/config`，body `{"revision": "<Date.now()>"}`，让宿主重新注册模型列表（失败不报错，只在提示条里说明「模型列表需重启或手动刷新才会更新」）。
6. 所有写操作后重新拉 `GET /plugins/cpa-router/state` 刷新界面；错误用顶部提示条展示（中文）。

### 验收
- 单测：`management.handle` 对 6 条路由的分发（含 404 未知路径、PUT 非法 body → 400）。
- 人工：部署后浏览器打开 `/v0/resource/plugins/cpa-router/panel`，截图/文本确认页面渲染出组列表（无 `undefined`/`NaN`）。

---

## 11. 日志

- 注册时用 `host.log` 打印**生效配置全文**（不含密钥；本插件没有密钥），例如：
  `cpa-router 已加载: 组 3 个, state_file=/CLIProxyAPI/plugins/cpa-router/groups.yaml, max_attempts=6, cooldown.base=30s, ...`
- 每次路由决策（debug 级）：`route hit group=free-stack strategy=round-robin`。
- 每次尝试（info 级）：`attempt group=free-stack member=wb2api-x status=429 cooldown=30s`。
- 成功（info 级）：`ok group=free-stack member=wb2api-x latency_ms=812`。
- 整组失败（warn 级）：中文摘要。
- 状态文件热加载（info 级）：`组配置已热加载: 3 个组`；失败 warn + 中文原因。
- 日志里不得出现完整请求体/响应体（只允许长度与前缀）。

---

## 12. 单元测试要求

- 全部测试**不得**依赖网络、不得依赖真实宿主；用假 `hostCaller` + 假时钟（注入 `now func() time.Time`）。
- 覆盖 §4/§5/§6/§7/§8/§9/§10 各节验收里列出的用例（**一条都不要跳**）。
- 至少包含一条「冷却后不再被选中」与一条「退避翻倍」的用例。
- `go test ./... -race` 必须通过（并发安全：所有共享状态用 mutex/atomic）。

### 验收命令（必须自己跑并贴真实输出）

```bash
gofmt -l .                                   # 必须无输出
go vet ./...
go test ./... -race 2>&1 | tail -20
go test ./... -v 2>&1 | grep -c -- '--- PASS'   # 报出通过数
go test ./... -v 2>&1 | grep -c -- '--- FAIL'   # 必须是 0
CGO_ENABLED=1 go build -buildmode=c-shared -o bin/cpa-router.so .
nm -D bin/cpa-router.so | grep -c cliproxy       # 应 >= 4
```

---

## 13. README.md 内容

- 一句话说明 + 能力列表。
- 安装：拷 `.so` 到 `/CLIProxyAPI/plugins/`，在 `config.yaml` 加 `plugins.configs.cpa-router`（给出完整 YAML 示例），重启或热重载，用 `GET /v0/management/plugins` 确认 `registered/effective_enabled`。
- 组语法说明（含嵌套示例：`free-stack = [wb2api-x, cheap(rr: [a, b])]`）。
- 调用示例：`curl -X POST .../v1/chat/completions -d '{"model":"free-stack",...}'`。
- 冷却/退避规则与参数表。
- UI 用法（怎么打开、怎么拿 key）。
- 已知限制：组名不能与真实模型名重名；冷却状态仅内存（重启清空）；新建组后模型列表刷新依赖宿主重新注册（见 §10.3 第 5 条）；`name_prefix` 为空时组名占用全局模型名空间。

---

## 14. 禁止项（review 时逐条检查）

1. 不要在插件里保存/读取上游密钥，不要自己发上游 HTTP 请求（必须走 `host.model.*`）。
2. 不要改宿主 `config.yaml`、不要改 CPA 仓库、不要写 `plugins/` 之外的文件。
3. 不要用字符串匹配 `err.Error()` 里的 "429" 来判断限流——用信封里的 `http_status`。
4. 不要给资源页 GET 加副作用；不要加载第三方脚本。
5. 不要为了「跑通测试」而写假实现/假输出。
6. 不要顺手重构与需求无关的部分；不要新增需求之外的配置项。
