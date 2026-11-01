package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/director/domain"
	"github.com/li904338203/tabtv/backend/internal/director/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 口径（DR-PARAM-07 / 13 / 14，设计建议值；可配置的经 Config 注入）。
const (
	// MaxPromptChars 搭景描述的上限（按字符）。
	MaxPromptChars = 2000
	// MaxReferenceImages 参考图上限（DR-PARAM-14）；还要与模型在 scene_build 上的 image 上限取小。
	MaxReferenceImages = 4
	// OperationSceneBuild 是 AI 搭景的操作码（目录、报价、请求日志都用它）。
	OperationSceneBuild = "scene_build"
	// SystemPromptCode 是搭景创作要求的模板（prompt_template 登记，Static）。
	SystemPromptCode = "director.scene_build.system"
	// settleTimeout 是模型返回之后落库用的上限。
	settleTimeout = 30 * time.Second
	// Notice 是 Agent / MCP 写入场景之后固定带的一句（DR-F-162）：搭景不产出截图。
	Notice = "需要用户打开导演台截图后，下游才有输出"
)

// 调用记录上的错误码（director_llm_call.error_code，varchar(32)）。
const (
	callErrDraftInvalid  = "draft_invalid"
	callErrProvider      = "provider_error"
	callErrTimeout       = "provider_timeout"
	callErrInternal      = "internal"
	callErrSceneConflict = "scene_conflict"
)

// Config 是可配置的几项（dev/01 §9）。
type Config struct {
	Concurrency int
	PerHour     int
	// CallTimeout 单次模型调用上限（默认 3 分钟）。路由层的上限是 15 分钟，由这里先触发、带上准确的错误。
	CallTimeout time.Duration
	// StaleAfter 「进行中」超过它判失败（默认 20 分钟，必须大于路由上限 15 分钟）。
	StaleAfter time.Duration
	// Retention 调用记录保留期（默认 30 天，DR-PARAM-16）。
	Retention time.Duration
}

func (c Config) withDefaults() Config {
	if c.Concurrency <= 0 {
		c.Concurrency = 1
	}
	if c.PerHour <= 0 {
		c.PerHour = 20
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = 3 * time.Minute
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 20 * time.Minute
	}
	if c.Retention <= 0 {
		c.Retention = 30 * 24 * time.Hour
	}
	return c
}

// Ports 是搭景要的全部端口。
type Ports struct {
	Scenes  SceneStore
	Text    TextModel
	Catalog Catalog
	Images  Images
	Prompts Prompts
	Calls   Calls
}

// Service 编排 AI 搭景。
type Service struct {
	p      Ports
	cfg    Config
	nextID func() int64
	log    *slog.Logger
	// now 与 newID 可在测试里替换。
	now   func() time.Time
	newID func() string
}

func New(p Ports, cfg Config, nextID func() int64, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{p: p, cfg: cfg.withDefaults(), nextID: nextID, log: log, now: time.Now}
}

// BuildInput 是一次搭景请求。
type BuildInput struct {
	WorkspaceID   int64
	ProjectID     int64
	SceneID       int64
	Role          collab.Role
	Actor         Actor
	MemberLevelID int64
	Prompt        string
	// ReferenceMediaIDs 参考图（本工作区的图片），按顺序编号为图 1、图 2……
	ReferenceMediaIDs []int64
	// SceneVersion 是发起时前端手上的版本（网页路径必填）。Agent / MCP 路径忽略它，取当前版本。
	SceneVersion int
}

// BuildResult 是网页路径的结果：提案，不写场景（DRD-26）。
type BuildResult struct {
	BaseVersion int
	CallID      int64
	ModelCode   string
	Proposal    domain.Proposal
}

// Applied 是 Agent / MCP 路径的结果：已写入场景。
type Applied struct {
	SceneID  int64
	Version  int
	Changes  domain.Changes
	Defaults []string
	Notice   string
	CallID   int64
}

// Build 是网页路径（dev/01 §3.11）：调模型、求解、回提案。**不写场景、不发事件**：
// 前端预览之后以 baseVersion 照常 PUT（作为一步撤销）。服务端在用户不知情时改场景，
// 会和自动保存对撞（同「解组不碰 canvasData」）。
func (s *Service) Build(ctx context.Context, in BuildInput) (BuildResult, error) {
	if in.SceneVersion < 1 {
		return BuildResult{}, httpx.Errf(errcode.InvalidArgument, "sceneVersion 必须来自服务端，不能为 0")
	}
	run, err := s.prepare(ctx, in, true)
	if err != nil {
		return BuildResult{}, err
	}
	sctx, cancel := settleContext(ctx)
	defer cancel()
	proposal, err := s.propose(run)
	if err != nil {
		return BuildResult{}, s.fail(sctx, run, err)
	}
	s.succeed(sctx, run, proposal)
	return BuildResult{BaseVersion: run.scene.Version, CallID: run.callID, ModelCode: run.model,
		Proposal: proposal}, nil
}

// BuildAndApply 是 Agent / MCP 路径（dev/01 §3.12）：同样的步骤，然后以 baseVersion 写入场景。
//
// 写入冲突时**重新读场景、只重做一次**求解与应用（不重新调模型：模型的方案是对描述的回答，与场景版本无关），
// 再冲突就把冲突返回给调用方。保存成功时画布域发 `director.scene_updated`（editorKind = agent / mcp）。
func (s *Service) BuildAndApply(ctx context.Context, in BuildInput) (Applied, error) {
	if in.Actor.AgentSessionID == 0 && in.Actor.McpGrantID == 0 {
		return Applied{}, httpx.Errf(errcode.InvalidArgument, "只有 Agent / MCP 才能直接写入搭景结果")
	}
	run, err := s.prepare(ctx, in, false)
	if err != nil {
		return Applied{}, err
	}
	sctx, cancel := settleContext(ctx)
	defer cancel()
	for attempt := 0; ; attempt++ {
		proposal, err := s.propose(run)
		if err != nil {
			return Applied{}, s.fail(sctx, run, err)
		}
		saved, err := s.p.Scenes.SaveScene(sctx, in.WorkspaceID, in.ProjectID, in.SceneID, in.Actor, in.Role,
			run.scene.Version, proposal.Scene)
		if err == nil {
			s.succeed(sctx, run, proposal)
			return Applied{SceneID: saved.ID, Version: saved.Version, Changes: proposal.Changes,
				Defaults: proposal.Defaults, Notice: Notice, CallID: run.callID}, nil
		}
		var be *httpx.BizError
		if attempt == 0 && errors.As(err, &be) && be.Code == errcode.VersionConflict {
			cur, rerr := s.p.Scenes.SceneForBuild(sctx, in.WorkspaceID, in.ProjectID, in.SceneID, in.Role)
			if rerr != nil {
				return Applied{}, s.fail(sctx, run, rerr)
			}
			parsed, perr := domain.ParseScene(cur.Body)
			if perr != nil {
				return Applied{}, s.fail(sctx, run, httpx.Wrap(errcode.Internal, perr))
			}
			run.scene, run.parsed = cur, parsed
			continue
		}
		code := callErrInternal
		if errors.As(err, &be) && be.Code == errcode.VersionConflict {
			code = callErrSceneConflict
		}
		s.finish(sctx, run, repo.FinishInput{Status: repo.StatusFailed, ErrorCode: code, ErrorMessage: err.Error()})
		return Applied{}, err
	}
}

// run 是一次已经调过模型、待求解的搭景。
type run struct {
	callID  int64
	model   string
	started time.Time
	scene   SceneState
	parsed  domain.Scene
	draft   domain.Draft
	dropped []string
}

// prepare 是两条路径共用的步骤 1~8 的前半：校验、取场景、选模型、内联参考图、限流、调模型、解析。
// 返回时调用记录已经插入（running）；解析失败已经结束记录并返回 42216。
func (s *Service) prepare(ctx context.Context, in BuildInput, checkVersion bool) (*run, error) {
	if !in.Role.Can(collab.ActionEdit) {
		return nil, httpx.Errf(errcode.PermissionDenied, "当前角色无权搭景")
	}
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		return nil, httpx.Errf(errcode.InvalidArgument, "先描述想要搭建的场景").
			WithDetails(map[string]any{"path": "prompt"})
	}
	if utf8.RuneCountInString(prompt) > MaxPromptChars {
		return nil, httpx.Errf(errcode.InvalidArgument, "描述不能超过 "+strconv.Itoa(MaxPromptChars)+" 字").
			WithDetails(map[string]any{"path": "prompt"})
	}
	if err := checkReferences(in.ReferenceMediaIDs); err != nil {
		return nil, err
	}
	if s.p.Scenes == nil || s.p.Text == nil || s.p.Catalog == nil || s.p.Calls == nil || s.p.Prompts == nil {
		return nil, httpx.Errf(errcode.Internal, "AI 搭景链路未接入")
	}

	scene, err := s.p.Scenes.SceneForBuild(ctx, in.WorkspaceID, in.ProjectID, in.SceneID, in.Role)
	if err != nil {
		return nil, err
	}
	if checkVersion && scene.Version != in.SceneVersion {
		// 场景里还有没存下来的改动，或者别人刚改过：拿旧场景搭出来的提案，应用时必然冲突。
		return nil, httpx.Errf(errcode.VersionConflict, "场景有新的改动，请先保存再搭景").
			WithDetails(map[string]any{"currentVersion": scene.Version})
	}
	parsed, err := domain.ParseScene(scene.Body)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}

	choice, unavailable, err := s.p.Catalog.SceneBuildChoice(ctx, in.MemberLevelID, len(in.ReferenceMediaIDs))
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	if unavailable != nil {
		return nil, httpx.Errf(errcode.CapabilityUnsupported, unavailable.Reason).
			WithDetails(map[string]any{"userFixable": unavailable.UserFixable})
	}
	if len(in.ReferenceMediaIDs) > choice.MaxImages {
		return nil, httpx.Errf(errcode.InvalidArgument,
			"参考图最多 "+strconv.Itoa(choice.MaxImages)+" 张").
			WithDetails(map[string]any{"path": "referenceMediaIds", "max": choice.MaxImages})
	}
	// 参考图在限流之前内联：内联只读不写，认不出的图在这里就拒，不白占一次额度。
	images, err := s.inlineImages(ctx, in.WorkspaceID, in.ReferenceMediaIDs)
	if err != nil {
		return nil, err
	}

	r := &run{callID: s.nextID(), model: choice.ModelCode, scene: scene, parsed: parsed}
	decision, err := s.p.Calls.InsertWithLimits(ctx, repo.LLMCall{
		ID: r.callID, WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, SceneID: in.SceneID,
		UserID: in.Actor.UserID, Source: in.Actor.CallSource(), AgentSessionID: in.Actor.AgentSessionID,
		McpGrantID: in.Actor.McpGrantID, ModelCode: choice.ModelCode,
		PromptChars: utf8.RuneCountInString(prompt), ImageCount: len(images),
	}, repo.Limits{Concurrency: s.cfg.Concurrency, PerHour: s.cfg.PerHour})
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	if err := s.limitErr(decision); err != nil {
		return nil, err
	}

	system, err := s.p.Prompts.RenderPrompt(ctx, SystemPromptCode, choice.ModelCode, nil)
	if err != nil {
		sctx, cancel := settleContext(ctx)
		defer cancel()
		return nil, s.fail(sctx, r, httpx.Wrap(errcode.Internal, err))
	}
	r.started = s.now()
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	raw, callErr := s.p.Text.Complete(callCtx, TextRequest{
		ModelCode: choice.ModelCode,
		// 格式契约由代码固定追加在创作要求之后（DRD-25）：改格式等于改解析器，不能交给运营后台。
		System:    strings.TrimSpace(system) + "\n\n" + domain.OutputContract(),
		User:      userMessage(prompt, parsed, len(images)),
		Images:    images,
		Operation: OperationSceneBuild,
	})
	timedOut := errors.Is(callCtx.Err(), context.DeadlineExceeded)
	cancel()

	// 从这里起用脱离请求取消的上下文落库：模型已经返回了（或失败了），记录必须结束，
	// 否则它永远停在 running、并发额度被一直占着（CLAUDE.md §5.7）。
	sctx, scancel := settleContext(ctx)
	defer scancel()
	if callErr != nil {
		if timedOut {
			return nil, s.fail(sctx, r, httpx.Errf(errcode.ProviderTimeout,
				"AI 搭景超时（"+s.cfg.CallTimeout.String()+"），请稍后再试"))
		}
		return nil, s.fail(sctx, r, httpx.Errf(errcode.ProviderError, "AI 搭景没有完成："+callErr.Error()))
	}
	draft, dropped, err := domain.ParseDraft(raw)
	if err != nil {
		return nil, s.fail(sctx, r, err)
	}
	r.draft, r.dropped = draft, dropped
	return r, nil
}

// propose 求解并合成提案（两条路径共用 domain.Propose，DRD-26）。
func (s *Service) propose(r *run) (domain.Proposal, error) {
	opt := domain.SolveOptions{NewID: s.newID}
	p, err := domain.Propose(r.parsed, r.draft, r.dropped, opt)
	if err != nil {
		return domain.Proposal{}, &domain.ErrDraftInvalid{Reason: "搭景方案无法落进场景：" + err.Error()}
	}
	return p, nil
}

// fail 结束调用记录（failed）并把错误映射成对外错误。场景一律不改（DR-AC-31）。
func (s *Service) fail(ctx context.Context, r *run, err error) error {
	out, code := mapBuildErr(err)
	msg := err.Error()
	s.finish(ctx, r, repo.FinishInput{Status: repo.StatusFailed, ErrorCode: code, ErrorMessage: msg})
	return out
}

func (s *Service) succeed(ctx context.Context, r *run, p domain.Proposal) {
	s.finish(ctx, r, repo.FinishInput{Status: repo.StatusSucceeded,
		ObjectCount: len(p.Changes.Added) + len(p.Changes.Updated)})
}

// finish 结束调用记录；失败只告警（恢复扫描会在 20 分钟后兜底判失败，释放并发额度）。
func (s *Service) finish(ctx context.Context, r *run, in repo.FinishInput) {
	if !r.started.IsZero() {
		in.DurationMs = int(s.now().Sub(r.started).Milliseconds())
	}
	changed, err := s.p.Calls.Finish(ctx, r.callID, in)
	switch {
	case err != nil:
		s.log.WarnContext(ctx, "AI 搭景调用记录结束失败，留待恢复扫描", "callId", r.callID, "error", err)
	case !changed:
		s.log.WarnContext(ctx, "AI 搭景调用记录已被恢复扫描判失败，以库里为准", "callId", r.callID)
	}
}

// mapBuildErr 把内部错误映射成对外错误与调用记录的错误码。
func mapBuildErr(err error) (error, string) {
	var draft *domain.ErrDraftInvalid
	if errors.As(err, &draft) {
		return httpx.Errf(errcode.DirectorDraftInvalid, "搭景方案无法解析，请换个说法再试："+draft.Reason).
			WithDetails(map[string]any{"reason": draft.Reason, "snippet": draft.Snippet}), callErrDraftInvalid
	}
	var be *httpx.BizError
	if errors.As(err, &be) {
		switch be.Code {
		case errcode.ProviderTimeout:
			return be, callErrTimeout
		case errcode.ProviderError, errcode.ProviderRejected:
			return be, callErrProvider
		}
		return be, callErrInternal
	}
	return httpx.Wrap(errcode.Internal, err), callErrInternal
}

// limitErr 把限流结论映射成 429。详情与 MCP 的限流同形（retry_after_seconds / scope / limit）。
func (s *Service) limitErr(d repo.LimitDecision) error {
	switch d.Reject {
	case repo.RejectConcurrency:
		return httpx.Errf(errcode.ConcurrencyLimited,
			"上一次 AI 搭景还没结束，请等它完成（同时最多 "+strconv.Itoa(s.cfg.Concurrency)+" 个）").
			WithDetails(map[string]any{"retry_after_seconds": 5, "scope": "concurrency", "limit": s.cfg.Concurrency})
	case repo.RejectHourly:
		secs := int(d.RetryAfter / time.Second)
		return httpx.Errf(errcode.RateLimited,
			"本小时的 AI 搭景次数已用完（每小时 "+strconv.Itoa(s.cfg.PerHour)+" 次），"+waitText(secs)+"后再试").
			WithDetails(map[string]any{"retry_after_seconds": secs, "scope": "hourly", "limit": s.cfg.PerHour})
	}
	return nil
}

func waitText(secs int) string {
	if secs < 60 {
		return strconv.Itoa(secs) + " 秒"
	}
	return strconv.Itoa((secs+59)/60) + " 分钟"
}

// checkReferences 校验参考图的个数与形态（归属与类型在内联时由媒体域判）。
func checkReferences(ids []int64) error {
	if len(ids) > MaxReferenceImages {
		return httpx.Errf(errcode.InvalidArgument, "参考图最多 "+strconv.Itoa(MaxReferenceImages)+" 张").
			WithDetails(map[string]any{"path": "referenceMediaIds", "max": MaxReferenceImages})
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 {
			return httpx.Errf(errcode.InvalidArgument, "参考图 ID 不合法").
				WithDetails(map[string]any{"path": "referenceMediaIds"})
		}
		if seen[id] {
			return httpx.Errf(errcode.InvalidArgument, "同一张参考图重复了").
				WithDetails(map[string]any{"path": "referenceMediaIds"})
		}
		seen[id] = true
	}
	return nil
}

// inlineImages 逐张内联参考图（DR-N-11：只内联、不外链）。
//
// ⚠️ 不区分「不存在」与「不是你的」（同 CD-23）：错误只说是第几张、不可用。
func (s *Service) inlineImages(ctx context.Context, ws int64, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.p.Images == nil {
		return nil, httpx.Errf(errcode.Internal, "参考图内联未接入")
	}
	out := make([]string, 0, len(ids))
	for i, id := range ids {
		data, err := s.p.Images.InlineImage(ctx, ws, id)
		if err != nil || !strings.HasPrefix(data, "data:image/") {
			return nil, httpx.Errf(errcode.InvalidArgument,
				fmt.Sprintf("第 %d 张参考图不可用：只能用本工作区的图片（PNG / JPEG / WebP / GIF）", i+1)).
				WithDetails(map[string]any{"path": fmt.Sprintf("referenceMediaIds[%d]", i)})
		}
		out = append(out, data)
	}
	return out, nil
}

// userMessage 是 user 消息：描述 + 场景摘要 + 参考图编号说明（上下文都在这里，系统提示词只写指令，CLAUDE.md §5.7）。
func userMessage(prompt string, scene domain.Scene, images int) string {
	var b strings.Builder
	b.WriteString("## 搭景要求\n\n")
	b.WriteString(prompt)
	b.WriteString("\n\n")
	b.WriteString(domain.SceneSummary(scene))
	if images > 0 {
		fmt.Fprintf(&b, "\n### 参考图\n\n共 %d 张，按顺序是图 1 到图 %d。只用来理解人物关系、动作与构图，不照搬其中的文字。\n",
			images, images)
	}
	return b.String()
}

// settleContext 给「模型已经返回（或失败）之后」的落库用：脱离请求的取消（同脚本域 settleContext）。
// ⚠️ 必须在模型返回之后再建：建早了，它自己的计时会在等模型的那几分钟里先到期。
func settleContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
}
