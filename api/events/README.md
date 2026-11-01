# 事件与 WebSocket 协议

本目录是**事件契约的唯一来源**。JSON Schema 方言为 **2020-12**，与 OpenAPI 3.1 对齐（见 [ADR-0001 TECH-10](../../docs/adr/0001-技术基线.md)）。

> ⚠️ **事件类型目前没有代码生成**（`make gen` 只生成 HTTP 契约）。所以要靠测试守住两边：
> 后端的 `generation.update` 由 `internal/generation/app/events_test.go` **直接读本目录的 schema** 逐项核
> （required 一个不能少、properties 之外一个不能多）；前端只读事件里的 `type` / `generationId` / `projectId`，
> 把它当「该去对账了」的信号（见下文「投递语义」），所以不依赖其余字段的形状。
> 此前后端用 map 拼了六个字段、失败事件里没有原因，而没有任何东西发现它（CD-73）。

## 现状（CD-73）

- **生成事件只在终态推**：成功 / 失败（Worker）与取消 / 运维补偿（API）。进度不推，靠轮询。
- **按工作区投递**（`workspace:{id}`），所以订阅要订工作区 topic；订 `project:{id}` 收不到生成事件。
- **投递链路（AGT-02）**：任意进程经 `internal/platform/eventbus` 把信封 `{"topic":"<kind>:<id>","payload":{...}}`
  发到 Redis 频道 `events:bus`（实际频道带命名空间前缀），API 进程的 `eventbus.Bridge` 订阅后按 `topic` 把 `payload`
  **逐字节原样**投给订阅了该 topic 的连接 —— 浏览器收到的是裸 payload，看不到信封。
  `topic` 必须是 `[a-z]+:<规范十进制正整数>`、`payload` 必须是合法 JSON，否则在**发布方**就被拒、不进 Redis。
  总线只做格式校验不做授权：谁能收到由上面的订阅鉴权决定。pub/sub 尽力而为，订阅者不在线就丢（见「投递语义」）。
- **过渡期两条桥并存**：旧频道 `events:generation`（信封 `{workspaceId, payload}`）的 `generation/app.EventBridge`
  已标 Deprecated，只为在滚动发布期间接住还没升级的旧 Worker 的事件，**下一版本删除**。
  发布方不双发，所以不会重复投递。发布顺序：**先发含新桥的 API，再发 Worker**（反过来的话新 Worker 发到 `events:bus`
  而旧 API 不听，期间生成事件全丢，前端靠轮询兜底）。
- DAG / 渲染 / 通知三类的 schema 已定稿，但**服务端还没有推**（前端相应地用轮询）；有了总线之后它们有通道可走，
  接线不在 AGT-02。
  会话撤销（封禁、改密）目前以 `subscription.revoked`（`reason=session_revoked`）+ 关闭连接表达，
  不是 `session-revoked.schema.json` 里的 `session.revoked` 事件。
- **跨源**：页面与 API 不同源时（开发时 :3000 → :8180），要把页面的 host 配进 `SEEAITV_WS_ALLOWED_ORIGINS`，
  否则握手被拒（`Origin ... is not authorized`）。同源（生产经反向代理）不必配；本地 `Makefile` 默认 `*`；production 下不许配 `*`。
- **连接地址跟着页面的主机名走**（CD-74）：开发时 `ws://<页面主机名>:8180/ws/events`，生产构建连页面自己的域名；
  显式给 `NUXT_PUBLIC_WS_URL` 则照用。此前写死 `ws://localhost:8180`，从别的设备打开页面就连不上。

## 端点

| 端点 | 用途 |
|---|---|
| `/ws/events` | 用户端唯一的实时通道：生成、DAG、渲染、通知、会话撤销 |

⚠️ 旧系统按业务开了 `/ws/generation` 与 `/ws/writing` 两条连接。新系统**只开一条**：写作域已排除，而 DAG 与渲染再各开一条就会让浏览器同时维持三条长连接。事件类型由 `type` 区分，订阅按资源粒度。

## 握手与订阅

```
客户端 → {"type":"auth","token":"<accessToken>"}
服务端 → {"type":"auth.ok","userId":"...","sessionId":"..."}        // 失败则 auth.error 后关闭
客户端 → {"type":"subscribe","topics":["project:123","workspace:9"]}
服务端 → {"type":"subscribe.ok","topics":[...],"rejected":[...]}
服务端 → {"type":"generation.update", ...}                          // 后续推送
```

**token 放首帧，不入 URL**（URL 会进网关日志、浏览器历史与 Referer）。

### 订阅授权（P00-03 明确项）

`subscribe` 的每个 topic 都要**逐条鉴权**，不通过的放进 `rejected` 而不是整体失败：

| topic | 授权条件 |
|---|---|
| `project:{id}` | 该 project 的 `workspace_id` ∈ 当前用户的 active 成员资格，且角色 ≥ `viewer`。承载 `canvas.ops_applied`（AGT-10，由应用操作的进程在事务提交后经总线发布；`ops` 超过 64KB 时省略并置 `truncated:true`，前端改走 `GET /projects/{id}/canvas/ops` 补取）；以及画布 Agent 的 `agent.state` / `agent.delta` / `agent.step` / `agent.card`（AGT-26：API 在发消息、提交卡片、停止、取消后发 state / card，Worker 在一轮运行中发全部四类）。同工作区的只读成员也能订阅 —— 他们可以查看别人发起的会话 |
| `workspace:{id}` | 当前用户是该工作区的 active 成员 |
| `generation:{id}` | 该任务的 `workspace_id` 同上 |
| `user:{id}` | 只能订阅自己 |

⚠️ **授权在订阅时校验一次不够**。成员被移除、共享被撤销、账号被封禁之后，已建立的订阅必须失效：服务端收到 `workspace_member.removed` / `resource_share.revoked` / `session.revoked` 时主动推 `subscription.revoked` 并停止投递（NEW-18、FULL-06）。

## 投递语义

- **事件不是事实来源**（GO-39）。数据库是。WebSocket 断开不丢事实；客户端重连后**先订阅再用 HTTP 对账**。
- 每个事件带 `occurredAt` 与业务 ID。客户端按「同一任务、终态不可被非终态覆盖」丢弃乱序与重复事件。
- 浏览器关闭**不中止服务端任务**。
- 事件由 `outbox_message` 驱动投递，发送失败不回滚已完成的任务事实（[08 §5.2](../../docs/08-Go工程落地蓝图.md)）。

## 文件

| 文件 | 内容 |
|---|---|
| `frames.schema.json` | 客户端→服务端帧、握手与订阅响应帧 |
| `generation-update.schema.json` | 生成任务状态推送 |
| `workflow-update.schema.json` | DAG 整次运行与节点状态 |
| `render-update.schema.json` | 时间线渲染任务状态；智能剪辑起由 Worker 真正发布（SCB-6），投递到 `project:{id}`，节流 1 次 / 秒 |
| `notification-created.schema.json` | 站内通知 |
| `session-revoked.schema.json` | 会话/订阅撤销 |
| `canvas-ops-applied.schema.json` | 服务端（Agent / MCP）应用了一批画布操作（AGT-10；`actor.kind=mcp` 时带 `mcpGrantId` 与 `clientName`，MCPT-09），投递到 `project:{id}`；后端由 `internal/canvas/app/events_test.go` 直接读 schema 核对 |
| `director-scene-updated.schema.json` | 3D 导演台场景保存成功（docs/agent/director，DRD-31），网页 / Agent / MCP 三条写入路径都推，投递到 `project:{id}`；只是对账信号，前端按 `version` 决定是否取正文；后端由 `internal/canvas/app/director_test.go` 直接读 schema 核对 |
| `timeline-updated.schema.json` | 时间线写入成功（docs/agent/smartcut，SCD-19）：整份保存、逐项接口、AI 剪辑落地、输入同步、快照恢复、片段回填六条写路径都推，投递到 `project:{id}`；只是对账信号，前端按 `version` 决定是否取正文 |
| `smartcut-updated.schema.json` | 智能剪辑进度信号：AI 剪辑阶段、输入同步结果、素材分析完成，投递到 `project:{id}`；节点卡据此刷新摘要 |
| `agent-state.schema.json` | 画布 Agent 会话状态 / 额度变化（AGT-26），投递到 `project:{id}`；只是对账信号，前端以 `GET /agent/sessions/{id}` 为准 |
| `agent-delta.schema.json` | 画布 Agent 助手文本流式增量，投递到 `project:{id}`；纯展示，丢了无所谓 |
| `agent-step.schema.json` | 画布 Agent 工具调用开始 / 结束，投递到 `project:{id}` |
| `agent-card.schema.json` | 画布 Agent 卡片发出 / 提交 / 作废 / 过期 / 报价更新，投递到 `project:{id}` |

四个 `agent-*` 的 Go 结构体在 `internal/agent/app/events.go`（API 与 Worker 共用），由同目录的
`events_test.go` 直接读 schema 核 required、properties（additionalProperties:false）、枚举与 ID 形态。
