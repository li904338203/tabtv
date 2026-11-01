// Package app 编排脚本用例。
//
// ⚠️ 这个域与生成域**走的是两条链**（06 §4.1）：
// 分镜与提示词合成产出的是结构化文本，走文本模型**同步**调用，
// 费用记在 `script_llm_call`；资产图、分镜图、视频才走 generation 队列。
// 两条链在日志、幂等与退款上要能分别追踪。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// TextModel 由 Provider 域实现：一次同步的文本模型调用。
//
// ⚠️ **只有这一个入口**。老系统同步与异步各写了一套，流式开关、错误处理、
// 超时语义三处都不一致，同一个上游故障在两条路上表现完全不同。
type TextModel interface {
	// Complete 调一次文本模型，返回文本产出。
	Complete(ctx context.Context, in TextRequest) (string, error)
}

// TextRequest 是一次文本模型调用。
type TextRequest struct {
	ModelCode string
	// System 与 User 分开：系统提示词来自 prompt_template（可被运营调优），
	// 用户内容来自脚本。合成一段发出去的话，运营调词会把用户内容也冲掉。
	System string
	User   string
}

// Quoter 由目录域实现：同步 LLM 调用的报价与模型信息。
type Quoter interface {
	QuoteText(ctx context.Context, in TextQuoteAsk) (TextQuote, error)
}

type TextQuoteAsk struct {
	ModelCode     string
	OperationCode string
	Text          string
	MemberLevelID int64
}

type TextQuote struct {
	Credits int
	ModelID int64
	// MaxInputChars 为 0 表示不限。⚠️ 按**字符**不按 token。
	MaxInputChars int
}

// Ledger 由账本域实现：同步调用的预扣与退款。
//
// ⚠️ 这里**没有 settle**：新账本是「预扣即消费」——reserve 时就真实扣减，
// 成功不需要再做什么，失败才退。所谓"结算晚于校验"在这套语义下就是
// **退款要在解析失败时发生**，而不是"先别扣、等成功再扣"。
type Ledger interface {
	Consume(ctx context.Context, payerID, userID int64, amount int,
		refType, refID, reason string) error
	Refund(ctx context.Context, payerID int64, refType, refID, reason string) error
}

// PromptSource 由目录域实现：取系统提示词（模型定制 > 通用 > 内置兜底）**并渲染**。
//
// ⚠️ 端口给的是 Render 不是 Resolve：模板里写的是 `{{story}}` 这种占位符，
// 取原文直接发出去的话，模型收到的就是字面量 `{{story}}` —— 而这不报错，
// 模型会照着那段乱码生成，用户只看到"质量莫名其妙地差"。
// 目录域自己就有 RenderPrompt，之前只是没接上。
type PromptSource interface {
	RenderPrompt(ctx context.Context, code, modelCode string,
		vars map[string]string) (string, error)
}

// ProjectGuard 由画布域实现：校验项目归属。
type ProjectGuard interface {
	EnsureProject(ctx context.Context, workspaceID, projectID int64) error
}

type Service struct {
	repo    *repo.Repo
	nextID  func() int64
	model   TextModel
	quoter  Quoter
	ledger  Ledger
	prompts PromptSource
	guard   ProjectGuard
	assets  AssetGenerator
	balance BalanceReader
	// groupGen / canceller 是组任务用的端口（P07-04）。
	groupGen  GroupGenerator
	canceller GenerationCanceller
	log       *slog.Logger
}

func New(r *repo.Repo, nextID func() int64) *Service {
	return &Service{repo: r, nextID: nextID}
}

func (s *Service) SetTextModel(m TextModel)       { s.model = m }
func (s *Service) SetQuoter(q Quoter)             { s.quoter = q }
func (s *Service) SetLedger(l Ledger)             { s.ledger = l }
func (s *Service) SetPrompts(p PromptSource)      { s.prompts = p }
func (s *Service) SetProjectGuard(g ProjectGuard) { s.guard = g }
func (s *Service) SetLogger(l *slog.Logger)       { s.log = l }

func (s *Service) warn(ctx context.Context, msg string, args ...any) {
	if s.log != nil {
		s.log.WarnContext(ctx, msg, args...)
	}
}

// CreateInput 是一次脚本创建。
type CreateInput struct {
	WorkspaceID  int64
	ProjectID    int64
	CanvasNodeID string
	SourceType   string
	RawInput     string
	GlobalStyle  string
}

// Create 建脚本；节点上已有脚本时**返回那一个**而不是报错。
//
// ⚠️ 这是 AV-05 的「按节点恢复已有脚本」：用户重新打开节点时前端会再调
// 一次创建。报 409 的话前端得再发一次查询，而中间那一刻用户看到的是
// 一个失败提示 —— 而他什么都没做错。
func (s *Service) Create(ctx context.Context, in CreateInput) (Detail, error) {
	if err := domain.ValidateSourceType(domain.SourceType(in.SourceType)); err != nil {
		return Detail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	// ⚠️ 脚本与画布节点一一对应（项目 + 节点唯一，重复即恢复）。空节点 ID 也能写进去的话，
	//    同一项目里**所有**漏传它的脚本节点都会落到同一条脚本上 —— 移植过来的前端照老契约发 `nodeId`，
	//    建出来的就是这种（201 成功、而第二个脚本节点会看到第一个的镜头），所以在入口拒绝。
	if strings.TrimSpace(in.CanvasNodeID) == "" {
		return Detail{}, httpx.Errf(errcode.InvalidArgument, "canvasNodeId 必填：脚本要挂在一个画布节点上")
	}
	if s.guard != nil {
		if err := s.guard.EnsureProject(ctx, in.WorkspaceID, in.ProjectID); err != nil {
			return Detail{}, err
		}
	}
	sc := repo.Script{
		ID: s.nextID(), WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID,
		CanvasNodeID: in.CanvasNodeID, SourceType: in.SourceType,
		CurrentStep: int(domain.StepShots), Status: "editing",
		RawInput: in.RawInput, GlobalStyle: in.GlobalStyle,
	}
	err := s.repo.Insert(ctx, sc)
	switch {
	case errors.Is(err, repo.ErrDuplicate):
		existing, err := s.repo.GetByNode(ctx, in.WorkspaceID, in.ProjectID, in.CanvasNodeID)
		if err != nil {
			return Detail{}, httpx.Wrap(errcode.Internal, err)
		}
		return s.detail(ctx, existing)
	case err != nil:
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	// ⚠️ 必须回读一次，不能直接用刚才那个本地结构体：`version` 与 `status`
	// 的真值来自**库的列默认值**，本地结构体里是零值。直接返回的话，
	// 客户端拿到 version=0，紧接着的第一次保存必然撞乐观锁 ——
	// 而他什么都没改错。
	created, err := s.repo.Get(ctx, in.WorkspaceID, sc.ID)
	if err != nil {
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.detail(ctx, created)
}

// Detail 是脚本详情（含镜头表）。
type Detail struct {
	Script repo.Script
	// Shots 的两个提示词状态是**算出来的有效状态**（与 Prompts 同一算法），不是库里那一列。
	Shots []repo.Shot
	// AssetIDs 每个镜头引用的资产，按参考图锚定顺序（shot_asset_ref.sort）。
	AssetIDs map[int64][]int64
}

// detail 组装脚本详情。
//
// ⚠️ 提示词状态必须与 `GET /prompts` 是**同一个答案**（CD-77）：库里那一列只是改动时的即时标记，
// 「改了又改回原样」之后它还停在 stale，而按指纹算已经是 ready —— 详情若直接回那一列，
// 工作台与画布节点会让用户为一段没过期的词再花一次钱。
// ⚠️ 引用关系也由这里给：只有服务端知道参考图按什么顺序锚定，前端照名字去猜会与它对不上。
func (s *Service) detail(ctx context.Context, sc repo.Script) (Detail, error) {
	shots, err := s.repo.ListShots(ctx, sc.ID)
	if err != nil {
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	refs, err := s.repo.ListShotRefs(ctx, sc.ID)
	if err != nil {
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	assets, err := s.repo.ListAssetsByScript(ctx, sc.ID)
	if err != nil {
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	ups := buildUpstreams(sc, shots, refs, assets)
	ids := make(map[int64][]int64, len(shots))
	for i := range shots {
		sh := &shots[i]
		fp := ups[sh.ID].Fingerprint()
		sh.ImagePromptStatus = string(domain.EffectiveStatus(
			domain.PromptStatus(sh.ImagePromptStatus), sh.ImagePromptFingerprint, fp))
		sh.VideoPromptStatus = string(domain.EffectiveStatus(
			domain.PromptStatus(sh.VideoPromptStatus), sh.VideoPromptFingerprint, fp))
		list := make([]int64, 0, len(refs[sh.ID]))
		for _, ref := range refs[sh.ID] {
			list = append(list, ref.AssetID)
		}
		ids[sh.ID] = list
	}
	return Detail{Script: sc, Shots: shots, AssetIDs: ids}, nil
}

func (s *Service) Get(ctx context.Context, workspaceID, id int64) (Detail, error) {
	sc, err := s.repo.Get(ctx, workspaceID, id)
	if err != nil {
		return Detail{}, translate(err)
	}
	return s.detail(ctx, sc)
}

// GetByNode 按画布节点打开脚本。
func (s *Service) GetByNode(ctx context.Context, workspaceID, projectID int64,
	nodeID string) (Detail, error) {
	sc, err := s.repo.GetByNode(ctx, workspaceID, projectID, nodeID)
	if err != nil {
		return Detail{}, translate(err)
	}
	return s.detail(ctx, sc)
}

// PatchInput 是脚本元信息更新。
type PatchInput struct {
	WorkspaceID     int64
	ScriptID        int64
	ExpectedVersion int
	RawInput        *string
	GlobalStyle     *string
	CurrentStep     *int
}

// Patch 更新脚本元信息。
func (s *Service) Patch(ctx context.Context, in PatchInput) (Detail, error) {
	if in.CurrentStep != nil {
		// ⚠️ 步骤推进在**服务端独立校验**（AV-18）：前端也会挡一道，
		// 但那是体验；直接打 API 绕过前端是最容易发生的事。
		count, err := s.repo.CountShots(ctx, in.ScriptID)
		if err != nil {
			return Detail{}, httpx.Wrap(errcode.Internal, err)
		}
		// ⚠️ 资产就绪是**真查一次**，不是恒为 true：进提示词步之前
		// 必须确认参考图都有了 —— 没有参考图的提示词合成出来的词，
		// 下游拿去出图时没有任何锚定，等于白花一次钱。
		ready := true
		if domain.Step(*in.CurrentStep) == domain.StepPrompts {
			var err error
			if ready, err = s.AssetsReady(ctx, in.ScriptID); err != nil {
				return Detail{}, httpx.Wrap(errcode.Internal, err)
			}
		}
		if err := domain.CanAdvanceTo(domain.Step(*in.CurrentStep), count, ready); err != nil {
			return Detail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
	}
	sc, err := s.repo.Patch(ctx, repo.PatchInput{
		ID: in.ScriptID, WorkspaceID: in.WorkspaceID, ExpectedVersion: in.ExpectedVersion,
		RawInput: in.RawInput, GlobalStyle: in.GlobalStyle, CurrentStep: in.CurrentStep,
	})
	if err != nil {
		return Detail{}, translate(err)
	}
	// ⚠️ 全片风格是所有提示词的上游：改了它，整个脚本的提示词都该变灰。
	if in.GlobalStyle != nil {
		if err := s.repo.MarkScriptPromptsStale(ctx, in.ScriptID); err != nil {
			s.warn(ctx, "标记提示词过期失败", "scriptId", in.ScriptID, "error", err)
		}
	}
	return s.detail(ctx, sc)
}

// Calls 列出同步 LLM 调用记录（逐项费用可对账）。
func (s *Service) Calls(ctx context.Context, workspaceID, scriptID int64) ([]repo.LLMCall, error) {
	if _, err := s.repo.Get(ctx, workspaceID, scriptID); err != nil {
		return nil, translate(err)
	}
	list, err := s.repo.ListCalls(ctx, scriptID, 50)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

func translate(err error) error {
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Err(errcode.NotFound)
	case errors.Is(err, repo.ErrVersionConflict):
		// ⚠️ 错误要**指出冲突对象**（06 §5.2），前端据此决定刷新还是放弃。
		return httpx.Errf(errcode.VersionConflict, "脚本已被他人修改，请刷新后重试")
	case errors.Is(err, repo.ErrCallDuplicate):
		return httpx.Errf(errcode.Conflict, "这次生成已经在进行中")
	case err != nil:
		return httpx.Wrap(errcode.Internal, err)
	}
	return nil
}

func errorsIsNotFound(err error) bool { return errors.Is(err, repo.ErrNotFound) }

func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
