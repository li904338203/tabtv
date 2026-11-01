package wiring

// 对外 MCP 委托站内 Agent（MCPT-17）的端口适配器：Principal → agent 的 SessionActor，调 agent/app.Service 的
// CreateSession / PostMessage / SubmitCard / Get / EstimateTurn，只做类型转换。
//
// ⚠️ 不碰 agent 的表（MCP 侧的硬约束）：状态机、发起人校验、额度、规划确认的重新报价、计费全在站内服务里。
// ⚠️ 新会话的偏好取用户在站内「设为默认」的那一份（agent/app.Service.UserPrefs）：网页的输入框建会话时带的
// 也是它；不带的话推理模型回落到平台默认（按会员等级报得出价的第一个），视频 / 图片模型是「智能匹配」
// （系统提示词列出可选模型，由推理模型挑；没有平台默认的视频 / 图片模型，04 文档 P-64），
// 同一个用户从 MCP 发起的会话与网页上的表现就不一样了。

import (
	"bytes"
	"context"
	"encoding/json"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
)

// MCPAgentSessions 实现 mcpapp.AgentSessions。
type MCPAgentSessions struct{ Svc *agentapp.Service }

func agentActor(p mcpapp.Principal) agentapp.SessionActor {
	return agentapp.SessionActor{WorkspaceID: p.WorkspaceID, PayerAccountID: p.PayerAccountID, UserID: p.UserID,
		Role: p.Role, MemberLevelID: p.MemberLevelID}
}

func agentWrite(w agentapp.SessionWrite) mcpapp.AgentWrite {
	return mcpapp.AgentWrite{SessionID: w.Session.ID, ProjectID: w.Session.ProjectID, State: string(w.Session.State),
		TurnID: w.TurnID, Queued: w.Queued, Replayed: w.Replayed}
}

// userPrefs 取用户级偏好；没设过（`{}`）或取不到时返回 nil（站内按平台默认）。
func (a MCPAgentSessions) userPrefs(ctx context.Context, userID int64) json.RawMessage {
	p, err := a.Svc.UserPrefs(ctx, userID)
	if err != nil || len(bytes.TrimSpace(p)) == 0 || bytes.Equal(bytes.TrimSpace(p), []byte("{}")) {
		return nil
	}
	return p
}

func (a MCPAgentSessions) Start(ctx context.Context, p mcpapp.Principal, in mcpapp.AgentStartInput) (
	mcpapp.AgentWrite, error) {
	w, err := a.Svc.CreateSession(ctx, agentActor(p), agentapp.CreateSessionInput{
		IdempotencyKey: in.IdempotencyKey, ProjectID: in.ProjectID, CreateProjectName: in.CreateProjectName,
		Text: in.Text, SkillCode: in.SkillCode, Prefs: a.userPrefs(ctx, p.UserID),
		// ⚠️ 营销场景不对 MCP 开放（MKD-22）：委托没有槽位与选择器，开放了只会得到一个缺商品图的营销会话。
		Scene: agentdomain.SceneGeneral,
	})
	if err != nil {
		return mcpapp.AgentWrite{}, err
	}
	return agentWrite(w), nil
}

func (a MCPAgentSessions) Send(ctx context.Context, p mcpapp.Principal, sessionID int64, in mcpapp.AgentSendInput) (
	mcpapp.AgentWrite, error) {
	w, err := a.Svc.PostMessage(ctx, agentActor(p), sessionID, agentapp.PostMessageInput{
		IdempotencyKey: in.IdempotencyKey, Text: in.Text, SkillCode: in.SkillCode,
	})
	if err != nil {
		return mcpapp.AgentWrite{}, err
	}
	return agentWrite(w), nil
}

func (a MCPAgentSessions) SubmitCard(ctx context.Context, p mcpapp.Principal, sessionID, cardID int64,
	submission []byte) (mcpapp.AgentWrite, error) {
	r, err := a.Svc.SubmitCard(ctx, agentActor(p), sessionID, cardID, submission)
	if err != nil {
		return mcpapp.AgentWrite{}, err
	}
	return mcpapp.AgentWrite{SessionID: r.Session.ID, ProjectID: r.Session.ProjectID, State: string(r.Session.State),
		TurnID: r.TurnID, CardID: r.CardID, CardStatus: string(r.CardStatus)}, nil
}

func (a MCPAgentSessions) EstimateTurn(ctx context.Context, p mcpapp.Principal, sessionID int64) (
	mcpapp.AgentTurnEstimate, error) {
	var prefs json.RawMessage
	if sessionID == 0 {
		prefs = a.userPrefs(ctx, p.UserID)
	}
	e, err := a.Svc.EstimateTurn(ctx, agentActor(p), sessionID, prefs)
	if err != nil {
		return mcpapp.AgentTurnEstimate{}, err
	}
	return mcpapp.AgentTurnEstimate{ModelCode: e.ModelCode, Credits: e.Credits}, nil
}

// agentMessageRoles 是给外部客户端看的消息：用户 / Agent 的话与系统说明。工具调用的原文（role=tool）与
// 内部事件不给 —— 那是站内 Agent 的工作记录，几 KB 的 JSON 只会撑大结果，且同样是不可信内容。
var agentMessageRoles = map[string]bool{agentrepo.RoleUser: true, agentrepo.RoleAssistant: true,
	agentrepo.RoleSystemNote: true}

func (a MCPAgentSessions) Get(ctx context.Context, p mcpapp.Principal, sessionID int64) (mcpapp.AgentSession, error) {
	d, err := a.Svc.Get(ctx, agentActor(p), sessionID)
	if err != nil {
		return mcpapp.AgentSession{}, err
	}
	s := d.Session
	out := mcpapp.AgentSession{ID: s.ID, ProjectID: s.ProjectID, Title: s.Title, State: string(s.State),
		CreatedByUserID: s.CreatedByUserID, InitiatorName: d.InitiatorName, Todo: s.Todo,
		Approved: d.Budget.Approved, Used: d.Budget.Used, Refunded: d.Budget.Refunded}
	for _, c := range d.PendingCards {
		out.PendingCards = append(out.PendingCards, mcpapp.AgentCard{ID: c.ID, Kind: string(c.Kind),
			Status: string(c.Status), Payload: c.Payload, CreatedAt: c.CreatedAt})
	}
	if t := d.LatestTurn; t != nil {
		out.LatestTurn = &mcpapp.AgentTurn{ID: t.ID, Trigger: string(t.Trigger), Status: string(t.Status),
			ModelCode: t.ModelCode, Credits: t.CreditsCost, ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage}
	}
	for _, m := range d.Messages.Messages {
		if !agentMessageRoles[m.Role] || m.Content == "" {
			continue
		}
		out.Messages = append(out.Messages, mcpapp.AgentMessage{ID: m.ID, Role: m.Role, Content: m.Content,
			CardID: m.CardID, CreatedAt: m.CreatedAt})
	}
	return out, nil
}

var _ mcpapp.AgentSessions = MCPAgentSessions{}
