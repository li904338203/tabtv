package wiring

// 画布 Agent 的端口适配器（AGT-22）。端口定义在 agent/app（谁定义端口谁拥有契约），
// 这里把各域的服务接上去。Worker 的轮次运行器与 API 侧服务（AGT-26）共用同一份，
// 不在两个 main 里各写一套。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingdomain "github.com/li904338203/tabtv/backend/internal/billing/domain"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasdomain "github.com/li904338203/tabtv/backend/internal/canvas/domain"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/usererr"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 推理模型

// AgentChatModel 把 Provider 域的 Chat 接成 agent 的 ChatModel 端口（类型逐字段转换）。
type AgentChatModel struct{ P *providerapp.HTTPProvider }

func (a AgentChatModel) Chat(ctx context.Context, in agentapp.ChatInput,
	onDelta func(string)) (agentapp.ChatOutput, error) {
	msgs := make([]protocol.ChatMessage, 0, len(in.Messages))
	for _, m := range in.Messages {
		pm := protocol.ChatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Images: m.Images}
		for _, c := range m.ToolCalls {
			pm.ToolCalls = append(pm.ToolCalls, protocol.ToolCall{ID: c.ID, Name: c.Name,
				Arguments: c.Arguments, RawArguments: c.RawArguments})
		}
		msgs = append(msgs, pm)
	}
	tools := make([]protocol.ToolDef, 0, len(in.Tools))
	for _, t := range in.Tools {
		tools = append(tools, protocol.ToolDef{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	out, err := a.P.Chat(ctx, providerapp.ChatInput{ModelCode: in.ModelCode, System: in.System,
		Messages: msgs, Tools: tools, Options: in.Options, MaxTokensDefault: in.MaxOutputTokens}, onDelta)
	res := agentapp.ChatOutput{Text: out.Text, FinishReason: out.FinishReason,
		InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}
	for _, c := range out.ToolCalls {
		res.ToolCalls = append(res.ToolCalls, agentapp.ToolCall{ID: c.ID, Name: c.Name,
			Arguments: c.Arguments, RawArguments: c.RawArguments})
	}
	// ⚠️ 推理模型的报错会写进轮次状态、推给前端：上游原话（报文、内部地址）只留在请求日志里，
	// 给用户的是归类后的说法。errors.As 拿 *ChatError 仍然成立（Unwrap 保留错误链）。
	return res, usererr.Wrap(err)
}

// ---------------------------------------------------------------- 报价、账本、会员等级

// AgentQuoter 把目录域的报价接成 agent 的 Quoter 端口。
// AgentVideoModels 把模型目录的视频能力（AGT-05）交给运行器写进系统提示词。
type AgentVideoModels struct{ Catalog *catalogapp.Service }

func (a AgentVideoModels) VideoModel(ctx context.Context, code string) (agentapp.VideoModelInfo, error) {
	t, err := a.Catalog.VideoTraits(ctx, code)
	if err != nil {
		return agentapp.VideoModelInfo{}, err
	}
	return agentapp.VideoModelInfo{IsVideo: t.OK, MaxDurationSec: t.MaxDurationSec,
		MultiShot: t.IsMultiShot, NativeAudio: t.HasNativeAudio}, nil
}

// DefaultReasoningModel 取用户端推理模型列表（与 /models?agentCapable=true 同一来源）里
// 按这个会员等级报得出价的第一个（04 文档 P-13）；都报不出价时退回第一个（报价会说明原因）。
func (a AgentVideoModels) DefaultReasoningModel(ctx context.Context, memberLevelID int64) (string, error) {
	list, err := a.Catalog.UsableModels(ctx, "", true)
	if err != nil || len(list) == 0 {
		return "", err
	}
	q := AgentQuoter{a.Catalog}
	for _, m := range list {
		if _, err := q.QuoteTurn(ctx, m.Code, memberLevelID); err == nil {
			return m.Code, nil
		}
	}
	return list[0].Code, nil
}

// AgentModelDirectory 列出某产出类型下此刻可用的模型（与 /models 同一来源），并带上每个模型能用的生成方式。
//
// ⚠️ 会员门槛与报价同一条判断（QuoteWithFacts 的 MemberLevelRequired）：列出来的模型要真能报出价，
// 列一个等级不够的模型给推理模型，它照着规划，卡片上就是一项「需要更高会员等级」。
type AgentModelDirectory struct{ Catalog *catalogapp.Service }

func (a AgentModelDirectory) UsableModels(ctx context.Context, output string, memberLevelID int64) (
	[]agentapp.ModelBrief, error) {
	list, err := a.Catalog.UsableModels(ctx, output, false)
	if err != nil {
		return nil, err
	}
	var out []agentapp.ModelBrief
	for _, m := range list {
		if m.MinMemberLevelID > 0 && memberLevelID < m.MinMemberLevelID {
			continue
		}
		fits, err := a.Catalog.ModelOperationFits(ctx, m.Code)
		if err != nil {
			continue
		}
		var ops []string
		for _, f := range fits {
			if f.Output == output && len(f.Caps) > 0 {
				ops = append(ops, f.Code)
			}
		}
		if len(ops) == 0 {
			continue
		}
		out = append(out, agentapp.ModelBrief{Code: m.Code, Name: m.Name, Operations: ops})
	}
	return out, nil
}

type AgentQuoter struct{ Catalog *catalogapp.Service }

func (a AgentQuoter) QuoteTurn(ctx context.Context, modelCode string, level int64) (agentapp.Quote, error) {
	d, err := a.Catalog.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: modelCode, OperationCode: providerapp.OpAgentChat, Count: 1, MemberLevelID: level,
	})
	if err != nil {
		return agentapp.Quote{}, err
	}
	return agentapp.Quote{Credits: d.Credits, ModelID: d.ModelID}, nil
}

func (a AgentQuoter) QuoteGeneration(ctx context.Context, in agentapp.QuoteInput) (agentapp.Quote, error) {
	d, err := a.Catalog.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode, Count: 1, Text: in.Text,
		MemberLevelID: in.MemberLevelID, RoleCounts: toRoleCount(in.RoleCount),
	})
	if err != nil {
		return agentapp.Quote{}, err
	}
	return agentapp.Quote{Credits: d.Credits, ModelID: d.ModelID}, nil
}

// AgentLedger 把账本接成 agent 的 Ledger 端口（同 scriptLedger 的翻译规则，但翻成 agent 的哨兵错误）。
type AgentLedger struct {
	Repo       *billingrepo.Repo
	Compensate time.Duration
}

func (l AgentLedger) Consume(ctx context.Context, payerID, userID int64, amount int,
	refType, refID, reason string) error {
	_, err := l.Repo.Consume(ctx, billingrepo.ConsumeInput{
		PayerID: payerID, Amount: amount, RefType: refType, RefID: refID, UserID: userID, Reason: reason,
	})
	switch {
	case errors.Is(err, billingdomain.ErrInsufficient):
		return fmt.Errorf("%w: %v", agentapp.ErrInsufficientCredits, err)
	case errors.Is(err, billingrepo.ErrDuplicate):
		// 同一轮已经扣过（消息重投）：由 Runner 视为已扣。
		return agentapp.ErrLedgerDuplicate
	}
	return err
}

func (l AgentLedger) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := l.Repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: l.Compensate,
	})
	// 幂等命中不是错误：退款的一次性由账本唯一索引保证。
	if errors.Is(err, billingrepo.ErrDuplicate) {
		return nil
	}
	// 找不到原消费流水（当时没扣成）：翻成 agent 的哨兵，恢复扫描据此判「没什么可退」。
	if errors.Is(err, billingrepo.ErrNotFound) {
		return errors.Join(agentapp.ErrLedgerNotFound, err)
	}
	return err
}

// AgentMemberLevels 用 billing 的同一份实现取会员等级（Worker 没有 *http.Request）。
type AgentMemberLevels struct{ Billing *billingapp.Service }

func (a AgentMemberLevels) MemberLevelID(ctx context.Context, payerID int64) int64 {
	return a.Billing.MemberLevelID(ctx, payerID)
}

// ---------------------------------------------------------------- 成员资格、事件

// AgentMembers 实时查发起人在工作区的角色。不是 active 成员（或工作区已归档）→ ok=false。
type AgentMembers struct{ Repo *collabrepo.Repo }

func (a AgentMembers) Role(ctx context.Context, ws, userID int64) (collab.Role, bool, error) {
	role, err := a.Repo.Membership(ctx, ws, userID)
	if errors.Is(err, collabrepo.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

// BusPublisher 是事件总线发布方的那一个方法（platform/eventbus.Publisher 满足它）。
type BusPublisher interface {
	Publish(ctx context.Context, topic string, payload []byte) error
}

// AgentEvents 把结构化事件序列化后发到事件总线。
type AgentEvents struct{ Bus BusPublisher }

func (a AgentEvents) Publish(ctx context.Context, topic string, payload any) error {
	if a.Bus == nil {
		return nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return a.Bus.Publish(ctx, topic, b)
}

// ---------------------------------------------------------------- 画布与文档

// AgentCanvas 把画布域接成 agent 的 Canvas 端口。
//
// ⚠️ 读取用 viewer 角色调 canvas：会话发起人的实时角色校验由 Runner 在每次工具前做，
// 这里只是「按会话所属工作区取项目」。
type AgentCanvas struct{ Svc *canvasapp.Service }

func (a AgentCanvas) Read(ctx context.Context, ws, projectID int64) (agentapp.CanvasView, error) {
	p, err := a.Svc.Get(ctx, ws, projectID, collab.RoleViewer)
	if err != nil {
		return agentapp.CanvasView{}, err
	}
	nodes, edges := agentapp.SummarizeCanvas(p.CanvasData)
	return agentapp.CanvasView{Version: p.Version, CanvasData: p.CanvasData, Nodes: nodes, EdgeCount: edges}, nil
}

func (a AgentCanvas) Apply(ctx context.Context, in agentapp.ApplyOpsInput) (agentapp.ApplyOpsResult, error) {
	var ops []canvasdomain.Op
	if err := json.Unmarshal(in.Ops, &ops); err != nil {
		return agentapp.ApplyOpsResult{}, httpx.Errf(errcode.InvalidArgument, "画布操作不是合法的数组: "+err.Error())
	}
	actor := canvasdomain.Actor{Kind: canvasdomain.ActorAgent, UserID: in.UserID, SessionID: in.AgentSessionID}
	r, err := a.Svc.ApplyOps(ctx, canvasapp.ApplyOpsInput{WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID,
		Actor: actor, BatchID: in.BatchID, Ops: ops})
	if errors.Is(err, canvasdomain.ErrNotOwned) {
		// 翻成 agent 的哨兵：工具据此让模型在旁边新建（AGT-24），agent 不 import canvas 的类型。
		return agentapp.ApplyOpsResult{}, errors.Join(agentapp.ErrNodeNotOwned, err)
	}
	if err != nil {
		return agentapp.ApplyOpsResult{}, err
	}
	return agentapp.ApplyOpsResult{FromVersion: r.FromVersion, ToVersion: r.ToVersion, NodeIDs: r.NodeIDs,
		Replayed: r.Replayed}, nil
}

// AgentDocuments 把项目文档（canvas 域，AGT-09）接成 agent 的 Documents 端口。
type AgentDocuments struct{ Svc *canvasapp.Service }

func (a AgentDocuments) Get(ctx context.Context, ws, projectID int64, role collab.Role,
	path string) (agentapp.Document, error) {
	d, err := a.Svc.DocumentByPath(ctx, ws, projectID, role, path)
	if err != nil {
		var be *httpx.BizError
		if errors.As(err, &be) && be.Code == errcode.NotFound {
			return agentapp.Document{}, fmt.Errorf("%w: %s", agentapp.ErrDocumentNotFound, path)
		}
		return agentapp.Document{}, err
	}
	return agentapp.Document{ID: d.ID, Path: d.Path, Content: d.Content, Version: d.Version}, nil
}

// Put Version=0 新建，否则按版本乐观锁更新（冲突原样返回 canvas 的 40902，带「是谁改的」）。
func (a AgentDocuments) Put(ctx context.Context, in agentapp.PutDocumentInput) (agentapp.Document, error) {
	actor := canvasapp.DocumentActor{UserID: in.UserID, AgentSessionID: in.AgentSessionID}
	if in.Version == 0 {
		d, err := a.Svc.CreateDocument(ctx, in.WorkspaceID, in.ProjectID, actor, in.Role,
			canvasapp.CreateDocumentInput{Path: in.Path, Content: in.Content})
		if err != nil {
			return agentapp.Document{}, err
		}
		return agentapp.Document{ID: d.ID, Path: d.Path, Content: d.Content, Version: d.Version}, nil
	}
	cur, err := a.Svc.DocumentByPath(ctx, in.WorkspaceID, in.ProjectID, in.Role, in.Path)
	if err != nil {
		return agentapp.Document{}, err
	}
	d, err := a.Svc.UpdateDocument(ctx, in.WorkspaceID, in.ProjectID, cur.ID, actor, in.Role, in.Version, in.Content)
	if err != nil {
		return agentapp.Document{}, err
	}
	return agentapp.Document{ID: d.ID, Path: d.Path, Content: d.Content, Version: d.Version}, nil
}

// ---------------------------------------------------------------- 运行器装配

// AgentRunnerInput 是 Worker 装配轮次运行器要的东西。
type AgentRunnerInput struct {
	AgentRepo    *agentrepo.Repo
	Chat         *providerapp.HTTPProvider
	Catalog      *catalogapp.Service
	Billing      *billingrepo.Repo
	BillingSvc   *billingapp.Service
	Collab       *collabrepo.Repo
	Canvas       *canvasapp.Service
	Bus          BusPublisher
	Tools        agentapp.ToolSet
	Compensate   time.Duration
	NextID       func() int64
	Logger       *slog.Logger
	MaxCalls     int
	ContextChars int
	// MaxOutputTokens 推理模型单次输出上限的缺省值（04 文档 P-09）；0 = 协议缺省。
	MaxOutputTokens int
	// Scene 营销会话的场景段与营销输入（MKT-T-18）；nil = 营销会话只写场景标题并告警。
	Scene agentapp.SceneContext
	// Images 营销会话给推理模型看商品图（MKT-T-19）；nil = 不附图。
	Images agentapp.SceneImages
}

// NewAgentRunner 装出 Worker 侧的轮次运行器。
func NewAgentRunner(in AgentRunnerInput) *agentapp.Runner {
	d := agentapp.RunnerDeps{
		Store:        agentapp.NewRepoStore(in.AgentRepo),
		Chat:         AgentChatModel{in.Chat},
		Quoter:       AgentQuoter{in.Catalog},
		Ledger:       AgentLedger{Repo: in.Billing, Compensate: in.Compensate},
		Members:      AgentMembers{in.Collab},
		MemberLevels: AgentMemberLevels{in.BillingSvc},
		Events:       AgentEvents{in.Bus},
		Documents:    AgentDocuments{in.Canvas},
		Canvas:       AgentCanvas{in.Canvas},
		Tools:        in.Tools,
		SkillRender:  AgentSkillRenderer{},
		VideoModels:  AgentVideoModels{in.Catalog},
		Ops:          AgentOperationCatalog{Catalog: in.Catalog},
		Models:       AgentModelDirectory{Catalog: in.Catalog},
		Reasoning:    AgentVideoModels{in.Catalog},
		Scene:        in.Scene,
		Images:       in.Images,
		NextID:       in.NextID,
		Logger:       in.Logger,
	}
	return agentapp.NewRunner(d, agentapp.RunnerConfig{MaxCalls: in.MaxCalls, ContextChars: in.ContextChars,
		MaxOutputTokens: in.MaxOutputTokens})
}

// 编译期核对：适配器实现了 agent 的端口。
var (
	_ agentapp.ChatModel         = AgentChatModel{}
	_ agentapp.Quoter            = AgentQuoter{}
	_ agentapp.Ledger            = AgentLedger{}
	_ agentapp.MemberLevels      = AgentMemberLevels{}
	_ agentapp.Members           = AgentMembers{}
	_ agentapp.Events            = AgentEvents{}
	_ agentapp.Canvas            = AgentCanvas{}
	_ agentapp.VideoModels       = AgentVideoModels{}
	_ agentapp.ReasoningDefaults = AgentVideoModels{}
	_ agentapp.Documents         = AgentDocuments{}
)
