# 智能剪辑共享 fixture

> 前后端共用（docs/agent/smartcut dev/02 §6、SC-N-15）。改任何一份，都要让读它的两边一起过。
> 各子目录由对应的任务卡产出，落地一个补一行。

| 目录 / 文件 | 谁在读 | 怎么改 |
| --- | --- | --- |
| `content/valid-*.json` | Go：`internal/timeline/domain` 的 `TestValidateContent_合法内容全部通过`、`TestDecodeContent_*`；前端：`utils/smartcutContent.test.ts`（SCT-18） | 手写：整份内容本身。`valid-full.json` 每个原样 JSON 字段（变换、裁切、轨道样式、闪避、字幕样式、锚点、文字样式、位置框）都带 `future*` 未知字段，校验必须放行且原样保留 |
| `content/invalid-*.json` | Go：`TestValidateContent_非法内容报出字段路径`；前端：SCT-18 的保存前校验 | 手写：`{"why", "content", "path"}`，**每份只有一处毛病**，两边都要报同一个 `path` |
| `content/aspects.json` | Go：`TestAspectPresets_与fixture一致`；前端：SCT-18 的画幅表 | 手写：`presets` 是固定画幅与默认尺寸（与 `domain.AspectPresets` 逐项相等）；`formats` 是「画幅 + 帧率 + 宽高 → 期望 path」，空串表示合法 |
| `layout/*.json` | Go：`internal/timeline/domain` 的 `TestLayout_与fixture一致`（SCT-04）；前端：SCT-18 的成片时间换算 | 手写：`{"why", "content", "expect"}`。`content` 本身必须合法（测试先过 `ValidateContent`）；`expect` 是 `Layout` 的结果：`clips`（按 ID 的 `startMs` / `durationMs`）、`subtitles` 与 `texts`（按 ID 的 `startMs` / `endMs` / `hidden`）、`totalMs`、`effectiveMs`。期望值在生成脚本里**手算**写死，不拿任一侧的实现去算 |
| `inputs/*.json` | Go：`internal/wiring` 的 `TestSmartcutInputs_与fixture一致`（画布 → `canvas/domain.InboundSources` → `smartcut/domain.ResolveInputs`，服务端同一条路，SCT-25）；前端：`utils/smartcutInputs.test.ts` | 手写：`{"why", "nodeId", "canvas", "lookup"?, "clips"?, "expect"}`。`lookup` 是服务端回查到的事实：`generations[生成ID] = {status, operation?, reason?, outputs?}`（`outputs` 缺省 = 没查，退回快照；`[]` = 查过、确实没有产物）、`media[媒体ID] = {status: ready \| invalid, reason?, durationMs?, hasAudio?, posterMediaId?}`；`clips` 是时间线里的片段（`id` / `sourceNodeId` / `mediaId`）。`expect` 是素材条 `items`（契约 `SmartcutInput` 的形状）、`omitted`、`disconnected`（来源已断开的片段 ID）、`unusedCount` |
| `draft/*.json` | Go：`internal/smartcut/domain` 的 `TestDraftFixtures`（SCT-26）；假上游（SCT-42）取其中的原始输出 | 手写：`{"why", "mode", "refs", "raw", "expect"}`。`refs` 是这一次能引用的编号（`cards` 素材卡片、`shots` 镜头 b1~bN、`sentences` 句子 s1~sN、`clips` / `texts` 追加指令的 c1~cN / t1~tN、`items` 翻译条数）；`raw` 是模型原文（围栏、杂文、截断、写死时间、不存在的编号都在这里）；`expect` 二选一：`invalid`（`ErrDraftInvalid.Reason` 原文，可带 `dropped`），或 `draft`（解析后的方案，`Draft` 的 JSON 形状）+ `dropped`（丢掉的项，每条一句、逐字）+ `ignored`（被忽略的时间字段说明） |
| `compile/*.json` | Go：`TestCompileFixtures`（SCT-26）：`raw` 先过 `ParseDraft` 再 `Compile`，`fallback: true` 走 `Fallback`；每份都另验「过 `ValidateContent`、`Layout` 总长与结果一致、两次编译逐字节相同」 | 手写：`{"why", "mode", "cards", "spec", "raw" \| "fallback", "beatGrid"?, "shots"?, "transcripts"?, "peaks"?, "script"?, "base"?, "expect"}`。`beatGrid[编号] = {intervalMs, untilMs, downbeatEvery}` 生成等距节拍；`transcripts[编号].utterances` 每句是若干段 `{"chars", "startMs", "charMs"}`（每字等长）或 `{"text", "startMs", "endMs"}`；`peaks[编号] = {intervalMs, lengthMs, base, set: [[起, 止, 值]]}`。`expect.video` 每段写成 `"媒体ID 入~出[ ×速度][ ←转场时长][ fade入/出]"`，`subtitles` / `title` 写成 `"成片起~止 文字"`，`music` / `voice` 写成 `"媒体ID 入~出[ fadeOut毫秒]"`。期望值按 dev/01 §4.3 的规则**手算**（各文件的 `why` 写了怎么算），不拿实现生成 |
| `asr/*.json` | Go：`internal/provider/protocol` 的 `TestASRFixtures`（SCT-33）：`body` 按 `protocol` 与 `phase` 交给对应协议的解析（`status` 与 `headers` 原样传入），比对产物的文字与 meta。`volc_talk_fillers.json`（3 次「嗯」+ 一句重复）与 `volc_narr_voice.json`（20 秒解说配音，有同音错字与漏字）的 `expect.meta` 同时是 `internal/smartcut/app/integration` 里口播精剪（`TestTalkPlan_*`，SCT-34）与解说成片（`TestNarrPlan_*`，SCT-35）集成测试的识别结果：改它们的时间要同步改那边写死的剪点、字幕，以及 `compile/talk-three-fillers.json`、`compile/narr-voice-script.json` | 手写：`{"name", "protocol", "phase": submit \| query, "status", "headers", "body", "expect": {"text", "meta"}}`。`body` 是两家的原始响应；`expect.meta` 是统一后的识别结果（`granularity` / `durationMs` / `language` / `utterances[{text, startMs, endMs, words[{text, startMs, endMs}]}]`），**时间一律整数毫秒、手算写死**（OpenAI 的秒乘 1000 四舍五入；词按起点挂到它落进的那一句，落在句与句之间的归前一句） |

## 约定

- 媒体 ID、对象 ID 一律是字符串。正式 ID 是数字串（1~19 位、大于 0）；新建对象用临时 ID `tmp-` + 1~40 个 `[A-Za-z0-9_-]`。
  **轨道、片段、字幕、文字四类对象的 ID 在整份内容里必须唯一**（保存后共用一张 `idMap`）。
- 所有时间是整数毫秒，所有位置是**成片时间**（SCK-15）。片段在成片里的长度
  `playLen = floor((sourceOutMs − sourceInMs) × 100 / speed)`（`speed` 缺省 100），与 Go 的 `Clip.PlayLen` 同一个整数算法。
- 「最多 N 个字」按 Unicode 码点数（Go 的 `utf8.RuneCountInString`；JS 用 `[...s].length`，不用 `s.length`——一个 emoji 在 UTF-16 里占 2）。
- 错误路径的格式（与导演台同一套）：点号分段，数组用方括号下标，例如 `tracks[2].clips[5].transform.scale`；整份不是对象时为 `$`。
- 只拦结构性损坏与上限，不顺手修复；缺省的可选字段放行（前端补默认值），`null` 与缺省同义。

## 内容校验的规则与顺序（`ValidateContent`）

按下面的顺序遍历，**遇到第一处错误就停**：同一份坏数据两边每次报同一个 path。

1. 整份是对象，否则 `$`。
2. `version`：必填整数，≥ 0。
3. 画幅与尺寸，顺序为 `aspectRatio` → `fps` → `width` → `height` → 宽高自洽：
   - `aspectRatio` 必填字符串，取 `16:9` / `9:16` / `1:1` / `4:3` / `3:4` / `21:9` / `custom`；
   - `fps` 整数 1~120；`width`、`height` 整数 64~7680 且为偶数；
   - 固定画幅要求 `|w/h − 默认宽/默认高| / (默认宽/默认高) ≤ 2%`，否则报 `height`；`custom` 不判比例。
4. `tracks`：必填数组。逐条（`tracks[i]`）：
   1. 是对象；`id` 格式与唯一；`kind` 取 `video` / `audio` / `subtitle` / `text`；
   2. `name` 字符串，最多 64 个字；
   3. `role`：音频轨取 `music` / `voice` / `sfx`；画面轨只能是 `voice`；字幕轨与文字轨不能有；
   4. `lang`：只有字幕轨能有，格式 `^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`，最长 16；
   5. `isHidden`、`isMuted` 布尔；`volume` 整数 0~200；
   6. `style` 是对象：字幕轨按「文字样式（含 `bottomPct`）」校验；音频轨校验 `style.duck`（`enabled` 布尔、`depthDb` 0~40）；其余种类不校验内容；
   7. `clips` 是数组；字幕轨与文字轨不能有片段（非空即报 `tracks[i].clips`）；逐段（`tracks[i].clips[j]`）：
      `id` → `mediaId`（必填数字串）→ `sourceInMs`（必填整数 ≥ 0）→ `sourceOutMs`（必填整数，> sourceInMs）→
      `startMs`（画面轨忽略；其它轨必填整数 ≥ 0）→ `speed`（25~400）→ `volume`（0~200）→
      成片长度 ≥ 100ms（否则报 `sourceOutMs`）→ `fadeInMs`、`fadeOutMs`（整数 ≥ 0，合计不超过成片长度，否则报 `fadeOutMs`）→
      `transitionIn` → `transitionOut` → `transform` → `origin`（`ai` / `user` / `sync`）→ `sourceNodeId`（最长 64）→
      `sourceShotId`（数字串）→ `alternates` → 非画面轨的 `startMs + 成片长度 ≤ 30 分钟`（否则报 `startMs`）。
      `anchor`（跟随片段，SCT-39）只有音频轨上的片段能带：画面轨上带了在 `id` 之后就报 `tracks[i].clips[j].anchor`；
      音频片段的锚点先记下，**所有轨道看完、片段总数查过之后**再逐个按「锚点」的规则查，指向本内容里的片段时那一段
      必须在画面轨上（否则报 `…anchor.clipId`）；
   8. 画面轨逐段之后：成片总长（各段成片长度之和，减去第 2 段起每个生效的转入时长）≤ 30 分钟，否则报 `tracks[i].clips`。
5. 轨道条数：画面轨恰好 1 条；音频轨 ≤ 6、字幕轨 ≤ 4、文字轨 ≤ 4；片段总数 ≤ 500。都报 `tracks`。
6. `subtitles`：数组，≤ 2000 条（报 `subtitles`）。逐条：`id` → `trackId`（必须是这份内容里的字幕轨）→
   `startMs`（整数 ≥ 0）→ `endMs`（整数，> startMs，≤ 30 分钟）→ `text`（去掉空白后非空，最多 500 个字）→
   `style`（文字样式，含 `bottomPct`）→ `anchor` → 同一条字幕轨上起止完全相同的第二条报 `startMs`（**重叠是允许的**）。
7. `texts`：数组，≤ 100 个（报 `texts`）。逐个：同字幕的前五项（`trackId` 必须是文字轨）→ `style`（文字样式）→ `box`。
   全部文字层逐个看完之后，按文字轨在 `tracks` 里的顺序查同轨重叠（见下）。

**同轨重叠**（SCD-15，SCT-04）：音频轨的片段、同一条文字轨上的文字层不许重叠；字幕轨允许（两个人同时说话）。
**带锚点的音频片段不参与**（SCT-39：落点跟着画面走）。
按（起点，数组下标）排序后看相邻两个，后一个的起点早于前一个的结束即为重叠，**报后一个**（按它在数组里的下标）：
音频片段报 `tracks[i].clips[j].startMs`（在第 4 步该轨的片段逐段看完之后、第 8 步之前），文字层报 `texts[k].startMs`。
首尾相接（起点 = 前一个的结束）不算重叠。音频片段的结束 = `startMs + playLen`。

各子对象（缺省或 `null` 都放行；出现了但不是对象报它自己的路径）：

| 对象 | 规则（未列出的键原样保留、不校验） |
| --- | --- |
| 转场 `transitionIn` / `transitionOut` | `kind` 必填，取 `none` / `fade` / `dissolve` / `slide_left` / `wipe`；`durationMs` 整数。`none` 或时长 0 = 没有转场。生效时 100~3000ms，且：画面轨的转入不超过本段与上一段各自的一半（第一段的转入是从黑场淡入，只看本段）；画面轨**中间段**的转出只校验取值（会被忽略）；最后一段的转出与非画面轨的转场不超过本段的一半。超了报 `durationMs` |
| 变换 `transform` | `x`、`y` −100~100；`scale` 0.1~5；`rotation` −180~180；`crop` 是对象时 `left` / `top` / `right` / `bottom` 各 0~45 |
| 备选 `alternates` | 数组，≤ 10 个（报 `alternates`）；逐个：`mediaId` 必填数字串 → `sourceInMs` → `sourceOutMs`（> sourceInMs）→ `kind`（必填，`original` / `regen` / `desubtitle`）→ `generationId`（可选数字串） |
| 文字样式 `style` | `fontSizePct` 2~20 → `color` → `stroke`（`color`、`widthPct` 0~5）→ `background`（`color`、`opacity` 0~1、`paddingPct` 0~10）→ `align`（`left` / `center` / `right`）→ `maxWidthPct` 10~100 → `bottomPct` 0~50（只有字幕的样式校验这一项）。颜色只收 `#RGB` / `#RRGGBB` / `#RRGGBBAA` |
| 锚点 `anchor` | `clipId` 格式同对象 ID；**临时 ID 必须指向这份内容里的片段**，正式 ID 可以指向已经不在的片段（片段被删时锚点失效，字幕按存储的起止留在原处——这是正常状态，不拦）→ `srcStartMs`（整数 ≥ 0）→ `srcEndMs`（> srcStartMs） |
| 位置框 `box` | `xPct`、`yPct` 0~100；`anchor` 取 `top` / `center` / `bottom` |

不在这里判的：素材种类与轨道是否匹配（要查媒体表，整份保存时由 app 层判）。

## 成片时间（`Layout`，SCT-04）

- 画面轨：`start[0] = 0`；`start[i] = start[i−1] + playLen[i−1] − 转入[i]`。只扣**生效的**转入（类型不是 `none` 且时长 > 0），且第一段的转入（从黑场淡入）不扣。`totalMs` = 各段 playLen 之和；`effectiveMs` = 最后一段的结束（不小于 0）。
- 音频轨片段、没有锚点的字幕、文字层：起止就是存储值。
- 跟随片段的字幕（有 `anchor` 且所属片段在内容里，可以是任何轨上的片段）：取素材区间与片段当前取用区间的交集 `[lo, hi)`，
  `start = 片段成片起点 + (lo − sourceInMs) × 100 / speed`、`end` 同理，整数除法向下取整；
  交集为空或**不到原区间（`srcEndMs − srcStartMs`）的一半**时 `hidden: true`，起止回存储值。
- 所属片段不在内容里：起止回存储值，不隐藏。
- 跟随片段的音频（音频轨上带 `anchor` 的片段，所属片段在画面轨上，SCT-39）：交集 `[lo, hi)` 同上，
  `start = 所属片段成片起点 + (lo − sourceInMs) × 100 / speed`；`expect.clips[id]` 另带
  `follow: {sourceInMs: lo − srcStartMs, sourceOutMs: hi − srcStartMs, speed: 所属片段的速度}`，`durationMs` 按它算。
  交集**不足 100 毫秒**（不是一半）时 `follow.hidden: true`，起点、时长与 `follow` 的取用区间都回存储值。
  所属片段不在内容里或不在画面轨上：按存储值、不带 `follow`。
