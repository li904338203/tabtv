package app

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 三级连接管理与两级开关（MCPT-14，MCP-F-72/73/74、MCP-N-06/09）。
//
//	用户       —— 自己的全部连接（访问密钥 + OAuth），撤销
//	工作区管理员 —— 本工作区所有成员的连接，撤销；关闭 / 打开本工作区 MCP
//	平台管理员   —— 全站连接、撤销、平台总开关、统计
//
// 三个入口**只差权限与范围**：撤销都走 RevokeGrant，范围条件写进同一条 SQL（repo.GrantScope）。

// 审计动作名。
const (
	AuditPlatformSwitch  = "mcp.platform.switch"
	AuditWorkspaceSwitch = "mcp.workspace.switch"
	auditObjectSetting   = "mcp_setting"
	auditObjectWorkspace = "workspace"
)

// 统计范围：缺省近 7 天，最多 30 天（= mcp_call_log 的保留期）。
const (
	DefaultStatsSpan = 7 * 24 * time.Hour
	MaxStatsSpan     = 30 * 24 * time.Hour
	// StatsTopClients 是客户端分布的名额，其余并进「其他」（各项之和仍等于总数）。
	StatsTopClients = 20
	maxReasonLen    = 255
)

// Workspaces 是工作区级开关与成员名（协作域实现，wiring.MCPWorkspaces）。
// ⚠️ 开关列在 workspace 表上，但 MCP 不直接读那张表（表归谁，查询就归谁）。
type Workspaces interface {
	MCPDisabled(ctx context.Context, workspaceID int64) (bool, error)
	// SetMCPDisabled 返回写之前的值（审计的 before）。
	SetMCPDisabled(ctx context.Context, workspaceID int64, disabled bool) (bool, error)
	// MemberNames 是本工作区 active 成员的 userID → 昵称（工作区连接列表显示「谁连的」）。
	MemberNames(ctx context.Context, workspaceID int64) (map[int64]string, error)
}

// ManageStore 是本文件要用的 MCP 自有表（mcp/repo.Repo 实现）。
type ManageStore interface {
	ListGrantsBy(ctx context.Context, f repo.GrantFilter) ([]repo.GrantRow, error)
	RevokeGrantIn(ctx context.Context, s repo.GrantScope, grantID int64) (repo.Grant, int64, error)
	GetPlatformSetting(ctx context.Context) (repo.PlatformSetting, error)
	SavePlatformSetting(ctx context.Context, enabled bool, adminID int64) (repo.PlatformSetting, repo.PlatformSetting, error)
	GetStats(ctx context.Context, f repo.StatsFilter) (repo.Stats, error)
}

// Manage 是连接管理与开关服务。
type Manage struct {
	store ManageStore
	ws    Workspaces
	audit Auditor
	log   *slog.Logger
	now   func() time.Time
	// envEnabled 是 SEEAITV_MCP_ENABLED（管理端要如实显示「环境变量关着，这里开了也没用」）。
	envEnabled bool
}

// NewManage 构造。audit 可为 nil（仅测试）。
func NewManage(store ManageStore, ws Workspaces, audit Auditor, envEnabled bool, log *slog.Logger) *Manage {
	if log == nil {
		log = slog.Default()
	}
	return &Manage{store: store, ws: ws, audit: audit, log: log, now: time.Now, envEnabled: envEnabled}
}

// EnvEnabled 是 SEEAITV_MCP_ENABLED。
func (m *Manage) EnvEnabled() bool { return m.envEnabled }

// ---------------------------------------------------------------- 列表与撤销

// RevokerKind 是撤销的发起方。
type RevokerKind string

const (
	RevokerUser      RevokerKind = "user"
	RevokerWorkspace RevokerKind = "workspace_admin"
	RevokerAdmin     RevokerKind = "platform_admin"
)

// Revoker 是撤销的发起方与它的范围。
type Revoker struct {
	Kind RevokerKind
	// ActorID：用户撤销 / 工作区管理员撤销是用户 ID，平台撤销是管理员 ID。
	ActorID int64
	// WorkspaceID 只对工作区管理员有意义（他只能撤销本工作区的连接）。
	WorkspaceID int64
}

func (b Revoker) scope() (repo.GrantScope, error) {
	switch b.Kind {
	case RevokerUser:
		return repo.GrantScope{UserID: b.ActorID}, nil
	case RevokerWorkspace:
		if b.WorkspaceID <= 0 {
			return repo.GrantScope{}, errors.New("工作区撤销缺少 workspaceID")
		}
		return repo.GrantScope{WorkspaceID: b.WorkspaceID}, nil
	case RevokerAdmin:
		return repo.GrantScope{}, nil
	}
	return repo.GrantScope{}, errors.New("未知的撤销发起方 " + string(b.Kind))
}

// ListGrants 按范围列连接。范围由调用方（transport）按入口固定，筛选条件只能在范围之内收窄 ——
// 工作区入口的 WorkspaceID 来自 X-Workspace-Id 的成员资格校验，不从查询参数收。
func (m *Manage) ListGrants(ctx context.Context, f repo.GrantFilter) ([]repo.GrantRow, error) {
	rows, err := m.store.ListGrantsBy(ctx, f)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return rows, nil
}

// MemberNames 取工作区成员昵称（失败只告警、返回空表：名字只是展示，不该让列表打不开）。
func (m *Manage) MemberNames(ctx context.Context, workspaceID int64) map[int64]string {
	if m.ws == nil {
		return map[int64]string{}
	}
	names, err := m.ws.MemberNames(ctx, workspaceID)
	if err != nil {
		m.log.Warn("取工作区成员昵称失败", "workspaceId", workspaceID, "error", err)
		return map[int64]string{}
	}
	return names
}

// RevokeGrant 是撤销的**唯一**入口（用户 / 工作区管理员 / 平台管理员只差范围）：
// grant 置 revoked + 同事务作废其 OAuth 令牌（repo.RevokeGrantIn）+ 写平台审计。
// 该凭据下一次调用即 401 TOKEN_REVOKED（凭据链每次实时查 grant 状态）。
//
// ⚠️ 平台管理员撤销**理由必填**：撤销的是用户自己授予外部客户端的访问，事后用户问「我的连接
// 怎么断了」时，审计里要答得上来。不在范围内与不存在回同一个 404。
func (m *Manage) RevokeGrant(ctx context.Context, by Revoker, grantID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if by.Kind == RevokerAdmin && reason == "" {
		return httpx.Errf(errcode.InvalidArgument, "撤销理由必填：它会进审计，用户问起「连接为什么断了」时要答得上来")
	}
	if utf8.RuneCountInString(reason) > maxReasonLen {
		return httpx.Errf(errcode.InvalidArgument, "撤销理由不能超过 255 个字符")
	}
	scope, err := by.scope()
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	before, tokens, err := m.store.RevokeGrantIn(ctx, scope, grantID)
	if err != nil {
		return mapRepoErr(err, "连接")
	}
	if m.audit != nil {
		actor := audit.ActorUser
		if by.Kind == RevokerAdmin {
			actor = audit.ActorAdmin
		}
		m.audit.Write(ctx, audit.Entry{ActorType: actor, ActorID: by.ActorID, WorkspaceID: before.WorkspaceID,
			Action: AuditGrantRevoke, ObjectType: auditObjectGrant, ObjectID: before.ID,
			Before: grantAudit(before),
			After: map[string]any{"status": repo.GrantRevoked, "revokedBy": string(by.Kind),
				"userId": before.UserID, "tokensRevoked": tokens},
			Reason: reason})
	}
	return nil
}

// ---------------------------------------------------------------- 开关

// PlatformSetting 读平台开关。读失败**按开启**并告警：总开关是止损手段，
// 一次数据库抖动不该让全体 MCP 用户掉线（MCPT-14 风险项的取向）。
func (m *Manage) PlatformSetting(ctx context.Context) (repo.PlatformSetting, error) {
	s, err := m.store.GetPlatformSetting(ctx)
	if err != nil {
		return repo.PlatformSetting{}, httpx.Wrap(errcode.Internal, err)
	}
	return s, nil
}

// SetPlatform 改平台开关（管理端，mcp:write）。关闭不撤销任何 grant，重新打开即恢复。
func (m *Manage) SetPlatform(ctx context.Context, adminID int64, enabled bool, reason string) (repo.PlatformSetting, error) {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > maxReasonLen {
		return repo.PlatformSetting{}, httpx.Errf(errcode.InvalidArgument, "理由不能超过 255 个字符")
	}
	before, after, err := m.store.SavePlatformSetting(ctx, enabled, adminID)
	if err != nil {
		return repo.PlatformSetting{}, httpx.Wrap(errcode.Internal, err)
	}
	if m.audit != nil {
		m.audit.Write(ctx, audit.Entry{ActorType: audit.ActorAdmin, ActorID: adminID,
			Action: AuditPlatformSwitch, ObjectType: auditObjectSetting, ObjectID: 1,
			Before: map[string]any{"enabled": before.Enabled, "version": before.Version},
			After:  map[string]any{"enabled": after.Enabled, "version": after.Version}, Reason: reason})
	}
	return after, nil
}

// WorkspaceSetting 是工作区页要显示的两个开关：本工作区开没开、平台开没开
// （平台关着时工作区开着也没用，要如实说出来，不然管理员会以为是自己没打开）。
type WorkspaceSetting struct {
	Enabled         bool
	PlatformEnabled bool
}

// WorkspaceSetting 读本工作区的开关。
func (m *Manage) WorkspaceSetting(ctx context.Context, workspaceID int64) (WorkspaceSetting, error) {
	disabled, err := m.ws.MCPDisabled(ctx, workspaceID)
	if err != nil {
		return WorkspaceSetting{}, err
	}
	return WorkspaceSetting{Enabled: !disabled, PlatformEnabled: m.platformOpen(ctx)}, nil
}

// SetWorkspace 改本工作区开关（路由闸已要求 manage）。关闭不撤销 grant，站内功能不受影响。
func (m *Manage) SetWorkspace(ctx context.Context, userID, workspaceID int64, enabled bool) (WorkspaceSetting, error) {
	before, err := m.ws.SetMCPDisabled(ctx, workspaceID, !enabled)
	if err != nil {
		return WorkspaceSetting{}, err
	}
	if m.audit != nil {
		m.audit.Write(ctx, audit.Entry{ActorType: audit.ActorUser, ActorID: userID, WorkspaceID: workspaceID,
			Action: AuditWorkspaceSwitch, ObjectType: auditObjectWorkspace, ObjectID: workspaceID,
			Before: map[string]any{"mcpEnabled": !before}, After: map[string]any{"mcpEnabled": enabled}})
	}
	return WorkspaceSetting{Enabled: enabled, PlatformEnabled: m.platformOpen(ctx)}, nil
}

// platformOpen 读平台开关，读失败按开启并告警。
func (m *Manage) platformOpen(ctx context.Context) bool {
	s, err := m.store.GetPlatformSetting(ctx)
	if err != nil {
		m.log.Warn("读 MCP 平台开关失败，按开启处理", "error", err)
		return true
	}
	return s.Enabled
}

// Switch 是 /mcp 请求链上的开关判定（transport.SwitchChecker）。
//
// ⚠️ **每次请求都读库、不缓存**：撤销延迟为 0 —— 关掉之后的下一次调用即被拒（与凭据链
// 每次实时查 grant / 成员角色同一取向，MT-4）。两条都是主键查询，代价与凭据解析的其他几条同量级。
// 读失败一律**按开启**并告警（理由见 PlatformSetting）；工作区那条读失败同样放行 ——
// 走到这一步时成员资格刚刚实时校验过，「库读不动」的话前面那一步本就会失败。
type Switch struct{ m *Manage }

// Switch 返回开关判定器。
func (m *Manage) Switch() Switch { return Switch{m} }

// PlatformOpen 平台层判定（环境变量关着时路由根本没挂，这里只看运行时开关）。
func (s Switch) PlatformOpen(ctx context.Context) bool {
	return domain.SwitchDecision(domain.SwitchState{EnvEnabled: s.m.envEnabled,
		PlatformEnabled: s.m.platformOpen(ctx), WorkspaceEnabled: true}) == domain.SwitchAllow
}

// Check 是凭据之后的完整判定（平台 + 工作区）。返回 nil 放行；否则是 404 或 403 MCP_DISABLED。
func (s Switch) Check(ctx context.Context, workspaceID int64) error {
	wsOpen := true
	if s.m.ws != nil {
		d, err := s.m.ws.MCPDisabled(ctx, workspaceID)
		if err != nil {
			s.m.log.Warn("读工作区 MCP 开关失败，按开启处理", "workspaceId", workspaceID, "error", err)
		} else {
			wsOpen = !d
		}
	}
	switch domain.SwitchDecision(domain.SwitchState{EnvEnabled: s.m.envEnabled,
		PlatformEnabled: s.m.platformOpen(ctx), WorkspaceEnabled: wsOpen}) {
	case domain.SwitchNotFound:
		return PlatformDisabled()
	case domain.SwitchWorkspaceDisabled:
		return httpx.Errf(errcode.McpDisabled, "工作区管理员已关闭本工作区的 MCP：外部客户端暂时不能访问这个工作区，"+
			"站内功能不受影响；请联系工作区管理员在「账号中心 → MCP 访问」重新打开").
			WithDetails(map[string]any{"reason": "workspace_disabled", "workspaceId": idString(workspaceID)})
	}
	return nil
}

// PlatformDisabled 是平台总开关关着时的错误：404（关掉就是不存在）。
func PlatformDisabled() error {
	return httpx.Errf(errcode.NotFound, "MCP 服务未开放（平台已暂停 MCP）")
}

// ---------------------------------------------------------------- 统计

// StatsInput 是统计入参：范围 + 与列表同一套筛选。
type StatsInput struct {
	Filter repo.GrantFilter
	From   *time.Time
	To     *time.Time
}

// StatsResult 是统计结果（比率 -1 = 暂无数据）。
type StatsResult struct {
	From, To     time.Time
	Raw          repo.Stats
	ErrorRate    float64
	ConfirmRate  float64
	ByClient     []domain.ClientShare
	MaxSpanDays  int
	SpanAdjusted bool
}

// Stats 算统计（MCP-N-09）。范围**校正而不是报错**：缺省近 7 天、倒置交换、超过 30 天夹到 30 天
// （日志只留 30 天，更长的范围只会让数字看起来像「这段时间几乎没人用」），并回**实际用的**范围。
func (m *Manage) Stats(ctx context.Context, in StatsInput) (StatsResult, error) {
	from, to, adjusted := statsRange(m.now(), in.From, in.To)
	f := in.Filter
	f.CursorID, f.Limit = 0, 0
	raw, err := m.store.GetStats(ctx, repo.StatsFilter{GrantFilter: f, From: from, To: to})
	if err != nil {
		return StatsResult{}, httpx.Wrap(errcode.Internal, err)
	}
	shares := make([]domain.ClientShare, 0, len(raw.ByClient))
	for _, c := range raw.ByClient {
		shares = append(shares, domain.ClientShare{Name: c.ClientName, Calls: c.Calls, Errors: c.Errors})
	}
	return StatsResult{
		From: from, To: to, Raw: raw,
		ErrorRate:   domain.Ratio(raw.Errors, raw.Calls),
		ConfirmRate: domain.Ratio(raw.QuotesUsed, raw.QuotesIssued),
		ByClient:    domain.TopClients(shares, StatsTopClients),
		MaxSpanDays: int(MaxStatsSpan / (24 * time.Hour)), SpanAdjusted: adjusted,
	}, nil
}

// statsRange 校正统计范围。
func statsRange(now time.Time, from, to *time.Time) (time.Time, time.Time, bool) {
	adjusted := false
	t := now
	if to != nil {
		t = *to
	}
	f := t.Add(-DefaultStatsSpan)
	if from != nil {
		f = *from
	}
	if f.After(t) {
		f, t = t, f
		adjusted = true
	}
	if t.Sub(f) > MaxStatsSpan {
		f = t.Add(-MaxStatsSpan)
		adjusted = true
	}
	return f, t, adjusted
}

func idString(v int64) string { return strconv.FormatInt(v, 10) }
