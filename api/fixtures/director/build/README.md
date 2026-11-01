# AI 搭景的解析与求解用例（`build/`）

> DRT-18 建立（docs/agent/director/dev/01 §4.3 / §4.4，dev/02 §6）。改任何一份，都要让读它的几方一起过。

| 文件 | 谁在读 | 内容 |
| --- | --- | --- |
| `raw-*.txt` | Go：`internal/director/domain` 的 `TestParseDraft_raw用例全部符合期望`（以及各 `TestParseDraft_*`）；DRT-30 的假上游 | 模型的原始输出：代码围栏、首尾杂文、末尾逗号、白名单外的码、自带坐标、空方案、全部不合法、通篇文字、被截断 |
| `raw-expect.json` | 同上 | 每个 `raw-*.txt` 的期望：`ok`、`objects`（对象数）、`camera`、`notes`、`droppedCount`、`droppedContains`（丢弃说明里必须出现的片段）、`reasonContains`（失败时的原因）。**新增 raw 文件必须在这里写期望**，否则测试红 |
| `case-*.json` | Go：`TestSolve_fixture用例全部通过`；DRT-30 的假上游 | `{"why", "scene", "draft", "expect"}`，见下 |
| `fake-reply.json` | DRT-30 的假上游（`scripts/fake-upstream/director.go`；`TestDirectorReply_与对峙用例同一份方案` 核对两份一致） | 假上游给搭景请求的固定回包，**就是** `case-default-scene-standoff.json` 的 `draft`：默认场景上求解出 3 个角色、1 个机位（E2E 第 265 步按这个断言） |

## `case-*.json`

- `scene`：输入场景（必须通过 `Validate`）。
- `draft`：**就是模型按输出契约写的那个 JSON 对象**（`objects` / `camera` / `notes`）。测试把它序列化后走完整条 `ParseDraft → Solve → ApplyProposal`；假上游把它原样当作模型回复。
- `expect`（都可省略，写了才核对）：

| 键 | 含义 |
| --- | --- |
| `added` / `updated` / `removed` | 变更清单各有几项 |
| `characters` / `cameras` / `props` / `crowds` | 建议场景里各类对象的数量 |
| `minGapM` | 角色两两水平距离的下限（至少一方是 AI 摆的） |
| `grounded` | AI 摆的对象全部贴地（`position.y == ground.height`） |
| `leftOf` | `[[A, B], …]`：从输出机位看，A 的屏幕横坐标小于 B（按名称；用机位存下的 XYZ 欧拉角 + `camera.roll` 重建视线，不读求解器内部） |
| `framing` | `{targets, shotSize}`：机位正对目标包围盒的注视点，且「画面高 ÷ 目标高」等于该景别的 coverage（误差 1%）。只用于目标全是角色的用例 |
| `pitch` / `pitchDeg` | 输出机位仰视（`up`）/ 俯视（`down`），或具体俯仰角（误差 0.5°） |
| `roll` | 输出机位的 `camera.roll` |
| `aspect` | 输出机位的画幅 |
| `outputCamera` | `new`（新建的机位）或具体 ID |
| `unchanged` / `absent` | 这些 ID 的对象逐字不变 / 已被删除 |
| `updatedContains` | 变更清单的 `updated` 里必须有这些 ID |
| `keepsField` | `{id: 字段}`：被修改的对象保留了这个未知字段 |
| `faces` / `facesCamera` | `[[A, B]]` A 面向 B；`[A]` A 面向输出机位（误差 1°） |
| `defaultsContain` | 提案的 `defaults` 里必须出现的片段 |

现有用例覆盖：观看者左右（含侧面机位、沿用用户机位）、三人对峙低机位（DR-AC-27）、围坐、群众加机位、重搭保留（DR-AC-28）、8 档景别 × 5 种角度、找不到目标、没写机位、默认场景直接搭景、沿用竖屏画幅。

## 约定

- 坐标约定同 `../README.md`：米、Y 向上；角色 `rotation.y = 0` 面朝 +Z；机位 rotation 全 0 朝 −Z 看。
- 左右、前后都按**观看者**（输出机位的屏幕）算（DRD-27）。
- 草案里不写坐标；写了也会被忽略并写进丢弃说明。
