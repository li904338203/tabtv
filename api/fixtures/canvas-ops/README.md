# 画布操作共享 fixture（AGT-10 / AGT-11）

Go（`canvas/domain.Apply`，服务端应用）与前端（重放）**用同一批 fixture 钉住**，任一侧与期望不一致即失败。

## 用例文件（`0NN-*.json`）

| 字段 | 含义 |
| --- | --- |
| `actor` | `{kind:"agent"\|"user"\|"mcp", userId, sessionId, grantId, clientName}`，ID 一律字符串；`sessionId` 只对 agent、`grantId`/`clientName` 只对 mcp（MCPT-09） |
| `newIds` | 服务端分配 ID 的序列：每个新建对象（节点、连线、画框）按操作顺序取下一个 |
| `before` / `after` | 落库形状的 `canvasData`（扁平节点 `{id,type,title,position,size,params,result,agent,origin}`） |
| `ops` | Agent 提交的操作（可含批内引用 `ref:xxx`，节点 / 连线不带 ID） |
| `applied` | **已解析**的操作：引用换成真实 ID，新建项带完整节点 / 连线。它就是 `canvas_op_log.ops` 与 `canvas.ops_applied.ops` 的内容 |
| `nodeIds` | 本批新建的节点 ID（含画框，按创建顺序） |
| `expectError` | 期望整批被拒：`not_owned` / `node_not_found` / `invalid_op` / `invalid_canvas`（此时没有 `after`） |

`link_generation`（007 / 008）只由 Agent 的 generate 工具在受理成功后产生（canvas_apply 不开放）：`patch` 只有 `generationId`（字符串），把节点 `result` 整体替换为 `{id, status:"queued", progress:0}`；只认会话、不看 `userTouched`；同一任务已挂着时不动。

## 来源字段（MCPT-09，009~015）

- Agent 建的节点写 `agent:{sessionId,…}`（不变）；MCP 建的节点只写 `origin:{kind:"mcp", grantId, clientName}`。服务端按 actor **强制写入**：MCP 剥掉客户端带来的 `agent`/`origin`，Agent 额外剥掉 `origin`，user 两者都剥。
- 读取只经一个函数（Go `domain.NodeOrigin`、前端 `utils/canvasOrigin.ts nodeOrigin`）：有 `origin` 以它为准，只有 `agent` 视为 kind=agent；`userTouched` 两处任一为 true 即为 true。
- 所有权 = 来源主体一致（agent 比 sessionId、mcp 比 grantId）且没被用户动过；Agent 与 MCP 互不可改对方的节点。`link_generation` 只比主体、不看 `userTouched`。

- Go 侧：`Apply(before, ops, actor, newIds…)` 的结果与 `after`、`applied`、`nodeIds` 语义相等（JSON 解析后比较，键序不计）。
- 前端侧：把 `applied` 依次重放到 `before` 上，结果与 `after` 语义相等（重放不分配 ID、不认识批内引用）。

## `addnode-shape.json`

前端 `addNode` + `document()` 落库的节点形状。Go 断言 Agent 用同样的 `type/title/params` 建出的节点**除 `id`、`agent`、`position` 外逐键相同**；前端断言 `addNode` + `document()` 与 `node` 一致（`id`、`position` 除外）。
