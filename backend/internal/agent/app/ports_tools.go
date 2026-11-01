package app

import (
	"context"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 规划阶段免费工具（AGT-23）额外需要的端口。与 ports.go 分开放是为了并行开发时少撞文件；
// 实现都在 internal/wiring（agent 不 import 其他域的 repo / app 包，dev/01 T-1）。

// ---------------------------------------------------------------- read_canvas

// NodeGeneration 是画布节点**最近一次**生成任务（任意状态）的摘要。
// OutputMediaID 为 0 表示没有产物（排队中 / 失败 / 文本产物）。
type NodeGeneration struct {
	GenerationID  int64
	Status        string
	ErrorCode     string
	OutputMediaID int64
	OutputType    string
}

// NodeGenerations 按节点取最近一次生成（generation/repo.LatestByNode）。
type NodeGenerations interface {
	LatestByNode(ctx context.Context, ws, projectID int64) (map[string]NodeGeneration, error)
}

// ---------------------------------------------------------------- list_assets

// 素材来源（list_assets 的 source 参数）。media = 上传的参考素材；其余是脚本资产库的类型。
const (
	AssetSourceMedia     = "media"
	AssetSourceCharacter = "character"
	AssetSourceScene     = "scene"
	AssetSourceProp      = "prop"
)

// AssetQuery 是一次素材查询。Limit 已由工具夹到 1..20。
type AssetQuery struct {
	Source string
	// Kind 只对 media 有意义：image / video / audio，空 = 全部。
	Kind   string
	Cursor string
	Limit  int
}

// AssetItem 只回 ID / 名称 / 类型 / 缩略图 —— 描述与档案是外部内容，不进模型上下文。
type AssetItem struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	Type         string `json:"type"`
	ThumbMediaID string `json:"thumbMediaId,omitempty"`
}

// AssetPage 是一页素材。NextCursor 为空表示没有更多。
type AssetPage struct {
	Items      []AssetItem `json:"items"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// Assets 查本工作区的素材库与脚本资产库。
type Assets interface {
	ListAssets(ctx context.Context, ws int64, q AssetQuery) (AssetPage, error)
}

// ---------------------------------------------------------------- quote / submit_plan

// PricedItem 是一条计划项的报价结果。
//
// ⚠️ Item.Credits 由服务端报价写入；不可用时 Credits=0 且 BlockedCode 非空。
// 「不可用」不是错误：一项模型下线不该让整张规划卡发不出来（AGT-23 不阻断其余项）。
type PricedItem struct {
	Item domain.PlanItem
	// Unit 是目录里的计价单位（per_second / per_image / per_time / per_kchar）。
	Unit string
	// Available=false 时 BlockedCode / BlockedReason 说明原因，UserFixable 说明是不是用户自己能解决。
	Available     bool
	BlockedCode   string
	BlockedReason string
	UserFixable   bool
	// ArgFault 非空 = 不可用的原因是**调用方（模型）自己写错了计划项参数**（PlanFault*），
	// 不是用户或运营要处理的事。submit_plan 据此不发卡、把错误退回给模型当轮改正
	// （起因：模型凭空给 text2video 写了 inputs:{image:1}，卡片合计 0、确认禁用，
	// 用户只能点「需要调整」再花一轮钱）。分类在 wiring.quoteBlock，按业务码 + 细分原因，不匹配文案。
	ArgFault string
	// Operation 是实际用来报价的操作码（计划项没写时取类型默认值）；渲染为空。
	Operation string
}

// 计划项参数错误的类别（PricedItem.ArgFault）。
const (
	// PlanFaultOperation 模型码 / 操作码不存在，或该模型不支持这个操作。
	PlanFaultOperation = "operation"
	// PlanFaultInputs inputs 的角色或数量不符合该模型此操作的输入要求。
	PlanFaultInputs = "inputs"
	// PlanFaultQuantity 时长 / 张数等数量参数非法。
	PlanFaultQuantity = "quantity"
)

// PlanPricer 逐项报价。⚠️ 与批准时重新报价（PlanQuoter，AGT-26）**同一个映射**
// （wiring.PlanQuoteRequest）：映射各写一份的话，卡片上的价与批准时的价永远对不上。
// 只有内部错误才返回 error；业务上的「不可用」放在 PricedItem 里。
type PlanPricer interface {
	PriceItem(ctx context.Context, item domain.PlanItem, memberLevelID int64) (PricedItem, error)
}

// OperationCatalog 给出一个模型能用的生成方式，以及各自收什么输入（目录的能力矩阵，含继承）。
//
// 规划与生成据此在报价之前把「带了参考图却写成文生视频」改成该模型收得下的方式，
// 系统提示词据此告诉模型所选视频模型能做哪几种（04 文档「技能和模型参数」）。
type OperationCatalog interface {
	ModelOperations(ctx context.Context, modelCode string) ([]domain.OperationFit, error)
}

// ---------------------------------------------------------------- 营销规划（MKT-T-20）

// PlanPickQuery 是一条营销计划项要钉住 / 判定的选项。
type PlanPickQuery struct {
	// Refs 本项用到的选项（kind + code，按创意 → Hook → 风格）。
	Refs []ScenePickRef
	// Pinned 会话里已经钉住的（建会话时的选择 + 规划时新钉的）：命中的按版本历史取，**不重钉**
	// （同一会话同一 code 只认第一次的版本，否则同一张卡里两个变体可能用到同一选项的两个版本）。
	Pinned []ScenePin
	// OutputType 本项产出（video / image）：新钉的选项要适用它（Resolve 的 outputType）。
	OutputType string
	// ModelCode 本项模型：判「依赖出声的选项配了不出声的模型」（视频模型才判）。
	ModelCode string
	// Slots 本项各槽位的实际数量（product / character / source_video），判必填槽位。
	Slots map[string]int
}

// PlanPickFact 是一个选项在本项里的事实（不含片段原文）。
type PlanPickFact struct {
	Pin ScenePin
	// New 本次新钉（调用方要并进 scene_input.pins）。
	New bool
	// Tags 该版本的标签（口播提示据此判，见 PlanScene.TalkNotice）。
	Tags []string
	// BlockCode 非空 = 该选项在本项里不可用（营销域 BlockReason：产出不符 / 不出声 / 缺输入）；
	// UserFixable 用户自己能否解决（换模型、换技能、补图）。
	BlockCode   string
	BlockReason string
	UserFixable bool
}

// PlanScene 是营销会话规划要问目录与营销域的事（实现在 wiring）。
//
// ⚠️ 选图上限取自**报价用的那份能力矩阵**（catalog），不在 agent 里另写判断（MKR-2）；
// 选项可见性、出声 / 适用产出判定、口播标签都只有营销域一份实现（MKD-12、MKD-13）。
type PlanScene interface {
	// ImageLimit 取计划项（模型 × 操作；没写操作码时按类型取默认，与报价同一个映射）在 image 角色上
	// 的上限。found=false：模型 × 操作查不到或没声明能力矩阵 —— 交给报价去说明原因
	// （参数错误退回模型 / 运营问题照常发卡）。
	ImageLimit(ctx context.Context, item domain.PlanItem) (max int, found bool, err error)
	// PlanPicks 按顺序返回 Refs 的事实。未钉住的按 Resolve(outputType) 钉当前版本，失败原样返回
	// 42215（模型选了下线 / 不适用的选项，退回给模型改）。
	PlanPicks(ctx context.Context, q PlanPickQuery, now time.Time) ([]PlanPickFact, error)
	// TalkNotice 带「口播」标签时卡片上要追加的如实提示句；不带时返回 ""。
	TalkNotice(tags []string) string
}

// Balances 取付费主体当前余额（billing 实现；AgentAccounts 满足它）。
type Balances interface {
	Balance(ctx context.Context, payerID int64) (int, error)
}

// ---------------------------------------------------------------- 技能

// SkillRenderer 渲染技能正文（catalog/domain.RenderPrompt 的同一套占位符规则）。
//
// ⚠️ 缺变量 / 未闭合占位符一律返回错误，**不回落原文**：原文里的 `{{var}}` 会被模型
// 当成字面量照着生成而不报错（CLAUDE.md §5.7）。
type SkillRenderer interface {
	RenderSkill(content string, vars map[string]string) (string, error)
}

// SkillVars 是渲染技能正文时提供的**全部**变量。load_skill / read_skill_ref 与 Runner
// 组装系统提示词共用这一份 —— 两边给的变量不一样的话，同一个技能在工具里渲染成功、
// 进系统提示词时却失败（或反过来）。新增变量只改这里。
func SkillVars(state domain.State, reasoningModel string) map[string]string {
	return map[string]string{
		"state":          string(state),
		"reasoningModel": reasoningModel,
	}
}

// ---------------------------------------------------------------- 营销执行（MKT-T-22）

// SessionPlan 是 generate 判场景、取已批准计划项要用到的会话事实。
type SessionPlan struct {
	// Scene 会话场景（general / marketing）。
	Scene domain.Scene
	// Input 营销会话的 scene_input（通用会话为零值）。
	Input SceneInput
	// Items 最近一张已批准规划卡的条目（OperationCode 已按报价同一映射补成实际操作码）；
	// Approved=false 表示会话里还没有已批准的规划卡。
	Items    []domain.PlanItem
	Approved bool
	// CardID / Title 那张已批准规划卡（批量变体按它归进同一画框，MKT-T-27）。
	CardID int64
	Title  string
	// Steps 这张计划批准之后的 generate 步骤（按开始时间升序）：停后续按它现算（domain.ShouldStop）。
	Steps []domain.PlanStep
}

// PlanSource 是营销会话 generate 的读取端口（实现在 wiring：agent_session + agent_card + 脚本资产）。
//
// ⚠️ 付费生成必须对应已批准的计划项（MKD-9）：图与参数以卡片为准，确认卡上的图就是发给上游的图。
type PlanSource interface {
	// SessionPlan 读会话场景；营销会话再读 scene_input 与最近一张已批准的规划卡。通用会话不读卡片。
	SessionPlan(ctx context.Context, sessionID int64) (SessionPlan, error)
	// CharacterMedia 把会话角色槽里的引用换成要发给上游的图片 mediaId（资产取它的图；媒体原样），
	// 按工作区校验归属。资产还没有图时返回错误（不悄悄少发一张参考图）。
	CharacterMedia(ctx context.Context, ws int64, ref SceneRef) (int64, error)
}

// CreativeRatioSource（可选：PlanSource 的实现同时实现它时生效）给出创意的推荐画幅。
//
// 取**当前版本**的推荐 —— 与「营销输入」里写给模型的「推荐：… · 9:16 · …」是同一份（推荐信息本来就
// 取当前版本，见 wiring.AgentSceneContext）。没有推荐、创意已下线时返回 ""（由偏好 / 节点默认兜底）。
type CreativeRatioSource interface {
	CreativeRatio(ctx context.Context, code string) (string, error)
}
