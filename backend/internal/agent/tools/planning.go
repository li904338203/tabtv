package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 规划阶段的免费工具（AGT-23）：读画布、查素材、报价、选择卡 / 问答卡、读写文档、
// 规划确认卡、加载技能。共用的依赖与小工具放在这里，各工具一个文件。
//
// ⚠️ 外部内容（素材名、文档正文、技能参考）回给模型时一律视为不可信 —— 它们改变不了
// 工具白名单与额度，因为两者都在服务端判定（AllowedTools / PaidCall），不读模型的话。

// MaxContentRunes 是单个工具回给模型的内容上限（按 Unicode 码点）。
const MaxContentRunes = 8000

// TruncatedNote 是内容被截断时附带的说明。
const TruncatedNote = "已截断，可用 nodeIds 精确读取"

// Store 是规划工具用到的 agent 表访问（*repo.Repo 直接满足；单测用内存假实现）。
//
// ⚠️ 卡片走 Pool() 而不是开事务：发卡与会话迁移本来就分两步（Runner 在工具返回后 CAS 迁移，
// AGT-22 T-4），这里多一个事务也合不进那一步。
type Store interface {
	GetByID(ctx context.Context, id int64) (repo.Session, error)
	UpdateSkills(ctx context.Context, id int64, skillCodes json.RawMessage) error
	GetSkillByCode(ctx context.Context, code string) (repo.Skill, error)
	SkillReferenceByName(ctx context.Context, skillID int64, name string) (repo.SkillReference, error)
	InsertCard(ctx context.Context, q repo.Querier, c repo.Card) (repo.Card, error)
	VoidCard(ctx context.Context, q repo.Querier, id int64) (bool, error)
	PendingBySession(ctx context.Context, sessionID int64) ([]repo.Card, error)
	// AppendScenePins 营销会话规划时新钉的选项并进 scene_input.pins（MKT-T-20）。
	AppendScenePins(ctx context.Context, sessionID int64, pins json.RawMessage) error
	Pool() repo.Querier
}

var _ Store = (*repo.Repo)(nil)

// PlanningDeps 是规划工具的依赖。Events 可为 nil（不推卡片事件，前端靠拉取对账）。
type PlanningDeps struct {
	Store       Store
	Canvas      app.Canvas
	Generations app.NodeGenerations
	Assets      app.Assets
	Pricer      app.PlanPricer
	// Ops 模型的可用生成方式（目录）：计划项的生成方式收不下所带的输入时，在报价之前换成收得下的。
	// nil = 不换（照模型写的报价）。
	Ops       app.OperationCatalog
	Balances  app.Balances
	Documents app.Documents
	Skills    app.SkillRenderer
	// Scene 营销会话规划（MKT-T-20）：选图上限、选项钉版本、口播提示。nil = 未接入，
	// 营销会话的 submit_plan 报错（不退回通用规划：那样卡片上没有条目键与选图，执行时对不上）。
	Scene app.PlanScene
	// MaxVariants 营销会话单次变体上限（SEEAITV_MARKETING_MAX_VARIANTS，MKT-T-27）；≤ 0 按缺省 4。
	MaxVariants int
	Events      app.Events
	NextID      func() int64
	Now         func() time.Time
	Logger      *slog.Logger
}

// PlanningTools 返回九个免费工具（注册顺序与 domain 规格表一致）。
func PlanningTools(d PlanningDeps) []Tool {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return []Tool{
		ReadCanvas{Canvas: d.Canvas, Generations: d.Generations},
		ListAssets{Assets: d.Assets},
		Quote{Pricer: d.Pricer, Ops: d.Ops},
		AskUser{d: d},
		ReadDoc{Documents: d.Documents},
		WriteDoc{d: d},
		SubmitPlan{d: d},
		LoadSkill{d: d},
		ReadSkillRef{d: d},
	}
}

// ptDecode 严格解码参数：未知字段报错（键名写错时让模型改，而不是被当成「没给」）。
// allowUnknown=true 时忽略未知字段（submit_plan 的计划项：模型常顺手带上金额，要忽略而不是拒）。
func ptDecode(args json.RawMessage, v any, allowUnknown bool) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	if !allowUnknown {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("参数格式错误：%v", err)
	}
	return nil
}

// ptJSON 序列化（工具内部结构，不会失败；失败时给一个可读的错误对象）。
func ptJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"error":"序列化失败"}`)
	}
	return b
}

// ptCutRunes 把字符串截到 n 个码点，返回是否截断。
func ptCutRunes(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	return string([]rune(s)[:n]), true
}

func ptRunes(b []byte) int { return utf8.RuneCount(b) }

// ptPublishCard 推 agent.card（只是对账信号，失败只记日志）。
// ⚠️ OccurredAt 由发布方填（Runner.publish 只给 state / delta / step 填）。
func ptPublishCard(ctx context.Context, d PlanningDeps, projectID, sessionID, cardID int64,
	kind domain.CardKind, status domain.CardStatus) {
	if d.Events == nil {
		return
	}
	ev := &app.AgentCardEvent{Type: app.EventAgentCard, OccurredAt: d.Now().UTC().Format(time.RFC3339Nano),
		SessionID: fmt.Sprint(sessionID), CardID: fmt.Sprint(cardID), Kind: string(kind), Status: string(status)}
	if err := d.Events.Publish(ctx, app.ProjectTopic(projectID), ev); err != nil {
		d.Logger.Debug("agent.card 事件发布失败（只是对账信号）", "cardId", cardID, "error", err)
	}
}

// ptTrim 去首尾空白。
func ptTrim(s string) string { return strings.TrimSpace(s) }
