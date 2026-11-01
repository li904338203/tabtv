package app

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// API 侧服务（service.go，AGT-26）额外需要的端口。
//
// 与 ports.go（AGT-22）分开放：那份是 Worker 侧运行器的端口，这里只有 API 进程用得到的
// 「建项目 / 校验引用 / 批准时重新报价 / 每轮对账」。实现都在 internal/wiring 的
// agent_api.go —— agent 不 import 其他域的 repo / app 包（dev/01 T-1）。

// ProjectNode 是画布里一个节点的摘要（引用解析用）。
type ProjectNode struct {
	ID    string
	Type  string
	Title string
}

// APIProjects 是项目能力（画布域实现）。
type APIProjects interface {
	// EnsureProject 按客户端幂等键建空项目；同一个键已建过则返回那一个。
	// origin 是项目来源（MKD-3）：营销会话传 "marketing"，其余传 ""；只在新建时写入。
	EnsureProject(ctx context.Context, ws, userID int64, role collab.Role, name, idemKey, origin string) (int64, error)
	// ProjectNodes 校验项目属于本工作区且未删除，返回画布节点摘要。
	ProjectNodes(ctx context.Context, ws, projectID int64) (map[string]ProjectNode, error)
	// DocVersion 取项目文档当前版本；found=false 表示文档不存在（已删除或从未建过）。
	DocVersion(ctx context.Context, ws, projectID int64, path string) (version int, found bool, err error)
}

// RefLookup 按工作区校验引用归属（GO-27：只认 ID，不认客户端给的地址）。
// 不存在与跨工作区一律返回 404 类错误。
type RefLookup interface {
	// MediaKind 返回媒体大类（image / video / audio / other）。
	MediaKind(ctx context.Context, ws, mediaID int64) (string, error)
	// Asset 返回资产名与类型（character / scene / prop …）。
	Asset(ctx context.Context, ws, assetID int64) (name, assetType string, err error)
}

// PlanQuoter 按规划确认卡上的计划项**逐项重新报价**（与 submit_plan 同一条路径，AGT-23）。
//
// ⚠️ 批准是「花钱授权」，不信卡片上（更不信前端带来）的金额，以服务端此刻的报价为准。
// 返回的计划项 Credits 已换成新报价；任一项不可报价（模型下线、会员等级不够）原样返回该错误。
type PlanQuoter interface {
	QuotePlan(ctx context.Context, items []domain.PlanItem, memberLevelID int64) ([]domain.PlanItem, error)
}

// LedgerRef 是账本流水的业务引用。
type LedgerRef struct {
	Type string
	ID   string
}

// LedgerEntry 是一条账本流水（只读投影）。Change 带符号：消费为负、退款为正。
type LedgerEntry struct {
	ID        int64
	Type      string // consume | refund | grant | expire | adjust
	Change    int
	RefType   string
	RefID     string
	Reason    string
	CreatedAt time.Time
}

// Accounts 是账本的只读端口：余额与按引用取流水（billing 实现）。
type Accounts interface {
	Balance(ctx context.Context, payerID int64) (int, error)
	TransactionsByRefs(ctx context.Context, payerID int64, refs []LedgerRef) ([]LedgerEntry, error)
}

// TaskControl 是生成任务的查询与取消（停止时列可取消项、取消会话时逐个取消）。
// ports.go 的 Generator 是它的超集，Worker 侧的适配器同样满足它。
type TaskControl interface {
	Status(ctx context.Context, ws, generationID int64) (TaskStatus, error)
	Cancel(ctx context.Context, ws, generationID int64, reason string) error
}

// ServicePorts 是 Service 的全部依赖端口。Events 可为 nil（不推事件，前端靠轮询对账）。
type ServicePorts struct {
	Projects APIProjects
	Refs     RefLookup
	Plans    PlanQuoter
	Accounts Accounts
	Tasks    TaskControl
	Events   Events
	// Optimize 一键优化提示词（AGT-43）；nil = 未接入（接口回 500「推理模型链路未接入」）。
	Optimize *OptimizePorts
	// SkillModels 技能列表下发绑定模型的显示名（MKT-D-13）；nil = 只给 code。
	SkillModels SkillModels
	// Scene 营销场景的选项钉版本与槽位校验（MKT-T-17）；nil = 营销场景未开放
	// （SEEAITV_MARKETING_ENABLED=false），scene=marketing 的建会话请求一律 400。
	Scene SceneResolver
	// MaxVariants 单次批量变体上限（SEEAITV_MARKETING_MAX_VARIANTS，MKT-T-27）；≤ 0 按缺省 4。
	// 批准规划时兜底校验（不看营销总开关：已经发出的卡片同样受限）。
	MaxVariants int
	// Library 建营销会话时取商品 / 角色快照（MKT-T-37）；nil = 未接入，带商品库 / 角色库引用的
	// 建会话请求回 500「营销库未接入」。
	Library SceneLibrary
	// SlotDecls 用户端技能列表下发前把输入槽声明换成新形状（MKD-26）；nil = 原样下发。
	SlotDecls SlotDeclNormalizer
}

// ScenePickRef 是用户提交的一次选择：只有维度 + code（版本由服务端在受理那一刻取当前版钉住）。
type ScenePickRef struct {
	Kind string // creative / hook / style
	Code string
}

// ScenePin 是钉住的一个选项：(code, version) + 该版本的名称（只给展示，不含片段原文）。
type ScenePin struct {
	Kind    string
	Code    string
	Version int
	Name    string
}

// SceneSlotFill 是一次发送里各槽位的实际填充（数量与类型；归属已由 RefLookup 校验过）。
type SceneSlotFill struct {
	// Refs：槽位 → 每一项的输入类型（image / video / character_asset …），按槽内顺序。
	Refs map[string][]string
	// Fields：槽位 → 字段 → 值（目前只有商品槽的 name / sellingPoints）。
	Fields map[string]map[string]string
}

// SlotIssue 是一条槽位问题。Slot 对应接口错误的 `details.slot`（前端据此挂到对应的槽上）。
type SlotIssue struct {
	Slot    string
	Field   string
	Code    string
	Message string
}

// SceneResolver 是营销场景建会话要问营销域的事（实现在 wiring，适配 marketing/app.Resolve 与
// marketing/domain.ValidateSlotFill）。⚠️ agent 不 import marketing：选项可见性与槽位规则全仓各只有
// 一份实现（MKD-13、MKD-12），在这里再写一遍迟早两边不一致。
type SceneResolver interface {
	// Resolve 校验并钉住用户此刻的选择（存在 ∧ 在投 ∧ kind 对得上）；失败原样返回 42215。
	Resolve(ctx context.Context, picks []ScenePickRef, now time.Time) ([]ScenePin, error)
	// ValidateSlotFill 按技能的输入槽声明（原文；空 = 场景默认声明）校验填充，一次报完。
	ValidateSlotFill(decl json.RawMessage, fill SceneSlotFill) []SlotIssue
}

// SceneLibrary 是建营销会话时向营销域取商品 / 角色快照的端口（MKT-T-37；dev/05 MKD-24、25）。
// agent 定义、wiring 适配营销域：agent 不 import marketing。
//
// productID ≤ 0 表示不取商品；角色按 characterIDs 的顺序返回。不存在 / 他人工作区 / 已删除一律回
// *SceneLibraryMissing（调用方据此点名槽位），其余错误原样返回。
type SceneLibrary interface {
	Snapshot(ctx context.Context, ws, productID int64, characterIDs []int64) (*SceneProductSnap,
		[]SceneCharacterSnap, error)
}

// SceneProductSnap 是商品在建会话那一刻的内容（冻结进 scene_input）。
type SceneProductSnap struct {
	ID            int64
	Version       int
	Name          string
	SellingPoints string
	Description   string
	// ImageMediaIDs 有序，[0] 为主图。
	ImageMediaIDs []int64
}

// SceneCharacterSnap 是角色库角色在建会话那一刻的内容。
type SceneCharacterSnap struct {
	ID          int64
	Name        string
	MainMediaID int64
	Appearance  string
	VoiceText   string
	// VoiceMediaID 参考音频（0 = 没有）：只存进快照，不发给任何模型（MKD-33）。
	VoiceMediaID int64
}

// SceneLibraryMissing 是快照里取不到的那一项。Slot = product / character。
type SceneLibraryMissing struct {
	Slot string
	ID   int64
}

func (e *SceneLibraryMissing) Error() string {
	return "营销库引用不存在：" + e.Slot + " " + strconv.FormatInt(e.ID, 10)
}
