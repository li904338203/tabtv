// Package transport 是画布 Agent 的用户端路由（AGT-26，dev/02 §4）。
//
// ⚠️ 权限闸在路由层（§5.17）：会话类路由挂在组装根的 wsGuard 上（GET → view，其余 → edit），
// 只读成员调任何写接口都在这里 403，app 层再判「只有发起人能继续对话」。
// 用户偏好只要登录（偏好属于人，不属于工作区）。
// ⚠️ 总开关 SEEAITV_AGENT_ENABLED=false 时全部路由 404 AGENT_DISABLED —— 在鉴权**之前**判，
// 关掉的功能不该先要求登录再告诉你它不存在。
package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collabtransport "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// HeaderIdempotencyKey 与生成受理同一个头。
const HeaderIdempotencyKey = "Idempotency-Key"

// maxBody 请求体上限：消息 8000 字 + 引用 + 偏好，256KiB 足够；更大的直接拒。
const maxBody = 256 << 10

// Handler 是 Agent 的 HTTP 入口。
type Handler struct {
	svc         *app.Service
	userIDs     func(r *http.Request) int64
	memberLevel func(*http.Request) int64
	enabled     bool
	media       MediaServer
}

// MediaServer 由媒体域实现：把一份媒体按 ID 吐给客户端，**不做归属校验**
// （调用方已按「技能启用」判过，MKD-19）。端口定义在这里：agent 不 import 媒体域。
type MediaServer interface {
	ServePublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
	// RedirectPublic 同 ServePublic，但 302 到对象存储的短时效签名地址、不经 API 转发字节（2026-11-02）。
	// 只给 <img> / <video> 直接加载的匿名公开端点用。
	RedirectPublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int)
}

// SetMedia 注入公开取图（技能封面）。未注入时封面端点回 500「媒体服务未接入」。
func (h *Handler) SetMedia(m MediaServer) { h.media = m }

// NewHandler 构造 handler。enabled=false 时全部路由 404 AGENT_DISABLED。
func NewHandler(svc *app.Service, userIDs func(r *http.Request) int64, enabled bool) *Handler {
	return &Handler{svc: svc, userIDs: userIDs, enabled: enabled,
		memberLevel: func(*http.Request) int64 { return 0 }}
}

// SetMemberLevelResolver 注入会员等级解析器（与生成受理**同一个**，批准规划时重新报价要用）。
func (h *Handler) SetMemberLevelResolver(f func(*http.Request) int64) {
	if f != nil {
		h.memberLevel = f
	}
}

func (h *Handler) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.enabled {
			httpx.Fail(w, r, httpx.Err(errcode.AgentDisabled))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Mount 挂载路由。requireWorkspace 用组装根的 wsGuard（带按方法的默认闸）。
func (h *Handler) Mount(r chi.Router, requireUser, requireWorkspace func(http.Handler) http.Handler) {
	// 技能封面是**公开**的（MKD-19、B-5）：营销页未登录也能浏览技能弹层的预览卡。
	// ⚠️ 必须挂在 requireUser / requireWorkspace 组之外 —— 挂进去的话访客全是破图。
	// 只受总开关控制；启用与否在 SkillCoverMedia 里判。
	r.With(h.gate).Get("/agent/skills/{code}/cover", h.skillCover)
	r.Group(func(r chi.Router) {
		r.Use(h.gate)
		r.Use(requireUser)
		// 用户级偏好：只要登录。
		r.Get("/agent/preferences", h.getPrefs)
		r.Put("/agent/preferences", h.putPrefs)

		r.Group(func(r chi.Router) {
			r.Use(requireWorkspace)
			r.Get("/agent/skills", h.skills)
			r.Post("/agent/sessions", h.createSession)
			// 一键优化提示词（AGT-43）：同步、按轮计费；POST 走默认 edit 闸（只读成员 403）。
			r.Post("/agent/prompt-optimize", h.optimize)
			r.Get("/projects/{projectId}/agent/sessions", h.listSessions)
			r.Get("/agent/sessions/{sessionId}", h.getSession)
			r.Patch("/agent/sessions/{sessionId}", h.patchSession)
			r.Get("/agent/sessions/{sessionId}/messages", h.listMessages)
			r.Post("/agent/sessions/{sessionId}/messages", h.postMessage)
			r.Post("/agent/sessions/{sessionId}/cards/{cardId}/submit", h.submitCard)
			r.Post("/agent/sessions/{sessionId}/stop", h.stop)
			r.Post("/agent/sessions/{sessionId}/cancel", h.cancel)
			r.Get("/agent/sessions/{sessionId}/turns/{turnId}/ledger", h.turnLedger)
		})
	})
}

// ---------------------------------------------------------------- 工具函数

func (h *Handler) actor(w http.ResponseWriter, r *http.Request) (app.SessionActor, bool) {
	ws, ok := collabtransport.WorkspaceFrom(r.Context())
	if !ok {
		httpx.Fail(w, r, httpx.Err(errcode.NotWorkspaceMember))
		return app.SessionActor{}, false
	}
	return app.SessionActor{
		WorkspaceID: ws.WorkspaceID, PayerAccountID: ws.PayerAccountID, Role: ws.Role,
		UserID: h.userIDs(r), MemberLevelID: h.memberLevel(r),
	}, true
}

func pathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, key), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, key+" 非法"))
		return 0, false
	}
	return id, true
}

// decode 严格解码请求体：未知字段直接拒（键名写错时不能当成「没传」，§5.10）。
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON 或含未知字段："+err.Error()))
		return false
	}
	return true
}

func idStr(v int64) string { return strconv.FormatInt(v, 10) }

func optID(v int64) *string {
	if v == 0 {
		return nil
	}
	s := idStr(v)
	return &s
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func optTS(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

func rawOr(v json.RawMessage, def string) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage(def)
	}
	return v
}

func optRaw(v json.RawMessage) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage("null")
	}
	return v
}

// ---------------------------------------------------------------- 视图

type refReq struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// URL 只为了**认出来并拒绝**：客户端给地址等于自己声明「我有权限」（GO-27）。
	URL string `json:"url,omitempty"`
	// Slot / Order 只用于建营销会话（MKT-T-17）；其余入口带了一律 400。
	Slot  string `json:"slot,omitempty"`
	Order int    `json:"order,omitempty"`
}

func toRefs(w http.ResponseWriter, r *http.Request, in []refReq) ([]app.RefInput, bool) {
	out := make([]app.RefInput, 0, len(in))
	for _, x := range in {
		if strings.TrimSpace(x.URL) != "" {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "引用只能按 ID，不接受地址"))
			return nil, false
		}
		out = append(out, app.RefInput{Kind: x.Kind, ID: x.ID, Slot: x.Slot, Order: x.Order})
	}
	return out, true
}

type sessionResp struct {
	ID              string          `json:"id"`
	WorkspaceID     string          `json:"workspaceId"`
	ProjectID       string          `json:"projectId"`
	Title           string          `json:"title"`
	State           string          `json:"state"`
	CreatedByUserID string          `json:"createdByUserId"`
	InitiatorName   string          `json:"initiatorName"`
	IsInitiator     bool            `json:"isInitiator"`
	SkillCodes      json.RawMessage `json:"skillCodes"`
	Prefs           json.RawMessage `json:"prefs"`
	Todo            json.RawMessage `json:"todo"`
	ApprovedBudget  int             `json:"approvedBudget"`
	PlanDocVersion  *int            `json:"planDocVersion"`
	StopRequested   bool            `json:"stopRequested"`
	LastActiveAt    string          `json:"lastActiveAt"`
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
}

func toSession(s repo.Session, name string, me int64) sessionResp {
	return sessionResp{
		ID: idStr(s.ID), WorkspaceID: idStr(s.WorkspaceID), ProjectID: idStr(s.ProjectID),
		Title: s.Title, State: string(s.State), CreatedByUserID: idStr(s.CreatedByUserID),
		InitiatorName: name, IsInitiator: s.CreatedByUserID == me,
		SkillCodes: rawOr(s.SkillCodes, "[]"), Prefs: rawOr(s.Prefs, "{}"), Todo: rawOr(s.Todo, "[]"),
		ApprovedBudget: s.ApprovedBudget, PlanDocVersion: s.PlanDocVersion,
		StopRequested: s.StopRequestedAt != nil,
		LastActiveAt:  ts(s.LastActiveAt), CreatedAt: ts(s.CreatedAt), UpdatedAt: ts(s.UpdatedAt),
	}
}

type messageResp struct {
	ID         string          `json:"id"`
	Seq        int             `json:"seq"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	Refs       json.RawMessage `json:"refs"`
	ToolCalls  json.RawMessage `json:"toolCalls"`
	ToolCallID *string         `json:"toolCallId"`
	TurnID     *string         `json:"turnId"`
	CardID     *string         `json:"cardId"`
	CreatedAt  string          `json:"createdAt"`
}

func toMessage(m repo.Message) messageResp {
	out := messageResp{
		ID: idStr(m.ID), Seq: m.Seq, Role: m.Role, Content: m.Content,
		Refs: rawOr(m.Refs, "[]"), ToolCalls: optRaw(m.ToolCalls),
		TurnID: optID(m.TurnID), CardID: optID(m.CardID), CreatedAt: ts(m.CreatedAt),
	}
	if m.ToolCallID != "" {
		v := m.ToolCallID
		out.ToolCallID = &v
	}
	return out
}

type stepResp struct {
	ID            string          `json:"id"`
	TurnID        string          `json:"turnId"`
	Tool          string          `json:"tool"`
	Status        string          `json:"status"`
	Attempt       int             `json:"attempt"`
	Display       json.RawMessage `json:"display"`
	GenerationID  *string         `json:"generationId"`
	CreditsQuoted int             `json:"creditsQuoted"`
	ErrorMessage  string          `json:"errorMessage"`
	StartedAt     string          `json:"startedAt"`
	FinishedAt    *string         `json:"finishedAt"`
}

func toStep(s repo.Step) stepResp {
	return stepResp{
		ID: idStr(s.ID), TurnID: idStr(s.TurnID), Tool: s.Tool, Status: s.Status, Attempt: s.Attempt,
		Display: rawOr(s.Display, "{}"), GenerationID: optID(s.GenerationID),
		CreditsQuoted: s.CreditsQuoted, ErrorMessage: s.ErrorMessage,
		StartedAt: ts(s.StartedAt), FinishedAt: optTS(s.FinishedAt),
	}
}

type cardResp struct {
	ID          string          `json:"id"`
	StepID      string          `json:"stepId"`
	Kind        string          `json:"kind"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload"`
	Submission  json.RawMessage `json:"submission"`
	SubmittedBy *string         `json:"submittedBy"`
	SubmittedAt *string         `json:"submittedAt"`
	CreatedAt   string          `json:"createdAt"`
}

func toCard(c repo.Card) cardResp {
	return cardResp{
		ID: idStr(c.ID), StepID: idStr(c.StepID), Kind: string(c.Kind), Status: string(c.Status),
		Payload: rawOr(c.Payload, "{}"), Submission: optRaw(c.Submission),
		SubmittedBy: optID(c.SubmittedBy), SubmittedAt: optTS(c.SubmittedAt), CreatedAt: ts(c.CreatedAt),
	}
}

type turnResp struct {
	ID           string  `json:"id"`
	Trigger      string  `json:"trigger"`
	Status       string  `json:"status"`
	ModelCode    string  `json:"modelCode"`
	CreditsCost  int     `json:"creditsCost"`
	CallCount    int     `json:"callCount"`
	ValidOutputs int     `json:"validOutputs"`
	ErrorCode    string  `json:"errorCode"`
	ErrorMessage string  `json:"errorMessage"`
	StartedAt    *string `json:"startedAt"`
	FinishedAt   *string `json:"finishedAt"`
	CreatedAt    string  `json:"createdAt"`
}

func toTurn(t repo.Turn) turnResp {
	return turnResp{
		ID: idStr(t.ID), Trigger: string(t.Trigger), Status: string(t.Status), ModelCode: t.ModelCode,
		CreditsCost: t.CreditsCost, CallCount: t.CallCount, ValidOutputs: t.ValidOutputs,
		ErrorCode: t.ErrorCode, ErrorMessage: t.ErrorMessage,
		StartedAt: optTS(t.StartedAt), FinishedAt: optTS(t.FinishedAt), CreatedAt: ts(t.CreatedAt),
	}
}

type pageResp struct {
	Items      []messageResp `json:"items"`
	Steps      []stepResp    `json:"steps"`
	Cards      []cardResp    `json:"cards"`
	Turns      []turnResp    `json:"turns"`
	NextCursor *string       `json:"nextCursor"`
}

func toPage(p app.MessagePage) pageResp {
	out := pageResp{
		Items: make([]messageResp, 0, len(p.Messages)), Steps: make([]stepResp, 0, len(p.Steps)),
		Cards: make([]cardResp, 0, len(p.Cards)), Turns: make([]turnResp, 0, len(p.Turns)),
	}
	for _, m := range p.Messages {
		out.Items = append(out.Items, toMessage(m))
	}
	for _, s := range p.Steps {
		out.Steps = append(out.Steps, toStep(s))
	}
	for _, c := range p.Cards {
		out.Cards = append(out.Cards, toCard(c))
	}
	for _, t := range p.Turns {
		out.Turns = append(out.Turns, toTurn(t))
	}
	if p.NextCursor > 0 {
		s := strconv.Itoa(p.NextCursor)
		out.NextCursor = &s
	}
	return out
}

type taskResp struct {
	StepID        string `json:"stepId"`
	GenerationID  string `json:"generationId"`
	Tool          string `json:"tool"`
	Status        string `json:"status"`
	CreditsQuoted int    `json:"creditsQuoted"`
}

func toTasks(in []app.TaskRef) []taskResp {
	out := make([]taskResp, 0, len(in))
	for _, t := range in {
		out = append(out, taskResp{StepID: idStr(t.StepID), GenerationID: idStr(t.GenerationID),
			Tool: t.Tool, Status: t.Status, CreditsQuoted: t.CreditsQuoted})
	}
	return out
}

// ---------------------------------------------------------------- 会话

type createReq struct {
	ProjectID     string `json:"projectId"`
	CreateProject *struct {
		Name string `json:"name"`
	} `json:"createProject"`
	Title   string `json:"title"`
	Message struct {
		Text string   `json:"text"`
		Refs []refReq `json:"refs"`
	} `json:"message"`
	SkillCode string          `json:"skillCode"`
	Prefs     json.RawMessage `json:"prefs"`
	// Scene 缺省 general（MKT-T-17）；marketing 只在营销页发送时带。
	Scene     string              `json:"scene"`
	Marketing *marketingCreateReq `json:"marketing"`
}

// marketingCreateReq 是建营销会话的营销输入（dev/02 §4.3）。picks 只有 code：版本由服务端钉。
//
// ⚠️ product 已废弃（MKT-T-37，MKD-27）：商品名称与卖点改由商品库提供（商品槽引用 marketing_product）。
// 字段留着只为**认出来并拒绝**（出现即 40001 legacy_shape）—— 删掉的话严格解码报的是「未知字段」，
// 前端据此分不出「页面旧了要刷新」。
type marketingCreateReq struct {
	Product json.RawMessage `json:"product"`
	Picks   struct {
		Creative *string `json:"creative"`
		Hook     *string `json:"hook"`
		Style    *string `json:"style"`
	} `json:"picks"`
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type writeResp struct {
	SessionID string  `json:"sessionId"`
	ProjectID string  `json:"projectId"`
	State     string  `json:"state"`
	TurnID    *string `json:"turnId"`
	MessageID *string `json:"messageId"`
	Queued    bool    `json:"queued"`
	Replayed  bool    `json:"replayed"`
}

func toWrite(res app.SessionWrite) writeResp {
	return writeResp{
		SessionID: idStr(res.Session.ID), ProjectID: idStr(res.Session.ProjectID),
		State: string(res.Session.State), TurnID: optID(res.TurnID), MessageID: optID(res.MessageID),
		Queued: res.Queued, Replayed: res.Replayed,
	}
}

func (h *Handler) createSession(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	in, ok := parseCreate(w, r)
	if !ok {
		return
	}
	res, err := h.svc.CreateSession(r.Context(), a, in)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// 新建 201；幂等重放 200（同生成受理：客户端据此分辨是不是重试命中了）。
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	httpx.JSON(w, status, toWrite(res))
}

// parseCreate 解析建会话请求体（严格解码；失败时已写好响应）。
func parseCreate(w http.ResponseWriter, r *http.Request) (app.CreateSessionInput, bool) {
	var req createReq
	if !decode(w, r, &req) {
		return app.CreateSessionInput{}, false
	}
	refs, ok := toRefs(w, r, req.Message.Refs)
	if !ok {
		return app.CreateSessionInput{}, false
	}
	in := app.CreateSessionInput{
		IdempotencyKey: r.Header.Get(HeaderIdempotencyKey), Title: req.Title,
		Text: req.Message.Text, Refs: refs, SkillCode: req.SkillCode, Prefs: req.Prefs,
		Scene: domain.Scene(req.Scene),
	}
	if m := req.Marketing; m != nil {
		in.Marketing = &app.MarketingInput{
			LegacyProduct: m.Product != nil,
			Picks: app.MarketingPicks{Creative: strOr(m.Picks.Creative), Hook: strOr(m.Picks.Hook),
				Style: strOr(m.Picks.Style)},
		}
	}
	if req.CreateProject != nil {
		name := req.CreateProject.Name
		in.CreateProjectName = &name
	} else if req.ProjectID != "" {
		id, err := strconv.ParseInt(req.ProjectID, 10, 64)
		if err != nil || id <= 0 {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "projectId 非法"))
			return app.CreateSessionInput{}, false
		}
		in.ProjectID = id
	}
	return in, true
}

type optimizeReq struct {
	Text      string `json:"text"`
	ModelCode string `json:"modelCode,omitempty"`
	// Purpose 可选：general（缺省）/ character_appearance（MKT-T-35「AI 帮写」角色形象）。
	Purpose string `json:"purpose,omitempty"`
}

type optimizeResp struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	ModelCode string `json:"modelCode"`
	Credits   int    `json:"credits"`
	Replayed  bool   `json:"replayed"`
}

// optimize 一键优化提示词：结果只回给前端，由用户确认替换（这里不改任何用户数据）。
func (h *Handler) optimize(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	var req optimizeReq
	if !decode(w, r, &req) {
		return
	}
	res, err := h.svc.OptimizePrompt(r.Context(), a, app.OptimizeInput{
		IdempotencyKey: r.Header.Get(HeaderIdempotencyKey), Text: req.Text, ModelCode: req.ModelCode,
		Purpose: req.Purpose,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, optimizeResp{ID: idStr(res.ID), Text: res.Text, ModelCode: res.ModelCode,
		Credits: res.Credits, Replayed: res.Replayed})
}

func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "projectId")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.svc.ListByProject(r.Context(), a, pid, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	items := make([]sessionResp, 0, len(rows))
	for _, it := range rows {
		items = append(items, toSession(it.Session, it.InitiatorName, a.UserID))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getSession(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), a, sid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cards := make([]cardResp, 0, len(d.PendingCards))
	for _, c := range d.PendingCards {
		cards = append(cards, toCard(c))
	}
	var latest *turnResp
	if d.LatestTurn != nil {
		t := toTurn(*d.LatestTurn)
		latest = &t
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"session": toSession(d.Session, d.InitiatorName, a.UserID),
		"budget": map[string]int{
			"approved": d.Budget.Approved, "spent": d.Budget.Used, "refunded": d.Budget.Refunded,
		},
		"pendingCards": cards,
		"latestTurn":   latest,
		"messages":     toPage(d.Messages),
		// 场景与营销输入（MKT-T-17）：只有 ID、名称、卖点与 (code,version,name)，不含 URL 与片段。
		// 通用会话 sceneInput 为 null。
		"scene":      sceneOf(d.Session),
		"sceneInput": sceneInputOf(d.Session),
		// 批量变体的执行进度（MKT-T-27）：现算，没有已批准的营销计划时为 null。
		"planProgress": planProgressOf(d.PlanProgress),
	})
}

// planProgressResp 是 AgentPlanProgress。
type planProgressResp struct {
	CardID    string                 `json:"cardId"`
	Status    string                 `json:"status"`
	Total     int                    `json:"total"`
	Succeeded int                    `json:"succeeded"`
	Failed    int                    `json:"failed"`
	Stopped   int                    `json:"stopped"`
	Running   int                    `json:"running"`
	Pending   int                    `json:"pending"`
	Items     []planItemProgressResp `json:"items"`
}

type planItemProgressResp struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Variant      int    `json:"variant,omitempty"`
	VariantLabel string `json:"variantLabel,omitempty"`
	State        string `json:"state"`
	ErrorCode    string `json:"errorCode,omitempty"`
	Attempts     int    `json:"attempts"`
}

func planProgressOf(p *app.PlanProgressDetail) *planProgressResp {
	if p == nil {
		return nil
	}
	out := &planProgressResp{CardID: idStr(p.CardID), Status: p.Status, Total: p.Total, Succeeded: p.Succeeded,
		Failed: p.Failed, Stopped: p.Stopped, Running: p.Running, Pending: p.Pending,
		Items: make([]planItemProgressResp, 0, len(p.Items))}
	for _, it := range p.Items {
		out.Items = append(out.Items, planItemProgressResp{Key: it.Key, Label: it.Label, Variant: it.Variant,
			VariantLabel: it.VariantLabel, State: it.State, ErrorCode: it.ErrorCode, Attempts: it.Attempts})
	}
	return out
}

func sceneOf(s repo.Session) string {
	if s.Scene == "" {
		return string(domain.SceneGeneral)
	}
	return string(s.Scene)
}

func sceneInputOf(s repo.Session) json.RawMessage {
	if s.Scene != domain.SceneMarketing || len(s.SceneInput) == 0 {
		return json.RawMessage("null")
	}
	return s.SceneInput
}

// patchReq 是改会话的请求体：只有标题与偏好。⚠️ 不要往这里加 scene —— 场景建后不可改。
type patchReq struct {
	Title *string         `json:"title"`
	Prefs json.RawMessage `json:"prefs"`
}

// patchSession 改标题 / 偏好。⚠️ 严格解码（decode 拒未知字段）：请求体带 scene / sceneInput 等键
// 一律 400，场景建后不可改不是靠"没人传"（B-18）。
func (h *Handler) patchSession(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	var req patchReq
	if !decode(w, r, &req) {
		return
	}
	sess, err := h.svc.Patch(r.Context(), a, sid, app.PatchInput{Title: req.Title, Prefs: req.Prefs})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toSession(sess, "", a.UserID))
}

func (h *Handler) listMessages(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	q := r.URL.Query()
	cursor := 0
	if c := q.Get("cursor"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n <= 0 {
			httpx.Fail(w, r, httpx.Err(errcode.InvalidPagination))
			return
		}
		cursor = n
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	page, err := h.svc.ListMessages(r.Context(), a, sid, cursor, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toPage(page))
}

type messageReq struct {
	Text      string          `json:"text"`
	Refs      []refReq        `json:"refs"`
	SkillCode string          `json:"skillCode"`
	Prefs     json.RawMessage `json:"prefs"`
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	var req messageReq
	if !decode(w, r, &req) {
		return
	}
	refs, ok := toRefs(w, r, req.Refs)
	if !ok {
		return
	}
	res, err := h.svc.PostMessage(r.Context(), a, sid, app.PostMessageInput{
		IdempotencyKey: r.Header.Get(HeaderIdempotencyKey),
		Text:           req.Text, Refs: refs, SkillCode: req.SkillCode, Prefs: req.Prefs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	httpx.JSON(w, status, toWrite(res))
}

func (h *Handler) submitCard(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	cid, ok := pathID(w, r, "cardId")
	if !ok {
		return
	}
	var req struct {
		Submission json.RawMessage `json:"submission"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := h.svc.SubmitCard(r.Context(), a, sid, cid, req.Submission)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"sessionId": idStr(res.Session.ID), "cardId": idStr(res.CardID),
		"cardStatus": string(res.CardStatus), "state": string(res.Session.State),
		"turnId": optID(res.TurnID), "messageId": optID(res.MessageID),
		"approvedBudget": res.Session.ApprovedBudget,
	})
}

func (h *Handler) stop(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	sess, tasks, err := h.svc.Stop(r.Context(), a, sid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"sessionId": idStr(sess.ID), "state": string(sess.State), "stopRequested": true,
		"tasks": toTasks(tasks),
	})
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	var req struct {
		CancelTasks bool `json:"cancelTasks"`
	}
	// 请求体可省略（= 不取消任务）。
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidJSON, "请求体不是合法 JSON 或含未知字段："+err.Error()))
		return
	}
	res, err := h.svc.Cancel(r.Context(), a, sid, req.CancelTasks)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	cancelled := make([]string, 0, len(res.Cancelled))
	for _, id := range res.Cancelled {
		cancelled = append(cancelled, idStr(id))
	}
	failed := make([]map[string]string, 0, len(res.Failed))
	for id, msg := range res.Failed {
		failed = append(failed, map[string]string{"generationId": idStr(id), "message": msg})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"sessionId": idStr(res.Session.ID), "state": string(res.Session.State),
		"cancelledTasks": cancelled, "failedTasks": failed, "remainingTasks": toTasks(res.Remaining),
	})
}

type ledgerEntryResp struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Category  string `json:"category"`
	Change    int    `json:"change"`
	RefType   string `json:"refType"`
	RefID     string `json:"refId"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"createdAt"`
}

func ledgerCategory(e app.LedgerEntry) string {
	switch {
	case e.Type == "refund":
		return "refunded"
	case e.Type == "consume" && e.RefType == app.RefTypeTurn:
		return "chat"
	case e.Type == "consume" && e.RefType == app.RefTypeGeneration:
		return "generation"
	}
	return "other"
}

func (h *Handler) turnLedger(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r)
	if !ok {
		return
	}
	sid, ok := pathID(w, r, "sessionId")
	if !ok {
		return
	}
	tid, ok := pathID(w, r, "turnId")
	if !ok {
		return
	}
	l, err := h.svc.TurnLedgerOf(r.Context(), a, sid, tid)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	entries := make([]ledgerEntryResp, 0, len(l.Entries))
	for _, e := range l.Entries {
		entries = append(entries, ledgerEntryResp{
			ID: idStr(e.ID), Type: e.Type, Category: ledgerCategory(e), Change: e.Change,
			RefType: e.RefType, RefID: e.RefID, Reason: e.Reason, CreatedAt: ts(e.CreatedAt),
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"turnId": idStr(l.Turn.ID), "trigger": string(l.Turn.Trigger), "status": string(l.Turn.Status),
		"chat": l.Chat, "generation": l.Generation, "refunded": l.Refunded, "entries": entries,
	})
}

// ---------------------------------------------------------------- 技能与偏好

// skills 列出某个场景下启用的技能（`?scene=general|marketing`，缺省 general，MKT-AC-15）。
// 响应只增不减：创造页（不带 scene）拿到的仍是原来那些字段与技能。
func (h *Handler) skills(w http.ResponseWriter, r *http.Request) {
	scene, err := app.ParseSkillScene(r.URL.Query().Get("scene"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	rows, err := h.svc.SkillsInScene(r.Context(), scene)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	type modelRef struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	type bindings struct {
		Video     *modelRef `json:"video,omitempty"`
		Image     *modelRef `json:"image,omitempty"`
		Reasoning *modelRef `json:"reasoning,omitempty"`
	}
	ref := func(m *app.SkillModelRef) *modelRef {
		if m == nil {
			return nil
		}
		return &modelRef{Code: m.Code, Name: m.Name}
	}
	type skillResp struct {
		Code          string          `json:"code"`
		Name          string          `json:"name"`
		Category      string          `json:"category"`
		Description   string          `json:"description"`
		Tags          json.RawMessage `json:"tags"`
		Steps         json.RawMessage `json:"steps"`
		CoverMediaID  *string         `json:"coverMediaId"`
		IsQuickEntry  bool            `json:"isQuickEntry"`
		Sort          int             `json:"sort"`
		Scenes        json.RawMessage `json:"scenes"`
		InputSlots    json.RawMessage `json:"inputSlots"`
		PrefillText   string          `json:"prefillText"`
		IsBatch       bool            `json:"isBatch"`
		ModelBindings bindings        `json:"modelBindings"`
	}
	items := make([]skillResp, 0, len(rows))
	for _, s := range rows {
		items = append(items, skillResp{
			Code: s.Code, Name: s.Name, Category: s.Category, Description: s.Description,
			Tags: rawOr(s.Tags, "[]"), Steps: rawOr(s.Steps, "[]"), CoverMediaID: optID(s.CoverMediaID),
			IsQuickEntry: s.IsQuickEntry, Sort: s.Sort,
			Scenes: rawOr(s.Scenes, "[]"), InputSlots: rawOr(s.InputSlots, "[]"), PrefillText: s.PrefillText,
			IsBatch:       s.IsBatch,
			ModelBindings: bindings{Video: ref(s.Video), Image: ref(s.Image), Reasoning: ref(s.Reasoning)},
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
}

// skillCover 公开取技能封面（MKD-19）：技能启用才出图，再交给媒体域 ServePublic
// （`Cache-Control: public, max-age=300` 由它设置 —— 缓存期即「撤不下来」的窗口）。
// 宽度默认 320（预览卡用缩略图），只接受媒体域的固定档位（那边会再校验一次）。
func (h *Handler) skillCover(w http.ResponseWriter, r *http.Request) {
	mediaID, err := h.svc.SkillCoverMedia(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if h.media == nil {
		httpx.Fail(w, r, httpx.Errf(errcode.Internal, "媒体服务未接入"))
		return
	}
	width := 320
	if v := r.URL.Query().Get("w"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "w 非法"))
			return
		}
		width = n
	}
	h.media.RedirectPublic(w, r, mediaID, width)
}

func (h *Handler) getPrefs(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.UserPrefs(r.Context(), h.userIDs(r))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"prefs": rawOr(p, "{}")})
}

func (h *Handler) putPrefs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prefs json.RawMessage `json:"prefs"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Prefs) == 0 {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少 prefs"))
		return
	}
	p, err := h.svc.SaveUserPrefs(r.Context(), h.userIDs(r), req.Prefs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"prefs": p})
}
