# cpa-router

一个给 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 使用的
**C ABI 动态库插件**（`.so`），提供类似 9Router "combo" 的**命名模型组**能力。

## 一句话说明

把多个真实模型聚成一个"组"，客户端直接用组名当 `model` 调用；插件按策略在成员间
分发，并复用宿主既有执行链路（渠道解析、别名映射、密钥选择、用量统计、日志全部仍由
宿主负责），不自己保存密钥、不自己发上游请求。

## 能力列表

- **命名组**：一组模型起一个名字（`model` 直接传组名即可调用）。
- **两种策略**：
  - `round-robin`（轮询组）：在组成员之间轮流分发；
  - `fallback`（降级组）：按声明顺序依次尝试，成功即止。
- **可嵌套**：组的成员可以是另一个组（任意层，加载时检测环）。
- **429 移除 + 指数退避**：某成员返回 429（或配置的 `cooldown.statuses`）时把它从可用
  池里**摘掉**（冷却），冷却时长按指数退避增长；冷却中的成员不会被选中（除非整组都在
  冷却，见 `all_cooling_policy`）。
- **UI**：CPA 管理面板里出现「模型路由」菜单，可视化增删改查组、看冷却/统计、发测试请求。
- **管理 API + 资源页**：`/v0/resource/.../panel` 单文件面板，`/v0/management/...` 下
  提供 state / groups 增删改 / reset / test 接口。

## 安装

1. 编译：

   ```bash
   make build      # 等价于 CGO_ENABLED=1 go build -buildvcs=false -buildmode=c-shared -o bin/cpa-router.so .
   ```

2. 把产物拷到宿主插件目录：

   ```bash
   cp bin/cpa-router.so /CLIProxyAPI/plugins/
   ```

3. 在宿主 `config.yaml` 加（优先级须高于 `qoder` 的 5，保证路由插件先被询问）：

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     configs:
       cpa-router:
         enabled: true          # 宿主字段
         priority: 20           # 宿主字段：必须高于 qoder(5)
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

4. 重启或热重载宿主，确认已注册：

   ```bash
   curl -X GET .../v0/management/plugins | jq '.plugins.cpa-router'
   # 期望 registered / effective_enabled 为 true
   ```

5. 打开管理面板的「模型路由」菜单即可看到组列表与测试面板。

## 组语法说明

状态文件（默认 `plugins/cpa-router/groups.yaml`，相对宿主工作目录）示例：

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
  - name: "cheap"
    strategy: "round-robin"
    members:
      - model: "a"
      - model: "b"
```

嵌套示例：`free-stack = [wb2api-x, cheap(rr: [a, b])]`。

校验规则：
- `version` 必须为 `1`；`name` 非空、唯一、匹配 `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`。
- 每个成员**恰好**有 `model` 或 `group` 之一；`group` 引用的组必须存在。
- 别名不得与组名或其它组别名冲突。
- 加载时做环检测（DFS），例如报错 `组配置错误: 检测到循环嵌套: a -> b -> c -> a`。
- 运行期还有硬编码深度上限 8（超过该次请求直接报错）。

## 调用示例

```bash
curl -X POST .../v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"free-stack","messages":[{"role":"user","content":"你好"}]}'
```

## 冷却 / 退避规则与参数表

- 命中 `cooldown.statuses`（默认 `[429]`）时：
  `level += 1`；`delay = min(base * factor^(level-1), max)`；`delay *= 1 + rand(-jitter, +jitter)`；
  `until = now + delay`。例（默认参数，jitter=0）：30s → 60s → 120s → 240s → … → 封顶 30m。
- 命中 `cooldown.transient_statuses`（默认 5xx）或**无状态码的调用错误/超时**时：用
  `transient_*` 参数做同样的退避。
- 其它 4xx（400/401/403/404/405 等）：**只记统计，不冷却**，继续尝试下一个成员。
- 成功（2xx）时：`level = 0`、清除冷却、直到时间为 0。
- 是否生效看 `now < until`；`cooldown.disable == true` 时永不冷却（仍会尝试下一个成员）。
- 冷却时长取整到秒写进日志与 UI。

| 参数 | 含义 | 默认 |
|---|---|---|
| `cooldown.statuses` | 触发退避的状态码 | `[429]` |
| `cooldown.base` | 退避基数 | `30s` |
| `cooldown.factor` | 退避因子 | `2.0` |
| `cooldown.max` | 退避封顶 | `30m` |
| `cooldown.jitter` | ±随机抖动 | `0.2` |
| `cooldown.transient_statuses` | 5xx 触发退避 | `[500,502,503,504]` |
| `cooldown.transient_base` | 5xx 退避基数 | `15s` |
| `cooldown.transient_max` | 5xx 退避封顶 | `5m` |

## UI 用法

- 在 CPA **管理面板**中以 iframe 打开 `/v0/resource/plugins/cpa-router/panel` 会自动从
  宿主获取管理密钥（零配置）。
- 直接打开页面（非 iframe 内嵌）时只能手填密钥：在 CPA 主面板里随便调一次管理接口，
  从浏览器拿到 `managementKey`，填到本页输入框即可（仅存 `sessionStorage`）。
- 页面功能：组卡片列表（策略徽标、启用开关、成员与冷却、统计、编辑/删除/重置冷却/测试）、
  新建/编辑弹窗、测试面板（选组 + prompt → 展示状态码/耗时/每次尝试/响应前 500 字符）、
  顶部刷新与生效配置摘要。
- 「添加成员」选择器把成员按前缀分组；**已不在宿主模型列表里的成员（渠道被删/上游下线）
  单独列在最上面的「已失效（不在模型列表）」分区**（红色、标 ✓），点一下即取消选中、
  回到外层点「保存」就会把该成员从组里删掉。模型清单尚未加载完时不显示该分区，避免把正常
  成员误判成失效。

## 已知限制

- 组名不能与真实模型名重名，否则路由时无法区分。
- 冷却状态仅内存（重启清空）。
- 新建/修改组后，模型列表刷新依赖宿主重新注册（见面板 §5：保存后发
  `PATCH /v0/management/plugins/cpa-router/config` 让宿主重新注册；失败不报错，提示里说明
  「模型列表需重启或手动刷新才会更新」）。
- `name_prefix` 为空时，组名占用全局模型名空间。
- 插件**不**读取/保存上游密钥、**不**自己发上游 HTTP 请求，所有执行都走 `host.model.*`。
