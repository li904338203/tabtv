package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// API 侧服务（dev/01 §9，AGT-26）：建会话、发消息、提交卡片、停止、取消、查看、每轮对账。
//
// ⚠️ 只做「收消息 → 写库 → 写 outbox」（T-2）：一轮的实际运行在 Worker。
// 事务内只写 agent 自己的表与 outbox；报价、余额、项目、引用这些跨域查询都在事务**之前**做
// （事务内禁止远程调用，§5.3）。批准规划时在事务里锁住会话行与卡片行后**再比一次合计**。
// ⚠️ 状态迁移只经 domain.CanTransition / MessageEvent / SubmissionEvent 选出，这里不写特例。

const (
	// MaxMessageRunes 用户一条消息的上限（按码点）。
	MaxMessageRunes = 8000
	// MaxRefs 一条消息最多引用多少个节点 / 素材 / 资产。
	MaxRefs = 20
	// MaxTitleRunes 会话标题上限（与 agent_session.title varchar(100) 一致）。
	MaxTitleRunes = 100
	// MaxPrefsBytes 会话级 / 用户级偏好 JSON 的上限：偏好只是几个模型码与参数。
	MaxPrefsBytes = 16 * 1024
	// MaxIdemKeyLen 与 agent_session.create_idempotency_key / agent_turn.idempotency_key 的列宽一致。
	MaxIdemKeyLen = 128
	// RefTypeGeneration 是生成费在账本里的 refType（generation/app/accept.go）。
	RefTypeGeneration = "generation"
	// recentInDetail 会话详情里带出的最近消息条数。
	recentInDetail = 20
)

// 引用类型。
const (
	RefNode  = "node"
	RefMedia = "media"
	RefAsset = "asset"
)

// SessionActor 是一次 API 调用的调用者（由 transport 从工作区上下文与会员解析器装出）。
type SessionActor struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	Role           collab.Role
	MemberLevelID  int64
}

// RefInput 是客户端给的一条引用：只有类型与 ID（GO-27：不接收 URL）。
//
// Slot / Order 只用于建营销会话（MKT-T-17）：引用填在哪个槽（product / character / source_video）
// 与槽内顺序（1 起；商品槽 #1 为主图）。通用会话带了一律 400，不静默忽略。
type RefInput struct {
	Kind  string
	ID    string
	Slot  string
	Order int
}

// ResolvedRef 是服务端校验归属后存进 agent_message.refs 的引用（模型只拿这份摘要）。
type ResolvedRef struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	NodeType  string `json:"nodeType,omitempty"`
	MediaKind string `json:"mediaKind,omitempty"`
	AssetType string `json:"assetType,omitempty"`
	// Slot / Order 只在营销会话的首条消息上有（MKT-T-17）；Order 已规范成槽内 1..N。
	Slot  string `json:"slot,omitempty"`
	Order int    `json:"order,omitempty"`
}

// Service 是 API 侧服务。
type Service struct {
	repo   *repo.Repo
	nextID func() int64
	p      ServicePorts
	log    *slog.Logger
	now    func() time.Time
}

// NewService 构造 API 侧服务。
func NewService(r *repo.Repo, nextID func() int64, p ServicePorts, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: r, nextID: nextID, p: p, log: log, now: time.Now}
}

// ---------------------------------------------------------------- 通用

func agentErr(err error) error {
	if err == nil {
		return nil
	}
	var be *httpx.BizError
	if errors.As(err, &be) {
		return err
	}
	if errors.Is(err, repo.ErrNotFound) {
		return httpx.Err(errcode.AgentSessionNotFound)
	}
	return httpx.Wrap(errcode.Internal, err)
}

func validateIdemKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return httpx.Err(errcode.MissingIdempotencyKey)
	}
	if len(key) > MaxIdemKeyLen {
		return httpx.Errf(errcode.InvalidArgument, "Idempotency-Key 不能超过 "+strconv.Itoa(MaxIdemKeyLen)+" 字符")
	}
	return nil
}

func validateText(text string, refs int) (string, error) {
	t := strings.TrimSpace(text)
	if t == "" && refs == 0 {
		return "", httpx.Errf(errcode.InvalidArgument, "消息内容不能为空")
	}
	if utf8.RuneCountInString(t) > MaxMessageRunes {
		return "", httpx.Errf(errcode.InvalidArgument, "消息不能超过 "+strconv.Itoa(MaxMessageRunes)+" 字")
	}
	return t, nil
}

func validatePrefs(prefs json.RawMessage) (json.RawMessage, error) {
	if len(prefs) == 0 || string(prefs) == "null" {
		return nil, nil
	}
	if len(prefs) > MaxPrefsBytes {
		return nil, httpx.Errf(errcode.InvalidArgument, "偏好设置过大")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(prefs, &obj); err != nil || obj == nil {
		return nil, httpx.Errf(errcode.InvalidArgument, "prefs 必须是 JSON 对象")
	}
	return prefs, nil
}

// load 按工作区取会话（跨工作区与不存在回同一个 404）。
func (s *Service) load(ctx context.Context, a SessionActor, id int64) (repo.Session, error) {
	sess, err := s.repo.Get(ctx, a.WorkspaceID, id)
	return sess, agentErr(err)
}

// requireInitiator：同工作区成员可以看，只有发起人能继续对话 / 提交卡片（design/03 §8）。
func requireInitiator(a SessionActor, sess repo.Session) error {
	if sess.CreatedByUserID != a.UserID {
		return httpx.Err(errcode.AgentNotInitiator)
	}
	return nil
}

// requireController：停止 / 取消 / 改标题 —— 发起人，或者工作区管理员（会话可能挂在
// 一个已离开的人名下，管理员要能把它停掉）。
func requireController(a SessionActor, sess repo.Session) error {
	if sess.CreatedByUserID == a.UserID || a.Role.Can(collab.ActionManage) {
		return nil
	}
	return httpx.Err(errcode.AgentNotInitiator)
}

func notCancelled(st domain.State) error {
	if st == domain.StateCancelled {
		return httpx.Err(errcode.AgentSessionCancelled)
	}
	return nil
}

func (s *Service) skillCodes(ctx context.Context, code string, scene domain.Scene) (json.RawMessage, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, nil
	}
	sk, err := s.repo.GetSkillByCode(ctx, code)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && !sk.IsEnabled) {
		return nil, httpx.Errf(errcode.InvalidArgument, "技能不存在或已停用："+code)
	}
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	// 场景隔离（04 文档 P-22②）：营销技能的正文假定了营销会话才有的输入与工具，通用会话用它的话
	// 第一步就会调一个它没有的工具；反之营销会话里换成通用技能会丢掉营销的规则。
	if !domain.SkillInScene(sk.SceneList(), domain.SceneKey(scene)) {
		return nil, httpx.Errf(errcode.InvalidArgument, "技能「"+sk.Name+"」不适用于当前会话的场景")
	}
	b, _ := json.Marshal([]string{sk.Code})
	return b, nil
}

// mergeSkillCodes 把这次带的技能追加进会话已加载的技能（04 文档 P-22①）：去重，最多 maxSessionSkills 个。
//
// ⚠️ 原先是**覆盖**：营销会话里在画布面板选一个技能发消息，营销技能（正文、口播标签、白名单）就整组没了；
// 而模型自己 load_skill 是追加。两条路语义不同，同一个会话里先后两种操作的结果取决于顺序。
func mergeSkillCodes(current, add json.RawMessage) (json.RawMessage, error) {
	var have, more []string
	if len(current) > 0 {
		_ = json.Unmarshal(current, &have)
	}
	if err := json.Unmarshal(add, &more); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(have)+len(more))
	for _, c := range append(have, more...) {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) > maxSessionSkills {
		return nil, httpx.Errf(errcode.InvalidArgument, "一个会话最多加载 "+strconv.Itoa(maxSessionSkills)+" 个技能")
	}
	return json.Marshal(out)
}

// maxSessionSkills 与 load_skill 的上限一致。
const maxSessionSkills = 8

// resolveRefs 按工作区校验引用归属，产出存库的摘要（GO-27）。
//
// 节点要在本项目画布里；媒体、资产要属于本工作区。任一条不通过整条消息拒绝 ——
// 悄悄丢掉一条引用的话，用户以为 Agent 看到了那张图，而它其实没有。
func (s *Service) resolveRefs(ctx context.Context, a SessionActor, nodes map[string]ProjectNode,
	in []RefInput) (json.RawMessage, error) {
	if len(in) > MaxRefs {
		return nil, httpx.Errf(errcode.InvalidArgument, "一条消息最多引用 "+strconv.Itoa(MaxRefs)+" 项")
	}
	out, err := s.resolveRefList(ctx, a, nodes, in)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return b, nil
}

// resolveRefList 是 resolveRefs 的校验主体（不判条数上限：营销会话的商品槽按配置上限另判）。
// Slot / Order 原样带出；同一槽里重复的同一个引用只留一条。
func (s *Service) resolveRefList(ctx context.Context, a SessionActor, nodes map[string]ProjectNode,
	in []RefInput) ([]ResolvedRef, error) {
	out := make([]ResolvedRef, 0, len(in))
	seen := map[string]bool{}
	for i, r := range in {
		where := "第 " + strconv.Itoa(i+1) + " 个引用"
		id := strings.TrimSpace(r.ID)
		if id == "" {
			return nil, httpx.Errf(errcode.InvalidArgument, where+"缺少 id")
		}
		// 键里带槽位：同一张图同时填进两个槽是两条引用，按旧键去重会悄悄丢掉一条。
		key := r.Slot + "|" + r.Kind + ":" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		switch r.Kind {
		case RefNode:
			n, ok := nodes[id]
			if !ok {
				return nil, slotRefErr(r.Slot, httpx.Errf(errcode.InvalidArgument, where+"：画布上没有节点 "+id))
			}
			out = append(out, ResolvedRef{Kind: RefNode, ID: id, Label: n.Title, NodeType: n.Type,
				Slot: r.Slot, Order: r.Order})
		case RefMedia, RefAsset:
			n, err := strconv.ParseInt(id, 10, 64)
			if err != nil || n <= 0 {
				return nil, httpx.Errf(errcode.InvalidArgument, where+"的 id 非法")
			}
			if s.p.Refs == nil {
				return nil, httpx.Errf(errcode.Internal, "引用解析未接入")
			}
			if r.Kind == RefMedia {
				kind, err := s.p.Refs.MediaKind(ctx, a.WorkspaceID, n)
				if err != nil {
					return nil, slotRefErr(r.Slot, refErr(where, "素材", err))
				}
				out = append(out, ResolvedRef{Kind: RefMedia, ID: id, MediaKind: kind, Slot: r.Slot, Order: r.Order})
			} else {
				name, typ, err := s.p.Refs.Asset(ctx, a.WorkspaceID, n)
				if err != nil {
					return nil, slotRefErr(r.Slot, refErr(where, "资产", err))
				}
				out = append(out, ResolvedRef{Kind: RefAsset, ID: id, Label: name, AssetType: typ,
					Slot: r.Slot, Order: r.Order})
			}
		default:
			return nil, httpx.Errf(errcode.InvalidArgument, where+"的类型只能是 node / media / asset")
		}
	}
	return out, nil
}

func refErr(where, what string, err error) error {
	var be *httpx.BizError
	if errors.As(err, &be) && be.Code == errcode.NotFound {
		return httpx.Errf(errcode.InvalidArgument, where+"："+what+"不存在或不属于当前工作区")
	}
	return err
}

// slotRefErr 给营销槽位里的引用错误点名槽位（details.slot，MKT-T-28）：前端据此把那个槽标红 ——
// 「第 3 个引用不属于当前工作区」用户对不上是哪一张。通用会话（slot 为空）与非 400 错误原样返回。
func slotRefErr(slot string, err error) error {
	var be *httpx.BizError
	if slot == "" || !errors.As(err, &be) || be.Code != errcode.InvalidArgument {
		return err
	}
	return httpx.Errf(errcode.InvalidArgument, be.Message).
		WithDetails(map[string]any{"slot": slot, "field": slot, "code": "not_found"})
}

func titleFrom(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if t == "" {
		return "新对话"
	}
	r := []rune(t)
	if len(r) > 20 {
		return string(r[:20])
	}
	return t
}

// publish 发事件。⚠️ 事件只是对账信号：失败只告警。
func (s *Service) publish(ctx context.Context, projectID int64, ev any) {
	if s.p.Events == nil {
		return
	}
	switch e := ev.(type) {
	case *AgentStateEvent:
		e.OccurredAt = nowStr(s.now())
	case *AgentCardEvent:
		e.OccurredAt = nowStr(s.now())
	}
	if err := s.p.Events.Publish(ctx, ProjectTopic(projectID), ev); err != nil {
		s.log.WarnContext(ctx, "Agent 事件发布失败（前端靠 HTTP 对账）", "error", err)
	}
}

func (s *Service) publishState(ctx context.Context, sess repo.Session, turnID int64, reason string) {
	ev := &AgentStateEvent{Type: EventAgentState, SessionID: idStr(sess.ID), State: string(sess.State),
		ApprovedBudget: sess.ApprovedBudget, Reason: reason}
	if turnID != 0 {
		ev.TurnID = idStr(turnID)
	}
	if b, err := s.budget(ctx, sess.ID); err == nil {
		ev.Spent = b.Used
	}
	if cards, err := s.repo.PendingBySession(ctx, sess.ID); err == nil && len(cards) > 0 {
		ev.PendingCardID = idStr(cards[len(cards)-1].ID)
	}
	s.publish(ctx, sess.ProjectID, ev)
}

func (s *Service) publishCard(ctx context.Context, projectID int64, c repo.Card, status domain.CardStatus) {
	s.publish(ctx, projectID, &AgentCardEvent{Type: EventAgentCard, SessionID: idStr(c.SessionID),
		CardID: idStr(c.ID), Kind: string(c.Kind), Status: string(status)})
}

// BudgetSummary 是会话额度的汇总（来自 agent_budget_entry）。
type BudgetSummary struct {
	Approved int
	// Used = reserved + charged 的报价合计（额度占用口径，与 CheckBudget 一致）。
	Used int
	// Refunded = 失败已退款而归还额度的报价合计。
	Refunded int
}

func (s *Service) budget(ctx context.Context, sessionID int64) (BudgetSummary, error) {
	rows, err := s.repo.BudgetBySession(ctx, sessionID)
	if err != nil {
		return BudgetSummary{}, err
	}
	var b BudgetSummary
	for _, e := range rows {
		// 与 UsedInTx / RefundedInTx 同一口径：charged 的部分退回（组部分成功，AGT-50）
		// 占用里减掉、退回里加上。
		b.Used += e.Occupied()
		b.Refunded += e.Returned()
	}
	return b, nil
}

// ---------------------------------------------------------------- 建会话

// CreateSessionInput 是建会话的入参。
type CreateSessionInput struct {
	IdempotencyKey string
	// ProjectID 与 CreateProjectName 二选一：后者非 nil 表示顺带建一个新项目（创造页）。
	ProjectID         int64
	CreateProjectName *string
	Title             string
	Text              string
	Refs              []RefInput
	SkillCode         string
	Prefs             json.RawMessage
	// Scene 会话场景；空 = general（通用会话的请求体不带它，行为逐字不变）。建后不可改。
	Scene domain.Scene
	// Marketing 营销输入（只在 scene=marketing 时有意义；通用会话带了一律 400）。
	Marketing *MarketingInput
}

// SessionWrite 是写接口的返回：会话当前快照 + 本次建的轮次 / 消息。
type SessionWrite struct {
	Session   repo.Session
	TurnID    int64
	MessageID int64
	// Queued：会话正在运行，这条消息排队为下一轮（不迁移状态）。
	Queued bool
	// Replayed：同一个幂等键已经处理过，返回的是那一次的结果。
	Replayed bool
}

// CreateSession 建会话并写首条用户消息、建第一轮、写 outbox（dev/01 §9）。
//
// ⚠️ 幂等键存 agent_session.create_idempotency_key：同一个键第二次请求（包括并发的两次）
// 只回已建的那个会话，不报错（同 uk_generation_idem）。
// ⚠️ 顺带建项目时项目与会话不在同一个事务（项目归画布域）：项目也按同一个键幂等，
// 会话那一步失败后客户端带同一个键重试，拿回的是同一个项目。
func (s *Service) CreateSession(ctx context.Context, a SessionActor, in CreateSessionInput) (SessionWrite, error) {
	if err := validateIdemKey(in.IdempotencyKey); err != nil {
		return SessionWrite{}, err
	}
	if existing, err := s.repo.GetByCreateKey(ctx, a.WorkspaceID, in.IdempotencyKey); err == nil {
		return s.replayCreate(ctx, existing), nil
	} else if !errors.Is(err, repo.ErrNotFound) {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}
	scene, ok := domain.ParseScene(string(in.Scene))
	if !ok {
		return SessionWrite{}, httpx.Errf(errcode.InvalidArgument, "scene 只能是 general / marketing").
			WithDetails(map[string]any{"field": "scene"})
	}
	if scene != domain.SceneMarketing {
		if err := rejectMarketingFields(in); err != nil {
			return SessionWrite{}, err
		}
	}
	text, err := validateText(in.Text, len(in.Refs))
	if err != nil {
		return SessionWrite{}, err
	}
	prefs, err := validatePrefs(in.Prefs)
	if err != nil {
		return SessionWrite{}, err
	}
	// 营销分支：全部校验在建项目之前（失败不留一个空项目）、更在扣第一轮费用之前（MKT-T-17）。
	var mk *marketingPrep
	if scene == domain.SceneMarketing {
		if mk, err = s.prepareMarketing(ctx, a, in); err != nil {
			return SessionWrite{}, err
		}
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		if text == "" && mk != nil && mk.input.Product.Name != "" {
			title = titleFrom(mk.input.Product.Name)
		} else {
			title = titleFrom(text)
		}
	}
	if utf8.RuneCountInString(title) > MaxTitleRunes {
		return SessionWrite{}, httpx.Errf(errcode.InvalidArgument, "标题不能超过 100 字")
	}
	var skills json.RawMessage
	if mk != nil {
		skills = mk.skills
	} else if skills, err = s.skillCodes(ctx, in.SkillCode, domain.SceneGeneral); err != nil {
		return SessionWrite{}, err
	}
	if s.p.Projects == nil {
		return SessionWrite{}, httpx.Errf(errcode.Internal, "项目端口未接入")
	}

	projectID := in.ProjectID
	origin := ""
	if mk != nil {
		origin = ProjectOriginMarketing
	}
	if in.CreateProjectName != nil {
		projectID, err = s.p.Projects.EnsureProject(ctx, a.WorkspaceID, a.UserID, a.Role,
			*in.CreateProjectName, in.IdempotencyKey, origin)
		if err != nil {
			return SessionWrite{}, err
		}
	} else if projectID <= 0 {
		return SessionWrite{}, httpx.Errf(errcode.InvalidArgument, "请指定 projectId 或 createProject")
	}
	var refs, sceneInput json.RawMessage
	if mk != nil {
		// 营销会话的引用已在 prepareMarketing 里按工作区解析过（新项目画布为空，节点引用在那里就被拒）。
		refs, sceneInput = mk.refs, mk.sceneInput
	} else {
		nodes, err := s.p.Projects.ProjectNodes(ctx, a.WorkspaceID, projectID)
		if err != nil {
			return SessionWrite{}, err
		}
		if refs, err = s.resolveRefs(ctx, a, nodes, in.Refs); err != nil {
			return SessionWrite{}, err
		}
	}
	route, err := domain.MessageEvent(domain.StateIdle, domain.MessageContext{})
	if err != nil {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}

	tx, err := s.repo.Begin(ctx)
	if err != nil {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sess, err := s.repo.Create(ctx, tx, repo.Session{
		ID: s.nextID(), WorkspaceID: a.WorkspaceID, ProjectID: projectID,
		PayerAccountID: a.PayerAccountID, CreatedByUserID: a.UserID, Title: title,
		State: route.To, SkillCodes: skills, Prefs: prefs, CreateIdempotencyKey: in.IdempotencyKey,
		Scene: scene, SceneInput: sceneInput,
	})
	if errors.Is(err, repo.ErrDuplicate) {
		_ = tx.Rollback(ctx)
		existing, e2 := s.repo.GetByCreateKey(ctx, a.WorkspaceID, in.IdempotencyKey)
		if e2 != nil {
			return SessionWrite{}, httpx.Errf(errcode.Conflict, "并发创建冲突，请重试")
		}
		return s.replayCreate(ctx, existing), nil
	}
	if err != nil {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}
	turnID, msgID, err := s.writeUserTurn(ctx, tx, sess.ID, text, refs, in.IdempotencyKey, 0)
	if err != nil {
		return SessionWrite{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}
	s.publishState(ctx, sess, turnID, "")
	return SessionWrite{Session: sess, TurnID: turnID, MessageID: msgID}, nil
}

func (s *Service) replayCreate(ctx context.Context, sess repo.Session) SessionWrite {
	out := SessionWrite{Session: sess, Replayed: true}
	if t, err := s.repo.GetTurnByIdempotencyKey(ctx, sess.ID, sess.CreateIdempotencyKey); err == nil {
		out.TurnID = t.ID
	}
	return out
}

// writeUserTurn 在事务里建一轮（trigger=user）、写用户消息、写 outbox。
// cardID 非 0 时消息挂在那张卡片上（提交卡片生成的「具体内容」消息）。
func (s *Service) writeUserTurn(ctx context.Context, tx pgx.Tx, sessionID int64, text string,
	refs json.RawMessage, idemKey string, cardID int64) (turnID, messageID int64, err error) {
	turn, err := s.repo.InsertTurn(ctx, tx, repo.Turn{
		ID: s.nextID(), SessionID: sessionID, Trigger: domain.TriggerUser, IdempotencyKey: idemKey,
	})
	if err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			return 0, 0, err
		}
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}
	msg, err := s.repo.Append(ctx, tx, repo.Message{
		ID: s.nextID(), SessionID: sessionID, TurnID: turn.ID, Role: repo.RoleUser,
		Content: text, Refs: refs, CardID: cardID,
	})
	if err != nil {
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}
	if err := s.repo.EnqueueTurnInTx(ctx, tx, s.nextID(), sessionID, turn.ID); err != nil {
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}
	return turn.ID, msg.ID, nil
}

// ---------------------------------------------------------------- 发消息

// PostMessageInput 是发消息的入参。
type PostMessageInput struct {
	IdempotencyKey string
	Text           string
	Refs           []RefInput
	SkillCode      string
	Prefs          json.RawMessage
}

// PostMessage 发消息：校验 → 按当前状态选迁移 → 写消息 + 建轮 + outbox（dev/01 §9）。
//
// 运行中（planning / executing）也可以发：不迁移状态，排队为下一轮。
// 挂起时直接发消息的去向由 domain.MessageEvent 决定（规划确认卡作废回规划，等）。
func (s *Service) PostMessage(ctx context.Context, a SessionActor, sessionID int64,
	in PostMessageInput) (SessionWrite, error) {
	if err := validateIdemKey(in.IdempotencyKey); err != nil {
		return SessionWrite{}, err
	}
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return SessionWrite{}, err
	}
	if err := requireInitiator(a, sess); err != nil {
		return SessionWrite{}, err
	}
	if t, err := s.repo.GetTurnByIdempotencyKey(ctx, sess.ID, in.IdempotencyKey); err == nil {
		return SessionWrite{Session: sess, TurnID: t.ID, Replayed: true}, nil
	}
	if err := notCancelled(sess.State); err != nil {
		return SessionWrite{}, err
	}
	for i, r := range in.Refs {
		if r.Slot != "" || r.Order != 0 {
			return SessionWrite{}, httpx.Errf(errcode.InvalidArgument,
				"第 "+strconv.Itoa(i+1)+" 个引用：slot / order 只用于建营销会话")
		}
	}
	text, err := validateText(in.Text, len(in.Refs))
	if err != nil {
		return SessionWrite{}, err
	}
	prefs, err := validatePrefs(in.Prefs)
	if err != nil {
		return SessionWrite{}, err
	}
	skills, err := s.skillCodes(ctx, in.SkillCode, sess.Scene)
	if err != nil {
		return SessionWrite{}, err
	}
	nodes := map[string]ProjectNode{}
	if hasNodeRef(in.Refs) {
		if nodes, err = s.p.Projects.ProjectNodes(ctx, a.WorkspaceID, sess.ProjectID); err != nil {
			return SessionWrite{}, err
		}
	}
	refs, err := s.resolveRefs(ctx, a, nodes, in.Refs)
	if err != nil {
		return SessionWrite{}, err
	}
	pending, err := s.repo.PendingBySession(ctx, sess.ID)
	if err != nil {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}

	tx, err := s.repo.Begin(ctx)
	if err != nil {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := s.repo.LockForUpdate(ctx, tx, sess.ID)
	if err != nil {
		return SessionWrite{}, agentErr(err)
	}
	if err := notCancelled(locked.State); err != nil {
		return SessionWrite{}, err
	}
	route, voided, err := s.routeAsMessage(ctx, tx, &locked, pending)
	if err != nil {
		return SessionWrite{}, err
	}
	if len(skills) > 0 {
		merged, err := mergeSkillCodes(locked.SkillCodes, skills)
		if err != nil {
			return SessionWrite{}, err
		}
		if err := s.repo.SetSkillsInTx(ctx, tx, sess.ID, merged); err != nil {
			return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
		}
	}
	if prefs != nil {
		if _, err := s.repo.UpdateSessionMeta(ctx, tx, sess.ID, nil, prefs); err != nil {
			return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
		}
	}
	turnID, msgID, err := s.writeUserTurn(ctx, tx, sess.ID, text, refs, in.IdempotencyKey, 0)
	if errors.Is(err, repo.ErrDuplicate) {
		_ = tx.Rollback(ctx)
		if t, e2 := s.repo.GetTurnByIdempotencyKey(ctx, sess.ID, in.IdempotencyKey); e2 == nil {
			return SessionWrite{Session: sess, TurnID: t.ID, Replayed: true}, nil
		}
		return SessionWrite{}, httpx.Errf(errcode.Conflict, "并发提交冲突，请重试")
	}
	if err != nil {
		return SessionWrite{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SessionWrite{}, httpx.Wrap(errcode.Internal, err)
	}
	if voided != nil {
		s.publishCard(ctx, sess.ProjectID, *voided, domain.CardVoided)
	}
	s.publishState(ctx, locked, turnID, "")
	return SessionWrite{Session: locked, TurnID: turnID, MessageID: msgID, Queued: !route.Transition}, nil
}

// routeAsMessage 按「直接发一条消息」的规则决定会话去向，并在事务里迁移状态、作废被跳过的卡片。
// 返回被作废的卡片（提交后要推送；nil = 没有作废）。
//
// PostMessage 与「提交一张已过时的卡片」共用这一份（04 文档 P-08）。后者原先把挂起的卡写死成
// 追加额度卡：会话停在规划确认时，点一张更早的选择卡就直接进了执行态，规划卡也没作废，
// 绕过了「未批准不得执行」（AG-05）。
//
// locked 必须是本事务里 LockForUpdate 拿到的会话行，迁移后原地更新它的 State。
func (s *Service) routeAsMessage(ctx context.Context, tx pgx.Tx, locked *repo.Session,
	pending []repo.Card) (domain.MessageRoute, *repo.Card, error) {
	used, err := s.repo.UsedInTx(ctx, tx, locked.ID)
	if err != nil {
		return domain.MessageRoute{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	mc := domain.MessageContext{PlanApproved: locked.ApprovedBudget > 0 && used < locked.ApprovedBudget}
	card, hasCard := suspendCard(locked.State, pending)
	if hasCard {
		mc.PendingCard = card.Kind
		if locked.State == domain.StateAwaitingInput {
			if mc.ResumeState, err = domain.ResumeStateOf(card.Domain()); err != nil {
				mc.ResumeState = domain.StatePlanning // 卡片内容损坏：保守回规划（不进执行）
			}
		}
	} else if locked.State == domain.StateAwaitingInput {
		mc.ResumeState = domain.StatePlanning
	}
	route, err := domain.MessageEvent(locked.State, mc)
	if errors.Is(err, domain.ErrSessionCancelled) {
		return domain.MessageRoute{}, nil, httpx.Err(errcode.AgentSessionCancelled)
	}
	if err != nil {
		return domain.MessageRoute{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	if route.Transition {
		ok, err := s.repo.TransitionState(ctx, tx, locked.ID, locked.State, route.To)
		if err != nil {
			return domain.MessageRoute{}, nil, httpx.Wrap(errcode.Internal, err)
		}
		if !ok {
			return domain.MessageRoute{}, nil, httpx.Errf(errcode.Conflict, "会话状态刚刚变化，请重试")
		}
		locked.State = route.To
	}
	if !route.VoidCard || !hasCard {
		return route, nil, nil
	}
	if _, err := s.repo.VoidCard(ctx, tx, card.ID); err != nil {
		return domain.MessageRoute{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	if err := s.repo.ClearPlanVersion(ctx, tx, locked.ID); err != nil {
		return domain.MessageRoute{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	return route, &card, nil
}

func hasNodeRef(in []RefInput) bool {
	for _, r := range in {
		if r.Kind == RefNode {
			return true
		}
	}
	return false
}

// suspendCard 取会话挂起所等的那张卡片（最近发出的、与挂起状态匹配的 pending 卡）。
func suspendCard(st domain.State, pending []repo.Card) (repo.Card, bool) {
	for i := len(pending) - 1; i >= 0; i-- {
		want, ok := domain.SuspendStateFor(pending[i].Kind)
		if ok && want == st {
			return pending[i], true
		}
	}
	return repo.Card{}, false
}

// ---------------------------------------------------------------- 提交卡片

// SubmitResult 是提交卡片的结果。
type SubmitResult struct {
	Session    repo.Session
	CardID     int64
	CardStatus domain.CardStatus
	TurnID     int64
	MessageID  int64
}

// PriceChange 是批准时发现价格变了的新报价（随 409 CONFLICT 的 details 返回）。
type PriceChange struct {
	Items         []domain.PlanItem
	Total         int
	PreviousTotal int
	Balance       int
}

// SubmitCard 提交卡片（dev/01 §9）。规划确认 / 追加额度的 approve / adjust / reject 也走这里。
//
// ⚠️ 卡片 CAS `pending → submitted`：重复提交只有第一次生效，第二次 409 AGENT_CARD_CLOSED、
// 不新建轮次（AG-06）。
// ⚠️ 规划确认卡的 approve 是「花钱授权」：**服务端重新报价**，与卡片合计不一致时把新报价
// 写回卡片并 409（不按新价格批准，前端提示「价格已更新」后由用户再确认）；余额不足 422 带差额；
// 规划文档在确认前被改过则卡片作废、回规划。通过后 approved_budget = 已占用 + 合计（不加缓冲）。
func (s *Service) SubmitCard(ctx context.Context, a SessionActor, sessionID, cardID int64,
	submission json.RawMessage) (SubmitResult, error) {
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return SubmitResult{}, err
	}
	if err := requireInitiator(a, sess); err != nil {
		return SubmitResult{}, err
	}
	if err := notCancelled(sess.State); err != nil {
		return SubmitResult{}, err
	}
	card, err := s.repo.GetCard(ctx, sess.ID, cardID)
	if errors.Is(err, repo.ErrNotFound) {
		return SubmitResult{}, httpx.Errf(errcode.NotFound, "卡片不存在")
	}
	if err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if card.Status != domain.CardPending {
		return SubmitResult{}, cardClosed(card.Status)
	}
	if len(submission) == 0 {
		return SubmitResult{}, httpx.Errf(errcode.InvalidArgument, "缺少 submission")
	}
	if err := domain.ValidateSubmission(card.Domain(), submission); err != nil {
		if errors.Is(err, domain.ErrInvalidSubmission) {
			return SubmitResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	ev, err := domain.SubmissionEvent(card.Domain(), submission)
	if err != nil {
		return SubmitResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	switch card.Kind {
	case domain.CardPlanConfirm:
		return s.submitPlan(ctx, a, sess, card, submission, ev)
	case domain.CardBudget:
		return s.submitBudget(ctx, a, sess, card, submission, ev)
	default:
		return s.submitInput(ctx, a, sess, card, submission, ev)
	}
}

func cardClosed(st domain.CardStatus) error {
	return httpx.Err(errcode.AgentCardClosed).WithDetails(map[string]any{"cardStatus": string(st)})
}

// beginLocked 开事务并锁会话行与卡片行，校验两者仍可提交。
func (s *Service) beginLocked(ctx context.Context, sessionID, cardID int64) (pgx.Tx, repo.Session,
	repo.Card, error) {
	tx, err := s.repo.Begin(ctx)
	if err != nil {
		return nil, repo.Session{}, repo.Card{}, httpx.Wrap(errcode.Internal, err)
	}
	fail := func(e error) (pgx.Tx, repo.Session, repo.Card, error) {
		_ = tx.Rollback(ctx)
		return nil, repo.Session{}, repo.Card{}, e
	}
	sess, err := s.repo.LockForUpdate(ctx, tx, sessionID)
	if err != nil {
		return fail(agentErr(err))
	}
	if err := notCancelled(sess.State); err != nil {
		return fail(err)
	}
	card, err := s.repo.GetCardForUpdate(ctx, tx, sessionID, cardID)
	if err != nil {
		return fail(agentErr(err))
	}
	if card.Status != domain.CardPending {
		return fail(cardClosed(card.Status))
	}
	return tx, sess, card, nil
}

// closeAndMove 在事务里关卡片（CAS）并按事件迁移会话状态。
func (s *Service) closeAndMove(ctx context.Context, tx pgx.Tx, sess *repo.Session, card repo.Card,
	status domain.CardStatus, submission json.RawMessage, userID int64, ev domain.Event) error {
	ok, err := s.repo.CloseCard(ctx, tx, card.ID, status, submission, userID)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if !ok {
		return cardClosed(domain.CardSubmitted)
	}
	to, allowed := domain.CanTransition(sess.State, ev)
	if !allowed {
		return httpx.Errf(errcode.InvalidStateTransition, "会话当前状态（"+string(sess.State)+"）不接受该操作")
	}
	moved, err := s.repo.TransitionState(ctx, tx, sess.ID, sess.State, to)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if !moved {
		return httpx.Errf(errcode.Conflict, "会话状态刚刚变化，请重试")
	}
	sess.State = to
	return nil
}

func cardTurnKey(cardID int64) string { return "card:" + idStr(cardID) }

func (s *Service) finishSubmit(ctx context.Context, tx pgx.Tx, sess repo.Session, card repo.Card,
	status domain.CardStatus, turnID, msgID int64) (SubmitResult, error) {
	if err := tx.Commit(ctx); err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	s.publishCard(ctx, sess.ProjectID, card, status)
	s.publishState(ctx, sess, turnID, "")
	return SubmitResult{Session: sess, CardID: card.ID, CardStatus: status, TurnID: turnID, MessageID: msgID}, nil
}

// submitInput：选择卡 / 问答卡 / 删除确认卡 → 回到发卡前的状态，建轮继续。
//
// 若会话已不在 awaiting_input（用户先直接发了消息，卡片还挂着），把这次提交当成一条
// 用户消息处理：去向同 PostMessage。
func (s *Service) submitInput(ctx context.Context, a SessionActor, sess repo.Session, card repo.Card,
	submission json.RawMessage, ev domain.Event) (SubmitResult, error) {
	text := describeSubmission(card, submission)
	// 会话不在等这张卡时要按「直接发消息」路由，路由要知道会话此刻挂起在哪张卡上。
	// 在事务外取（同 PostMessage）：事务里关掉的只是这张过时的卡，不影响按状态找挂起卡。
	pending, err := s.repo.PendingBySession(ctx, sess.ID)
	if err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	tx, locked, card, err := s.beginLocked(ctx, sess.ID, card.ID)
	if err != nil {
		return SubmitResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var voided *repo.Card
	if locked.State == domain.StateAwaitingInput {
		if err := s.closeAndMove(ctx, tx, &locked, card, domain.CardSubmitted, submission, a.UserID, ev); err != nil {
			return SubmitResult{}, err
		}
	} else {
		ok, err := s.repo.CloseCard(ctx, tx, card.ID, domain.CardSubmitted, submission, a.UserID)
		if err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		if !ok {
			return SubmitResult{}, cardClosed(domain.CardSubmitted)
		}
		// 去向与直接发一条消息完全相同：停在规划确认时回到规划并作废规划卡，不会直接进执行。
		others := make([]repo.Card, 0, len(pending))
		for _, p := range pending {
			if p.ID != card.ID {
				others = append(others, p)
			}
		}
		if _, voided, err = s.routeAsMessage(ctx, tx, &locked, others); err != nil {
			return SubmitResult{}, err
		}
	}
	turnID, msgID, err := s.writeUserTurn(ctx, tx, sess.ID, text, nil, cardTurnKey(card.ID), card.ID)
	if err != nil {
		return SubmitResult{}, agentErr(err)
	}
	res, err := s.finishSubmit(ctx, tx, locked, card, domain.CardSubmitted, turnID, msgID)
	if err == nil && voided != nil {
		s.publishCard(ctx, sess.ProjectID, *voided, domain.CardVoided)
	}
	return res, err
}

// submitBudget：追加额度卡。approve → 额度 += 追加额、回执行；reject → 回 idle、不建轮（不再扣费）。
func (s *Service) submitBudget(ctx context.Context, a SessionActor, sess repo.Session, card repo.Card,
	submission json.RawMessage, ev domain.Event) (SubmitResult, error) {
	var p domain.BudgetPayload
	if err := json.Unmarshal(card.Payload, &p); err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, fmt.Errorf("追加额度卡内容损坏：%w", err))
	}
	if ev == domain.EvApprove && p.Requested > 0 && s.p.Accounts != nil {
		bal, err := s.p.Accounts.Balance(ctx, sess.PayerAccountID)
		if err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		if bal < p.Requested {
			return SubmitResult{}, insufficient(p.Requested, bal)
		}
	}
	tx, locked, card, err := s.beginLocked(ctx, sess.ID, card.ID)
	if err != nil {
		return SubmitResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if locked.State != domain.StateAwaitingApproval {
		return SubmitResult{}, cardClosed(card.Status)
	}
	if err := s.closeAndMove(ctx, tx, &locked, card, domain.CardSubmitted, submission, a.UserID, ev); err != nil {
		return SubmitResult{}, err
	}
	if ev != domain.EvApprove {
		msg, err := s.repo.Append(ctx, tx, repo.Message{ID: s.nextID(), SessionID: sess.ID,
			Role: repo.RoleUser, Content: "不用追加额度了", CardID: card.ID})
		if err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		return s.finishSubmit(ctx, tx, locked, card, domain.CardSubmitted, 0, msg.ID)
	}
	n, err := s.repo.AddApprovedBudget(ctx, tx, sess.ID, p.Requested)
	if err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	locked.ApprovedBudget = n
	turnID, msgID, err := s.writeUserTurn(ctx, tx, sess.ID,
		"同意继续", nil, cardTurnKey(card.ID), card.ID)
	if err != nil {
		return SubmitResult{}, agentErr(err)
	}
	return s.finishSubmit(ctx, tx, locked, card, domain.CardSubmitted, turnID, msgID)
}

func insufficient(required, balance int) error {
	return httpx.Errf(errcode.InsufficientCredits, "积分不足：需要 "+strconv.Itoa(required)+
		"，当前 "+strconv.Itoa(balance)+"，还差 "+strconv.Itoa(required-balance)).
		WithDetails(map[string]any{"required": required, "balance": balance, "shortfall": required - balance})
}

// submitPlan：规划确认卡。
func (s *Service) submitPlan(ctx context.Context, a SessionActor, sess repo.Session, card repo.Card,
	submission json.RawMessage, ev domain.Event) (SubmitResult, error) {
	if sess.State != domain.StateAwaitingApproval {
		return SubmitResult{}, cardClosed(card.Status)
	}
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(card.Payload, &p); err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, fmt.Errorf("规划确认卡内容损坏：%w", err))
	}
	dec, _ := domain.ParseDecision(card.Kind, submission)

	switch ev {
	case domain.EvAdjust:
		return s.voidPlan(ctx, a, sess, card, submission, "需要调整："+dec.Comment, domain.TriggerUser, "")
	case domain.EvReject:
		tx, locked, card, err := s.beginLocked(ctx, sess.ID, card.ID)
		if err != nil {
			return SubmitResult{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := s.closeAndMove(ctx, tx, &locked, card, domain.CardSubmitted, submission, a.UserID, ev); err != nil {
			return SubmitResult{}, err
		}
		if err := s.repo.ClearPlanVersion(ctx, tx, sess.ID); err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		content := "不做了"
		if dec.Comment != "" {
			content += "：" + dec.Comment
		}
		msg, err := s.repo.Append(ctx, tx, repo.Message{ID: s.nextID(), SessionID: sess.ID,
			Role: repo.RoleUser, Content: content, CardID: card.ID})
		if err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		return s.finishSubmit(ctx, tx, locked, card, domain.CardSubmitted, 0, msg.ID)
	}

	// approve：① 规划文档在确认前被改过 → 作废回规划。
	docVersion := 0
	if p.DocPath != "" && s.p.Projects != nil {
		v, found, err := s.p.Projects.DocVersion(ctx, a.WorkspaceID, sess.ProjectID, p.DocPath)
		if err != nil {
			return SubmitResult{}, err
		}
		if p.PlanDocVersion > 0 && (!found || v != p.PlanDocVersion) {
			res, err := s.voidPlan(ctx, a, sess, card, submission,
				"规划文档在确认前被修改过，原规划已作废，将按最新文档重新规划。", domain.TriggerSystem, "plan_doc_changed")
			if err != nil {
				return res, err
			}
			return res, httpx.Errf(errcode.AgentCardClosed, "规划文档在确认前被修改过，原规划已作废，Agent 会按最新文档重新规划").
				WithDetails(map[string]any{"cardStatus": string(domain.CardVoided), "reason": "plan_doc_changed"})
		}
		if found {
			docVersion = v
		}
	}
	// ①'' 变体上限兜底（MKT-T-27、MKT-AC-28）：submit_plan 超上限不出卡，但卡片也可能来自别处
	// （上限调小之前发的卡、绕过 Agent 写的卡）—— 花钱授权这一步再判一次，超了不批准。
	if err := domain.ValidateVariants(p.Items, s.p.MaxVariants); err != nil {
		var rej *domain.ToolRejection
		if errors.As(err, &rej) {
			return SubmitResult{}, httpx.Errf(errcode.InvalidArgument, rej.Message+"，请让 Agent 调整规划").
				WithDetails(map[string]any{"reason": rej.Code, "max": rej.Details["max"],
					"variants": rej.Details["variants"]})
		}
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	// ①' 营销条目（MKT-T-20）：选项不可用的条目不能批准；确认前换图按覆盖重算商品图与输入计数。
	planItems, overridden, err := s.planItemsForApproval(sess, p.Items, dec.ItemOverrides)
	if err != nil {
		return SubmitResult{}, err
	}
	// ② 服务端重新报价 + 余额（事务前：报价是跨域调用）。
	if s.p.Plans == nil || s.p.Accounts == nil {
		return SubmitResult{}, httpx.Errf(errcode.Internal, "报价端口未接入")
	}
	items, err := s.p.Plans.QuotePlan(ctx, planItems, a.MemberLevelID)
	if err != nil {
		return SubmitResult{}, err
	}
	// 裂变条目的说明里写着金额（MKT-T-28）：按这次报价重写，价格变了时写回卡片的说明与金额一致。
	items = domain.WithFissionNotes(items)
	total, err := sumCredits(items)
	if err != nil {
		return SubmitResult{}, err
	}
	balance, err := s.p.Accounts.Balance(ctx, sess.PayerAccountID)
	if err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	if total != p.Total {
		return SubmitResult{}, s.priceChanged(ctx, sess.ProjectID, card, p, items, total, balance)
	}
	if balance < total {
		return SubmitResult{}, insufficient(total, balance)
	}

	// ③ 事务：锁会话行 + 卡片行，再比一次合计（期间可能有另一次批准把新报价写回了卡片）。
	tx, locked, card, err := s.beginLocked(ctx, sess.ID, card.ID)
	if err != nil {
		return SubmitResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var now domain.PlanConfirmPayload
	if err := json.Unmarshal(card.Payload, &now); err != nil || now.Total != total {
		return SubmitResult{}, httpx.Errf(errcode.Conflict, "价格已更新，请确认新的报价").
			WithDetails(map[string]any{"reason": "price_changed", "total": total, "previousTotal": now.Total})
	}
	used, err := s.repo.UsedInTx(ctx, tx, sess.ID)
	if err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	// 换过图：批准后的卡片 payload 就是「已批准计划」，执行时按它发图（MKT-T-22），所以换后的条目
	// 必须在同一事务里写回卡片 —— 只记在 submission 里的话，卡片上显示的还是换之前的那几张。
	if overridden {
		payload, err := cardWithItems(card.Payload, items)
		if err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		if ok, err := s.repo.UpdateCardPayload(ctx, tx, card.ID, payload); err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		} else if !ok {
			return SubmitResult{}, cardClosed(domain.CardSubmitted)
		}
	}
	if err := s.closeAndMove(ctx, tx, &locked, card, domain.CardSubmitted, submission, a.UserID, ev); err != nil {
		return SubmitResult{}, err
	}
	// 额度 = 已占用 + 本次合计：同一会话里第二次批准规划时，前一次已花掉的不能算进新额度。
	approved := used + total
	var ver *int
	if docVersion > 0 {
		ver = &docVersion
	}
	if err := s.repo.SetApprovedBudget(ctx, tx, sess.ID, approved, ver); err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	locked.ApprovedBudget = approved
	turnID, msgID, err := s.writeUserTurn(ctx, tx, sess.ID,
		"确认规划", nil, cardTurnKey(card.ID), card.ID)
	if err != nil {
		return SubmitResult{}, agentErr(err)
	}
	return s.finishSubmit(ctx, tx, locked, card, domain.CardSubmitted, turnID, msgID)
}

func sumCredits(items []domain.PlanItem) (int, error) {
	total := 0
	for _, it := range items {
		if it.Credits < 0 || total > math.MaxInt32-it.Credits {
			return 0, httpx.Errf(errcode.InvalidArgument, "报价金额超出范围")
		}
		total += it.Credits
	}
	return total, nil
}

// planItemsForApproval 是批准时要重新报价的条目（MKT-T-20）。
//
//   - 有条目因营销选项不可用（如依赖出声的 Hook 配了不出声的模型）→ 42215，不批准。
//     ⚠️ 不能交给重新报价去判：报价只看模型能不能做，报得出价就会把这条的阻塞清掉 ——
//     用户点一次确认，一个注定出不了声的条目就被批准了。
//   - 换图（itemOverrides，ValidateSubmission 已校验键、范围与上限）→ 按会话的商品图重算
//     ProductMediaIDs 与输入计数；changed 表示条目真的变了（要写回卡片）。
func (s *Service) planItemsForApproval(sess repo.Session, items []domain.PlanItem,
	ovs []domain.ItemOverride) ([]domain.PlanItem, bool, error) {
	for _, it := range items {
		if it.BlockedCode == errcode.MarketingOptionUnavailable.Name() {
			return nil, false, httpx.Errf(errcode.MarketingOptionUnavailable, "条目「"+it.Label+"」暂不可用："+
				it.BlockedReason+"，请让 Agent 调整规划").WithDetails(map[string]any{
				"key": it.Key, "reason": it.BlockedReason, "userFixable": it.UserFixable})
		}
	}
	if len(ovs) == 0 {
		return items, false, nil
	}
	si, err := ParseSceneInput(sess.SceneInput)
	if err != nil {
		return nil, false, httpx.Wrap(errcode.Internal, err)
	}
	out, changed, err := domain.ApplyOverrides(items, ovs, si.PlanInputs())
	if err != nil {
		return nil, false, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return out, changed, nil
}

// cardWithItems 只替换卡片 payload 里的 items，其余键原样保留（含将来新增的字段，§5.4）。
func cardWithItems(payload json.RawMessage, items []domain.PlanItem) (json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil || raw == nil {
		raw = map[string]json.RawMessage{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	raw["items"] = b
	return json.Marshal(raw)
}

// priceChanged 把新报价写回卡片并返回 409 CONFLICT（details 带新报价）。
func (s *Service) priceChanged(ctx context.Context, projectID int64, card repo.Card, p domain.PlanConfirmPayload,
	items []domain.PlanItem, total, balance int) error {
	prev := p.Total
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(card.Payload, &raw); err != nil || raw == nil {
		raw = map[string]json.RawMessage{}
	}
	// ⚠️ 只改金额相关的三个键，其余原样保留（含将来新增的字段，§5.4）。
	raw["items"], _ = json.Marshal(items)
	raw["total"], _ = json.Marshal(total)
	raw["balance"], _ = json.Marshal(balance)
	// 差额与能否批准跟着新报价走（AGT-23 发卡时写入）；不改的话卡片显示旧的「还差 ✦x」。
	_, shortfall, canApprove, _ := domain.PlanTotals(items, balance)
	raw["shortfall"], _ = json.Marshal(shortfall)
	raw["canApprove"], _ = json.Marshal(canApprove)
	payload, err := json.Marshal(raw)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if _, err := s.repo.UpdateCardPayload(ctx, s.pool(), card.ID, payload); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	s.publishCard(ctx, projectID, card, domain.CardPending)
	return httpx.Errf(errcode.Conflict, "价格已更新：合计由 ✦"+strconv.Itoa(prev)+" 变为 ✦"+strconv.Itoa(total)+
		"，请确认新的报价").WithDetails(map[string]any{
		"reason": "price_changed", "items": items, "total": total, "previousTotal": prev, "balance": balance,
	})
}

// voidPlan 作废规划确认卡并回规划，建一轮让 Agent 重新规划。
// trigger=system 时不计费（用户没有发起新的动作，是文档被改导致的作废）。
func (s *Service) voidPlan(ctx context.Context, a SessionActor, sess repo.Session, card repo.Card,
	submission json.RawMessage, text string, trigger domain.TurnTrigger, reason string) (SubmitResult, error) {
	tx, locked, card, err := s.beginLocked(ctx, sess.ID, card.ID)
	if err != nil {
		return SubmitResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.closeAndMove(ctx, tx, &locked, card, domain.CardVoided, submission, a.UserID,
		domain.EvAdjust); err != nil {
		return SubmitResult{}, err
	}
	if err := s.repo.ClearPlanVersion(ctx, tx, sess.ID); err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	var turnID, msgID int64
	if trigger == domain.TriggerUser {
		turnID, msgID, err = s.writeUserTurn(ctx, tx, sess.ID, text, nil, cardTurnKey(card.ID), card.ID)
		if err != nil {
			return SubmitResult{}, agentErr(err)
		}
	} else {
		turn, err := s.repo.InsertTurn(ctx, tx, repo.Turn{ID: s.nextID(), SessionID: sess.ID,
			Trigger: domain.TriggerSystem, IdempotencyKey: cardTurnKey(card.ID)})
		if err != nil {
			return SubmitResult{}, agentErr(err)
		}
		msg, err := s.repo.Append(ctx, tx, repo.Message{ID: s.nextID(), SessionID: sess.ID, TurnID: turn.ID,
			Role: repo.RoleSystemNote, Content: text, CardID: card.ID})
		if err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		if err := s.repo.EnqueueTurnInTx(ctx, tx, s.nextID(), sess.ID, turn.ID); err != nil {
			return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
		}
		turnID, msgID = turn.ID, msg.ID
	}
	if err := tx.Commit(ctx); err != nil {
		return SubmitResult{}, httpx.Wrap(errcode.Internal, err)
	}
	s.publishCard(ctx, sess.ProjectID, card, domain.CardVoided)
	s.publishState(ctx, locked, turnID, reason)
	return SubmitResult{Session: locked, CardID: card.ID, CardStatus: domain.CardVoided,
		TurnID: turnID, MessageID: msgID}, nil
}

// describeSubmission 把一次提交写成「具体内容」的用户消息（design/03 §3.1）：
// 「我选择了：方向 1 悲壮离别」，而不是一句笼统的「我已确认」。
func describeSubmission(card repo.Card, submission json.RawMessage) string {
	label := func(opts []domain.Option, ids []string) []string {
		m := map[string]string{}
		for _, o := range opts {
			m[o.ID] = o.Label
		}
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			if l := m[id]; l != "" {
				out = append(out, l)
			} else {
				out = append(out, id)
			}
		}
		return out
	}
	switch card.Kind {
	case domain.CardChoice:
		var p domain.ChoicePayload
		var sub domain.ChoiceSubmission
		_ = json.Unmarshal(card.Payload, &p)
		_ = json.Unmarshal(submission, &sub)
		parts := label(p.Options, sub.Selected)
		if sub.Other != nil {
			parts = append(parts, "我的想法："+strings.TrimSpace(*sub.Other))
		}
		return "我选择了：" + strings.Join(parts, "；")
	case domain.CardQuestions:
		var p domain.QuestionsPayload
		var sub domain.QuestionsSubmission
		_ = json.Unmarshal(card.Payload, &p)
		_ = json.Unmarshal(submission, &sub)
		byID := map[string]domain.Answer{}
		for _, ans := range sub.Answers {
			byID[ans.QuestionID] = ans
		}
		lines := make([]string, 0, len(p.Questions))
		for _, q := range p.Questions {
			ans := byID[q.ID]
			var v []string
			if q.Type == domain.QuestionText {
				v = []string{strings.TrimSpace(ans.Text)}
			} else {
				v = label(q.Options, ans.Selected)
				if ans.Other != nil {
					v = append(v, strings.TrimSpace(*ans.Other))
				}
			}
			lines = append(lines, q.Text+"："+strings.Join(v, "、"))
		}
		return "我的回答：\n" + strings.Join(lines, "\n")
	case domain.CardDeleteConfirm:
		var p domain.DeleteConfirmPayload
		_ = json.Unmarshal(card.Payload, &p)
		dec, _ := domain.ParseDecision(card.Kind, submission)
		if dec.Decision == domain.DecisionApprove {
			return "同意删除这 " + strconv.Itoa(len(p.NodeIDs)) + " 个节点"
		}
		return "保留这些节点"
	}
	return "已提交"
}

// ---------------------------------------------------------------- 停止 / 取消

// TaskRef 是会话里一个挂着生成任务的步骤。
type TaskRef struct {
	StepID        int64
	GenerationID  int64
	Tool          string
	Status        string
	CreditsQuoted int
}

// Stop 停止当前轮（AG-19）：只写 stop_requested_at，由 Runner 在下一次流式回调或工具前结束本轮。
// ⚠️ 已提交的生成任务**不自动取消**，原样列出让用户逐个决定。
func (s *Service) Stop(ctx context.Context, a SessionActor, sessionID int64) (repo.Session, []TaskRef, error) {
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return repo.Session{}, nil, err
	}
	if err := requireController(a, sess); err != nil {
		return repo.Session{}, nil, err
	}
	if err := notCancelled(sess.State); err != nil {
		return repo.Session{}, nil, err
	}
	if _, err := s.repo.RequestStop(ctx, sess.ID); err != nil {
		return repo.Session{}, nil, httpx.Wrap(errcode.Internal, err)
	}
	tasks, err := s.openTasks(ctx, a, sess.ID)
	if err != nil {
		return repo.Session{}, nil, err
	}
	s.publishState(ctx, sess, 0, "stop_requested")
	return sess, tasks, nil
}

// openTasks 列出本会话未终态的生成任务（任务状态以生成域为准，步骤状态只是线索）。
func (s *Service) openTasks(ctx context.Context, a SessionActor, sessionID int64) ([]TaskRef, error) {
	steps, err := s.repo.GenerationStepsBySession(ctx, sessionID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]TaskRef, 0, len(steps))
	for _, st := range steps {
		ref := TaskRef{StepID: st.ID, GenerationID: st.GenerationID, Tool: st.Tool,
			CreditsQuoted: st.CreditsQuoted, Status: "unknown"}
		if s.p.Tasks != nil {
			ts, err := s.p.Tasks.Status(ctx, a.WorkspaceID, st.GenerationID)
			if err == nil {
				if ts.Terminal {
					continue
				}
				ref.Status = ts.Status
			}
		}
		out = append(out, ref)
	}
	return out, nil
}

// CancelResult 是取消会话的结果。
type CancelResult struct {
	Session   repo.Session
	Cancelled []int64
	// Failed 是取消失败的任务与原因（例如任务恰好已经结束）。
	Failed map[int64]string
	// Remaining 是未取消、仍在运行的任务（cancelTasks=false 时）。
	Remaining []TaskRef
}

// Cancel 取消会话：状态 → cancelled（终态），待处理卡片作废；cancelTasks=true 时逐个取消
// 未终态的生成任务（退款由生成域的取消路径负责，只退一次）。
func (s *Service) Cancel(ctx context.Context, a SessionActor, sessionID int64, cancelTasks bool) (CancelResult, error) {
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return CancelResult{}, err
	}
	if err := requireController(a, sess); err != nil {
		return CancelResult{}, err
	}
	if sess.State != domain.StateCancelled {
		// 让正在跑的那一轮尽快停下（Runner 看到 cancelled 也会停，这只是更快）。
		if _, err := s.repo.RequestStop(ctx, sess.ID); err != nil {
			return CancelResult{}, httpx.Wrap(errcode.Internal, err)
		}
		pending, err := s.repo.PendingBySession(ctx, sess.ID)
		if err != nil {
			return CancelResult{}, httpx.Wrap(errcode.Internal, err)
		}
		tx, err := s.repo.Begin(ctx)
		if err != nil {
			return CancelResult{}, httpx.Wrap(errcode.Internal, err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		locked, err := s.repo.LockForUpdate(ctx, tx, sess.ID)
		if err != nil {
			return CancelResult{}, agentErr(err)
		}
		if locked.State != domain.StateCancelled {
			to, ok := domain.CanTransition(locked.State, domain.EvCancel)
			if !ok {
				return CancelResult{}, httpx.Errf(errcode.InvalidStateTransition, "当前状态不能取消")
			}
			if _, err := s.repo.TransitionState(ctx, tx, sess.ID, locked.State, to); err != nil {
				return CancelResult{}, httpx.Wrap(errcode.Internal, err)
			}
			for _, c := range pending {
				if _, err := s.repo.VoidCard(ctx, tx, c.ID); err != nil {
					return CancelResult{}, httpx.Wrap(errcode.Internal, err)
				}
			}
			locked.State = to
		}
		if err := tx.Commit(ctx); err != nil {
			return CancelResult{}, httpx.Wrap(errcode.Internal, err)
		}
		sess = locked
		for _, c := range pending {
			s.publishCard(ctx, sess.ProjectID, c, domain.CardVoided)
		}
		s.publishState(ctx, sess, 0, "cancelled")
	}

	res := CancelResult{Session: sess, Failed: map[int64]string{}}
	tasks, err := s.openTasks(ctx, a, sess.ID)
	if err != nil {
		return res, err
	}
	if !cancelTasks || s.p.Tasks == nil {
		res.Remaining = tasks
		return res, nil
	}
	for _, t := range tasks {
		if err := s.p.Tasks.Cancel(ctx, a.WorkspaceID, t.GenerationID, "取消 Agent 会话"); err != nil {
			var be *httpx.BizError
			msg := "取消失败"
			if errors.As(err, &be) && be.Message != "" {
				msg = be.Message
			}
			res.Failed[t.GenerationID] = msg
			continue
		}
		res.Cancelled = append(res.Cancelled, t.GenerationID)
	}
	return res, nil
}

// ---------------------------------------------------------------- 查看

// SessionDetail 是会话详情。
type SessionDetail struct {
	Session       repo.Session
	InitiatorName string
	Budget        BudgetSummary
	PendingCards  []repo.Card
	LatestTurn    *repo.Turn
	Messages      MessagePage
	// PlanProgress 营销会话最近一张已批准计划的执行进度（MKT-T-27，现算）；没有时为 nil。
	PlanProgress *PlanProgressDetail
}

// Get 会话详情：状态、todo、额度与已花、待处理卡片、最后 N 条消息。同工作区成员都能看。
func (s *Service) Get(ctx context.Context, a SessionActor, sessionID int64) (SessionDetail, error) {
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return SessionDetail{}, err
	}
	out := SessionDetail{Session: sess}
	if names, err := s.repo.UserNames(ctx, []int64{sess.CreatedByUserID}); err == nil {
		out.InitiatorName = names[sess.CreatedByUserID]
	}
	if out.Budget, err = s.budget(ctx, sess.ID); err != nil {
		return SessionDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	out.Budget.Approved = sess.ApprovedBudget
	if out.PendingCards, err = s.repo.PendingBySession(ctx, sess.ID); err != nil {
		return SessionDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	if t, err := s.repo.LatestTurn(ctx, sess.ID); err == nil {
		out.LatestTurn = &t
	} else if !errors.Is(err, repo.ErrNotFound) {
		return SessionDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	if out.Messages, err = s.messagePage(ctx, sess.ID, 0, recentInDetail); err != nil {
		return SessionDetail{}, err
	}
	if out.PlanProgress, err = s.planProgress(ctx, sess); err != nil {
		return SessionDetail{}, httpx.Wrap(errcode.Internal, err)
	}
	return out, nil
}

// SessionListItem 是会话列表的一项。
type SessionListItem struct {
	Session       repo.Session
	InitiatorName string
}

// ListByProject 本项目的会话（含他人发起的，带发起人）。
func (s *Service) ListByProject(ctx context.Context, a SessionActor, projectID int64, limit int) (
	[]SessionListItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.repo.ListByProject(ctx, a.WorkspaceID, projectID, limit)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.CreatedByUserID)
	}
	names, err := s.repo.UserNames(ctx, ids)
	if err != nil {
		names = map[int64]string{}
	}
	out := make([]SessionListItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, SessionListItem{Session: r, InitiatorName: names[r.CreatedByUserID]})
	}
	return out, nil
}

// MessagePage 是一页消息及其展示所需的步骤、卡片与轮次。
type MessagePage struct {
	Messages []repo.Message
	Steps    []repo.Step
	Cards    []repo.Card
	Turns    []repo.Turn
	// NextCursor 是更早一页的游标（本页最小 seq），0 表示没有更早的了。
	NextCursor int
}

// ListMessages 消息分页（往前翻），含本页涉及的步骤、卡片与轮次。
func (s *Service) ListMessages(ctx context.Context, a SessionActor, sessionID int64, cursor, limit int) (
	MessagePage, error) {
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return MessagePage{}, err
	}
	return s.messagePage(ctx, sess.ID, cursor, limit)
}

func (s *Service) messagePage(ctx context.Context, sessionID int64, cursor, limit int) (MessagePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	msgs, next, err := s.repo.ListPage(ctx, sessionID, cursor, limit)
	if err != nil {
		return MessagePage{}, httpx.Wrap(errcode.Internal, err)
	}
	var turnIDs, cardIDs []int64
	seenT, seenC := map[int64]bool{}, map[int64]bool{}
	for _, m := range msgs {
		if m.TurnID != 0 && !seenT[m.TurnID] {
			seenT[m.TurnID] = true
			turnIDs = append(turnIDs, m.TurnID)
		}
		if m.CardID != 0 && !seenC[m.CardID] {
			seenC[m.CardID] = true
			cardIDs = append(cardIDs, m.CardID)
		}
	}
	page := MessagePage{Messages: msgs, NextCursor: next}
	if page.Steps, err = s.repo.StepsByTurns(ctx, sessionID, turnIDs); err != nil {
		return MessagePage{}, httpx.Wrap(errcode.Internal, err)
	}
	if page.Turns, err = s.repo.TurnsByIDs(ctx, sessionID, turnIDs); err != nil {
		return MessagePage{}, httpx.Wrap(errcode.Internal, err)
	}
	if page.Cards, err = s.repo.CardsByIDs(ctx, sessionID, cardIDs); err != nil {
		return MessagePage{}, httpx.Wrap(errcode.Internal, err)
	}
	return page, nil
}

// TurnLedger 是一轮的账本对账（design/01 §6.3）：三项全部来自 credit_transaction。
type TurnLedger struct {
	Turn repo.Turn
	// Chat 对话费（refType=agent_turn 的消费）。
	Chat int
	// Generation 生成费（本轮步骤提交的生成任务的消费）。
	Generation int
	// Refunded 退回（以上两类的退款）。⚠️ 单列，**不从花费里减**（XYQ-114）。
	Refunded int
	Entries  []LedgerEntry
}

// TurnLedgerOf 取一轮的对话费 / 生成费 / 退回逐笔流水。
func (s *Service) TurnLedgerOf(ctx context.Context, a SessionActor, sessionID, turnID int64) (TurnLedger, error) {
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return TurnLedger{}, err
	}
	turn, err := s.repo.GetTurn(ctx, turnID)
	if errors.Is(err, repo.ErrNotFound) || (err == nil && turn.SessionID != sess.ID) {
		return TurnLedger{}, httpx.Errf(errcode.NotFound, "轮次不存在")
	}
	if err != nil {
		return TurnLedger{}, httpx.Wrap(errcode.Internal, err)
	}
	steps, err := s.repo.StepsByTurns(ctx, sess.ID, []int64{turn.ID})
	if err != nil {
		return TurnLedger{}, httpx.Wrap(errcode.Internal, err)
	}
	refs := []LedgerRef{{Type: RefTypeTurn, ID: idStr(turn.ID)}}
	seen := map[LedgerRef]bool{refs[0]: true}
	add := func(r LedgerRef) {
		if !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	var noGen []int64
	for _, st := range steps {
		if st.GenerationID != 0 {
			add(LedgerRef{Type: RefTypeGeneration, ID: idStr(st.GenerationID)})
		} else {
			noGen = append(noGen, st.ID)
		}
	}
	// 多段工具（AGT-50）的步骤挂的是组 / 整图运行 / 渲染 / 同步文本调用：从额度条目的引用换成账本引用。
	if len(noGen) > 0 {
		entries, err := s.repo.BudgetsBySteps(ctx, s.repo.Pool(), noGen)
		if err != nil {
			return TurnLedger{}, httpx.Wrap(errcode.Internal, err)
		}
		for _, id := range noGen {
			if e, ok := entries[id]; ok {
				for _, r := range LedgerRefsOf(e.RefType, e.RefID, e.RefIDs) {
					add(r)
				}
			}
		}
	}
	out := TurnLedger{Turn: turn}
	if s.p.Accounts == nil {
		return out, nil
	}
	entries, err := s.p.Accounts.TransactionsByRefs(ctx, sess.PayerAccountID, refs)
	if err != nil {
		return TurnLedger{}, err
	}
	out.Entries = entries
	for _, e := range entries {
		amount := e.Change
		if amount < 0 {
			amount = -amount
		}
		switch {
		case e.Type == "consume" && e.RefType == RefTypeTurn:
			out.Chat += amount
		case e.Type == "consume":
			// 生成任务、分镜 / 合成的同步调用、渲染：都是这一轮提交的付费工作。
			out.Generation += amount
		case e.Type == "refund":
			out.Refunded += amount
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 修改、偏好、技能

// PatchInput 是改会话的入参。nil 表示该项不改。
type PatchInput struct {
	Title *string
	Prefs json.RawMessage
}

// Patch 改标题与会话级偏好（发起人或管理员）。
func (s *Service) Patch(ctx context.Context, a SessionActor, sessionID int64, in PatchInput) (repo.Session, error) {
	sess, err := s.load(ctx, a, sessionID)
	if err != nil {
		return repo.Session{}, err
	}
	if err := requireController(a, sess); err != nil {
		return repo.Session{}, err
	}
	var title *string
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" || utf8.RuneCountInString(t) > MaxTitleRunes {
			return repo.Session{}, httpx.Errf(errcode.InvalidArgument, "标题必须是 1~100 字")
		}
		title = &t
	}
	prefs, err := validatePrefs(in.Prefs)
	if err != nil {
		return repo.Session{}, err
	}
	if title == nil && prefs == nil {
		return sess, nil
	}
	out, err := s.repo.UpdateSessionMeta(ctx, s.pool(), sess.ID, title, prefs)
	return out, agentErr(err)
}

// UserPrefs 取用户级模型偏好（没设过返回 `{}`）。
func (s *Service) UserPrefs(ctx context.Context, userID int64) (json.RawMessage, error) {
	p, err := s.repo.GetUserPref(ctx, userID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return p, nil
}

// SaveUserPrefs 整份写入用户级偏好（原样存，保留前端未知字段）。
func (s *Service) SaveUserPrefs(ctx context.Context, userID int64, prefs json.RawMessage) (json.RawMessage, error) {
	p, err := validatePrefs(prefs)
	if err != nil {
		return nil, err
	}
	if p == nil {
		p = json.RawMessage(`{}`)
	}
	if err := s.repo.UpsertUserPref(ctx, userID, p); err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return p, nil
}

// Skills 列出启用的技能。
func (s *Service) Skills(ctx context.Context) ([]repo.Skill, error) {
	rows, err := s.repo.ListEnabledSkills(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return rows, nil
}

// ---------------------------------------------------------------- 内部

// pool 返回不在事务里的查询器（repo 的写方法收 Querier）。
func (s *Service) pool() repo.Querier { return s.repo.Pool() }
