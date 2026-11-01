package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/marketing/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 生成角色（MKT-T-35；02 MK-26、MKT-F-106~108；dev/05 MKD-28、§4.3）。
//
// ⚠️ 本域不 import 生成 / 目录域（MKD-1）：报价与受理经 CharacterImageGenerator、画风经 StyleLookup，
// 由 internal/wiring 适配到 generation.Service（Preview / Accept）与 catalog。
// ⚠️ 本服务**不负责权限**：dryRun 要 view、真生成要 generate，由 transport 在调用之前判（CD-59）。
// ⚠️ 不建项目、不自动写回角色：候选图要用户挑（dev/05 §4.3），这里只回 generationId。

// CharacterGenActor 是付费主体、会员等级与操作者。
//
// ⚠️ 与 `POST /generations` **同一套解析**：工作区与付费主体来自工作区闸的上下文、会员等级来自
// cmd/api 里那唯一一个 memberLevelOf、操作者来自登录态 —— 由 transport 取好传进来，这里不另解析。
type CharacterGenActor struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	MemberLevelID  int64
}

// CharacterGenAsk 是交给生成端口的一次请求（操作已按参考图张数选好）。
type CharacterGenAsk struct {
	Actor             CharacterGenActor
	ModelCode         string
	OperationCode     string
	Description       string
	StyleCode         string
	ReferenceMediaIDs []int64
	Count             int
	// IdempotencyKey 是**已加前缀**的受理幂等键（`mkchar:` + 请求幂等键）；dryRun 为空。
	IdempotencyKey string
}

// CharacterGenQuote 是 dryRun 的结论。Blocked 时 Credits 为 0，原因与 `/generations/estimate` 给的同一句。
type CharacterGenQuote struct {
	Credits       int
	Unit          string
	Blocked       bool
	BlockedReason string
	// UserFixable 用户自己能解决（减参考图 / 升级会员 / 改描述长度）；其余要运营处理。按业务码分类，不匹配文案。
	UserFixable bool
}

// CharacterGenAccepted 是真生成的受理结果。
type CharacterGenAccepted struct {
	GenerationID int64
	Credits      int
	// Replayed 同一个幂等键已经受理过（没有再扣费）。
	Replayed bool
}

// CharacterImageGenerator 是生成角色的报价与受理（wiring 适配到 generation.Service.Preview / Accept）。
//
// ⚠️ Quote 与 Accept 必须走**同一条**输入解析 → 报价路径：两条的话「报 12 积分、扣 16」迟早出现。
type CharacterImageGenerator interface {
	Quote(ctx context.Context, ask CharacterGenAsk) (CharacterGenQuote, error)
	Accept(ctx context.Context, ask CharacterGenAsk) (CharacterGenAccepted, error)
}

// StyleLookup 判断画风素材码是否存在且启用（目录域实现）。
type StyleLookup interface {
	StyleExists(ctx context.Context, code string) (bool, error)
}

// CharacterGenInput 是一次「生成角色」请求（transport 解析后的）。
type CharacterGenInput struct {
	ModelCode         string
	Description       string
	StyleCode         string
	ReferenceMediaIDs []int64
	Count             int
	DryRun            bool
	// IdempotencyKey 是请求头里的原始幂等键（真生成必填）。
	IdempotencyKey string
}

// CharacterGenResult 是 dryRun 或真生成的结果（二者只有一个有值）。
type CharacterGenResult struct {
	Operation string
	Quote     *CharacterGenQuote
	Accepted  *CharacterGenAccepted
}

// CharacterGenIdemPrefix 是受理幂等键的前缀：与同一付费主体在别处用过的同名键隔开
// （uk_generation_idem 是 (payer, key)，不加前缀的话前端恰好复用一个键就会重放成别的任务）。
const CharacterGenIdemPrefix = "mkchar:"

// CharacterGen 是生成角色服务。
type CharacterGen struct {
	gen    CharacterImageGenerator
	styles StyleLookup
	media  WorkspaceMediaLookup
}

// NewCharacterGen 构造。三个端口都必填。
func NewCharacterGen(gen CharacterImageGenerator, styles StyleLookup, media WorkspaceMediaLookup) *CharacterGen {
	return &CharacterGen{gen: gen, styles: styles, media: media}
}

// Generate 报价（dryRun）或受理一次生成角色。
//
// 顺序：字段校验 → 参考图按工作区校验 → 画风存在性 → 报价 / 受理。前三步都在扣费之前且不调上游。
func (c *CharacterGen) Generate(ctx context.Context, a CharacterGenActor, in CharacterGenInput) (
	CharacterGenResult, error) {
	if c == nil || c.gen == nil || c.styles == nil || c.media == nil {
		return CharacterGenResult{}, httpx.Errf(errcode.Internal, "生成角色链路未接入")
	}
	req := domain.CharacterGenRequest{
		ModelCode: strings.TrimSpace(in.ModelCode), Description: strings.TrimSpace(in.Description),
		StyleCode: strings.TrimSpace(in.StyleCode), ReferenceMediaIDs: in.ReferenceMediaIDs, Count: in.Count,
	}
	issues := domain.ValidateGenRequest(req)
	if !domain.HasError(issues) {
		mi, err := c.referenceIssues(ctx, a.WorkspaceID, req.ReferenceMediaIDs)
		if err != nil {
			return CharacterGenResult{}, err
		}
		issues = append(issues, mi...)
	}
	if !domain.HasError(issues) && req.StyleCode != "" {
		ok, err := c.styles.StyleExists(ctx, req.StyleCode)
		if err != nil {
			return CharacterGenResult{}, httpx.Wrap(errcode.Internal, err)
		}
		if !ok {
			issues = append(issues, errIssue("styleCode", domain.IssueBadValue,
				fmt.Sprintf("画风「%s」不存在或已下线，请重新选择", req.StyleCode)))
		}
	}
	if domain.HasError(issues) {
		return CharacterGenResult{}, issuesErr(errcode.InvalidArgument, "生成角色的参数有问题", issues, nil)
	}

	op := domain.CharacterGenOperation(len(req.ReferenceMediaIDs))
	ask := CharacterGenAsk{Actor: a, ModelCode: req.ModelCode, OperationCode: op,
		Description: req.Description, StyleCode: req.StyleCode,
		ReferenceMediaIDs: req.ReferenceMediaIDs, Count: req.Count}

	if in.DryRun {
		q, err := c.gen.Quote(ctx, ask)
		if err != nil {
			return CharacterGenResult{}, err
		}
		return CharacterGenResult{Operation: op, Quote: &q}, nil
	}

	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		return CharacterGenResult{}, httpx.Errf(errcode.MissingIdempotencyKey, "生成角色必须带 Idempotency-Key")
	}
	ask.IdempotencyKey = CharacterGenIdemPrefix + key
	res, err := c.gen.Accept(ctx, ask)
	if err != nil {
		return CharacterGenResult{}, err
	}
	return CharacterGenResult{Operation: op, Accepted: &res}, nil
}

// referenceIssues 逐张校验参考图：存在、属于**本工作区**（不收平台素材 —— 预置角色的图要先 adopt，
// dev/05 MKD-30）、格式在 png / jpeg / webp 白名单里（与角色图片同一份，MKD-35）。
//
// ⚠️ 受理的输入解析还会再按工作区判一次；这里先判是为了**点名是第几张**，且挡住平台素材 ——
// 媒体域的按工作区查询会把 workspace_id = 0 的素材也查出来（见 WorkspaceMediaLookup 的注释）。
func (c *CharacterGen) referenceIssues(ctx context.Context, ws int64, ids []int64) ([]domain.Issue, error) {
	var out []domain.Issue
	for i, id := range ids {
		field := fmt.Sprintf("referenceMediaIds[%d]", i)
		m, found, err := c.media.LookupWorkspaceMedia(ctx, ws, id)
		if err != nil {
			return nil, httpx.Wrap(errcode.Internal, err)
		}
		if !found || m.WorkspaceID != ws {
			out = append(out, errIssue(field, domain.IssueBadValue,
				fmt.Sprintf("第 %d 张参考图不存在或不属于当前工作区", i+1)))
			continue
		}
		if !domain.AllowedImageMime(m.ContentType) {
			out = append(out, errIssue(field, domain.IssueBadFormat,
				fmt.Sprintf("第 %d 张参考图格式不支持（只收 PNG / JPEG / WebP）", i+1)))
		}
	}
	return out, nil
}
