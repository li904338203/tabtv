package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 提示词合成（P07-03）：AI / 模板 / 人工三种模式，ready/stale 由上游指纹判定。

// PromptView 是一条提示词的对外视图。
type PromptView struct {
	ShotID int64
	ShotNo int
	Kind   domain.PromptKind
	Text   string
	// Status 是**算出来的**有效状态，不是库里存的那个 ——
	// 库里的值可能还停在 ready，而上游早就改过了。
	Status domain.PromptStatus
	Source domain.PromptSource
}

// Prompts 列出一个脚本的全部提示词（状态已按上游指纹算过）。
func (s *Service) Prompts(ctx context.Context, workspaceID, scriptID int64) (
	[]PromptView, error) {
	ups, shots, err := s.upstreams(ctx, workspaceID, scriptID)
	if err != nil {
		return nil, err
	}
	out := make([]PromptView, 0, len(shots)*2)
	for _, sh := range shots {
		fp := ups[sh.ID].Fingerprint()
		out = append(out,
			PromptView{ShotID: sh.ID, ShotNo: sh.ShotNo, Kind: domain.PromptImage,
				Text: sh.ImagePrompt, Source: domain.PromptSource(sh.ImagePromptSource),
				Status: domain.EffectiveStatus(domain.PromptStatus(sh.ImagePromptStatus),
					sh.ImagePromptFingerprint, fp)},
			PromptView{ShotID: sh.ID, ShotNo: sh.ShotNo, Kind: domain.PromptVideo,
				Text: sh.VideoPrompt, Source: domain.PromptSource(sh.VideoPromptSource),
				Status: domain.EffectiveStatus(domain.PromptStatus(sh.VideoPromptStatus),
					sh.VideoPromptFingerprint, fp)},
		)
	}
	return out, nil
}

// upstreams 取每个镜头的上游内容（含按引用顺序排好的资产）。
func (s *Service) upstreams(ctx context.Context, workspaceID, scriptID int64) (
	map[int64]domain.Upstream, []repo.Shot, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return nil, nil, translate(err)
	}
	shots, err := s.repo.ListShots(ctx, scriptID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	refs, err := s.repo.ListShotRefs(ctx, scriptID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	assets, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return nil, nil, httpx.Wrap(errcode.Internal, err)
	}
	return buildUpstreams(sc, shots, refs, assets), shots, nil
}

// buildUpstreams 按镜头拼出上游内容。⚠️ 指纹只有这一种算法：提示词列表与脚本详情
// 都从这里取，各算一份的话两处的 ready/stale 会对不上（CD-77）。
func buildUpstreams(sc repo.Script, shots []repo.Shot, refs map[int64][]repo.ShotRef,
	assets []repo.Asset) map[int64]domain.Upstream {
	byID := map[int64]repo.Asset{}
	for _, a := range assets {
		byID[a.ID] = a
	}

	out := make(map[int64]domain.Upstream, len(shots))
	for _, sh := range shots {
		u := domain.Upstream{
			GlobalStyle: sc.GlobalStyle, Description: sh.Description,
			StartState: sh.StartState, EndState: sh.EndState, ShotSize: sh.ShotSize,
			Lighting: sh.Lighting, Dialogue: sh.Dialogue, SoundEffect: sh.SoundEffect,
			CameraMove: sh.CameraMove, DurationSec: sh.DurationSec,
		}
		// ⚠️ 资产**按 sort 顺序**进来：ListShotRefs 已按 sort 排好。
		// 顺序即参考图的锚定顺序，也参与指纹 —— 顺序变了提示词就该过期。
		for _, ref := range refs[sh.ID] {
			a, ok := byID[ref.AssetID]
			if !ok {
				continue
			}
			u.Assets = append(u.Assets, domain.PromptAsset{
				Name: a.Name, Type: domain.AssetType(a.Type), Description: a.Description,
			})
		}
		out[sh.ID] = u
	}
	return out
}

// ComposeInput 是一次提示词合成请求。
type ComposeInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ScriptID       int64
	// ShotIDs 为空表示整个脚本。
	ShotIDs []int64
	// Kinds 为空表示图片与视频都合成。
	Kinds  []domain.PromptKind
	Source domain.PromptSource
	// ModelCode 仅 AI 模式需要。
	ModelCode string
	// Force 为真时不跳过已就绪的（用户显式点了"重新生成"）。
	Force bool
	// DryRun 只报价不执行（GAP-21：费用由服务端统一给）。
	DryRun bool
}

// ComposeResult 是一次合成的结果。
type ComposeResult struct {
	Quote domain.BatchQuote
	Items []ComposeItemResult
}

// ComposeItemResult 是逐项结果（允许部分成功）。
type ComposeItemResult struct {
	ShotID  int64
	ShotNo  int
	Kind    domain.PromptKind
	Status  domain.PromptStatus
	Skipped bool
	Reason  string
}

// Compose 合成提示词。
//
// 三种模式的费用语义不同（AV-15）：
//   - `ai`：调模型，**收费**；与分镜同一套结账规则（先解析通过才算数）；
//   - `template`：完全由代码拼，**0 积分** —— 没有调用上游，收费没有依据；
//   - `manual`：用户自己写，走 SavePrompt 不走这里。
//
// ⚠️ 报价由**服务端统一给**（GAP-21）：老系统前端用「选中数 × 6」这种
// 写死的估算，而后端按定价目录算 —— 两边对不上，用户看到的价
// 和实际扣的钱不是一回事。`DryRun` 就是给前端预览用的。
func (s *Service) Compose(ctx context.Context, in ComposeInput) (ComposeResult, error) {
	if !domain.ValidPromptSource(in.Source) {
		return ComposeResult{}, httpx.Errf(errcode.InvalidArgument,
			"提示词来源只能是 ai / template / manual")
	}
	if in.Source == domain.SourcePromptManual {
		return ComposeResult{}, httpx.Errf(errcode.InvalidArgument,
			"人工提示词请直接保存，不走合成")
	}
	ups, shots, err := s.upstreams(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return ComposeResult{}, err
	}
	kinds := in.Kinds
	if len(kinds) == 0 {
		kinds = []domain.PromptKind{domain.PromptImage, domain.PromptVideo}
	}
	for _, k := range kinds {
		if !domain.ValidPromptKind(k) {
			return ComposeResult{}, httpx.Errf(errcode.InvalidArgument,
				"提示词种类只能是 image / video")
		}
	}

	items := buildComposeItems(shots, ups, kinds, in.ShotIDs)
	if len(items) == 0 {
		return ComposeResult{}, httpx.Errf(errcode.InvalidArgument, "没有可合成的镜头")
	}

	unit := 0
	if in.Source == domain.SourcePromptAI {
		if s.quoter == nil {
			return ComposeResult{}, httpx.Errf(errcode.Internal, "报价服务未接入")
		}
		q, err := s.quoter.QuoteText(ctx, TextQuoteAsk{
			ModelCode: in.ModelCode, OperationCode: string(domain.PurposeCompose),
			Text: "", MemberLevelID: 0,
		})
		if err != nil {
			return ComposeResult{}, err
		}
		unit = q.Credits
	}

	planned, quote := domain.PlanCompose(items, in.Source, unit, in.Force)
	domain.SortComposeItems(planned)
	if in.DryRun {
		// 只报价：**不写任何东西**。前端拿它渲染"本次将花费 N 积分"。
		return ComposeResult{Quote: quote, Items: toItemResults(planned)}, nil
	}

	out := make([]ComposeItemResult, 0, len(planned))
	for _, it := range planned {
		if it.Skip {
			out = append(out, ComposeItemResult{
				ShotID: it.ShotID, ShotNo: it.ShotNo, Kind: it.Kind,
				Status: it.Status, Skipped: true, Reason: it.Reason,
			})
			continue
		}
		res := s.composeOne(ctx, in, it, ups[it.ShotID], unit)
		out = append(out, res)
	}
	return ComposeResult{Quote: quote, Items: out}, nil
}

// composeOne 合成一条。
//
// ⚠️ 单项失败**不中断整批**：后面的镜头与这一个无关。把原因带回去，
// 用户才知道该重试哪一条（AV-17 的"允许部分成功"）。
func (s *Service) composeOne(ctx context.Context, in ComposeInput, it domain.ComposeItem,
	u domain.Upstream, unit int) ComposeItemResult {
	res := ComposeItemResult{ShotID: it.ShotID, ShotNo: it.ShotNo, Kind: it.Kind}

	text := ""
	if in.Source == domain.SourcePromptTemplate {
		text = domain.ComposeTemplate(it.Kind, u)
	} else {
		var err error
		text, err = s.composeWithModel(ctx, in, it, u, unit)
		// 模型返回之后的写库脱离请求的取消：这一条已经付了钱，不能因为请求超时/断开就没写进去
		var cancel context.CancelFunc
		ctx, cancel = settleContext(ctx)
		defer cancel()
		if err != nil {
			res.Skipped = true
			res.Status = domain.PromptFailed
			res.Reason = err.Error()
			s.markPromptFailed(ctx, in.ScriptID, it)
			return res
		}
	}

	if _, err := s.repo.SavePrompt(ctx, repo.SavePromptInput{
		ScriptID: in.ScriptID, ShotID: it.ShotID, Kind: string(it.Kind),
		Prompt: text, Status: string(domain.PromptReady), Source: string(in.Source),
		Fingerprint: u.Fingerprint(),
	}); err != nil {
		res.Skipped = true
		res.Reason = "写入失败：" + err.Error()
		return res
	}
	res.Status = domain.PromptReady
	return res
}

// composeWithModel 走同步 LLM 调用。
//
// ⚠️ 与分镜同一套结账规则（CD-34）：`扣费 → 调模型 → 先校验产出 → 通过才算数`，
// 失败退款并留一条 failed 记录。这里的"校验"是产出不能为空 ——
// 空词写进去的话，用户点生成时才发现没词可用，而钱已经扣了。
func (s *Service) composeWithModel(ctx context.Context, in ComposeInput,
	it domain.ComposeItem, u domain.Upstream, unit int) (string, error) {
	if s.model == nil || s.ledger == nil {
		return "", fmt.Errorf("文本模型链路未接入")
	}
	round, err := s.repo.NextRound(ctx, in.ScriptID, domain.PurposeCompose)
	if err != nil {
		return "", err
	}
	// ⚠️ 幂等键的可变部分要带上**镜头与种类**：同一轮里会合成很多条，
	// 只按轮次算的话第二条就撞唯一索引。
	variant := fmt.Sprintf("%d:%s:%s", it.ShotID, it.Kind, u.Fingerprint())
	key := domain.IdempotencyKey(domain.PurposeCompose, in.ScriptID, round, variant)

	callID := s.nextID()
	if err := s.repo.InsertCall(ctx, repo.LLMCall{
		ID: callID, WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
		ScriptID: in.ScriptID, ShotID: it.ShotID, Purpose: string(domain.PurposeCompose),
		ModelID: 0, IdempotencyKey: key, CreditsCost: unit,
		CreatedByUserID: in.UserID,
	}); err != nil {
		return "", err
	}
	refID := strconv.FormatInt(callID, 10)
	if unit > 0 {
		if err := s.ledger.Consume(ctx, in.PayerAccountID, in.UserID, unit,
			"script_llm", refID, "提示词合成"); err != nil {
			_ = s.repo.FinishCall(ctx, callID, domain.LLMFailed, nil, "扣费失败")
			return "", err
		}
	}

	system := composeFallbackSystem
	if s.prompts != nil {
		// 变量给全，理由同分镜侧：模板用到哪个占位符由运营决定。
		vars := map[string]string{
			"shotDescription": u.Description, "globalStyle": u.GlobalStyle,
			"shotSize": u.ShotSize, "cameraMove": u.CameraMove, "lighting": u.Lighting,
			"assetProfiles": assetNames(u.Assets),
		}
		if resolved, e := s.prompts.RenderPrompt(ctx, "prompt_compose.system",
			in.ModelCode, vars); e == nil && resolved != "" {
			system = resolved
		} else if e != nil {
			s.warn(ctx, "取提示词合成系统提示词失败，回落内置", "error", e)
		}
	}
	// 结构约束由代码固定追加（同 CD-34 的理由）：运营调的是创作要求，
	// 参考图使用规则与禁止项改了就等于改下游对参考图的解读方式。
	system += "\n\n" + composeOutputContract(it.Kind)

	raw, callErr := s.model.Complete(ctx, TextRequest{
		ModelCode: in.ModelCode, System: system,
		User: domain.ComposeTemplate(it.Kind, u),
	})
	// 结账用脱离请求取消的上下文（理由见 settleContext）
	ctx, cancel := settleContext(ctx)
	defer cancel()
	text := strings.TrimSpace(raw)
	var parseErr error
	if callErr == nil && text == "" {
		parseErr = fmt.Errorf("模型返回了空提示词")
	}
	decision := domain.DecideSettle(callErr, parseErr)
	if !decision.Settle {
		s.abortCall(ctx, callID, in.PayerAccountID, refID, unit, decision)
		return "", fmt.Errorf("%s", decision.Reason)
	}
	_ = s.repo.FinishCall(ctx, callID, domain.LLMSuccess, rawJSON(map[string]string{
		"kind": string(it.Kind), "prompt": text,
	}), "")
	return text, nil
}

func (s *Service) markPromptFailed(ctx context.Context, scriptID int64, it domain.ComposeItem) {
	if _, err := s.repo.SavePrompt(ctx, repo.SavePromptInput{
		ScriptID: scriptID, ShotID: it.ShotID, Kind: string(it.Kind),
		// ⚠️ 失败时**不清空已有的词**：用户可能手改过一版，
		// 清掉的话他得从头再写。只把状态标成 failed。
		Prompt: "", Status: string(domain.PromptFailed),
	}); err != nil {
		s.warn(ctx, "标记提示词失败状态出错", "shotId", it.ShotID, "error", err)
	}
}

// SavePromptInput 是人工写/改提示词。
type SavePromptInput struct {
	WorkspaceID     int64
	ScriptID        int64
	ShotID          int64
	Kind            domain.PromptKind
	Text            string
	ExpectedVersion int
}

// SavePrompt 人工保存提示词（AV-15 的"人工编辑"）。
//
// ⚠️ 人工写的词也记**当前上游的指纹**：它现在与画面是对得上的，
// 之后上游变了同样该提示过期。不记的话，手写的词永远显示 ready ——
// 而它恰恰最可能和后来改过的画面对不上。
func (s *Service) SavePrompt(ctx context.Context, in SavePromptInput) (repo.Shot, error) {
	if !domain.ValidPromptKind(in.Kind) {
		return repo.Shot{}, httpx.Errf(errcode.InvalidArgument,
			"提示词种类只能是 image / video")
	}
	if strings.TrimSpace(in.Text) == "" {
		return repo.Shot{}, httpx.Errf(errcode.InvalidArgument, "提示词不能为空")
	}
	ups, _, err := s.upstreams(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return repo.Shot{}, err
	}
	u, ok := ups[in.ShotID]
	if !ok {
		return repo.Shot{}, httpx.Errf(errcode.NotFound, "镜头不存在")
	}
	shot, err := s.repo.SavePrompt(ctx, repo.SavePromptInput{
		ScriptID: in.ScriptID, ShotID: in.ShotID, Kind: string(in.Kind),
		Prompt: in.Text, Status: string(domain.PromptReady),
		Source: string(domain.SourcePromptManual), Fingerprint: u.Fingerprint(),
		ExpectedVersion: in.ExpectedVersion,
	})
	if err != nil {
		return repo.Shot{}, translateShot(err)
	}
	return shot, nil
}

// PromptsReady 判断一批镜头的提示词是否都就绪（供组任务前置校验用）。
func (s *Service) PromptsReady(ctx context.Context, workspaceID, scriptID int64,
	kind domain.PromptKind) (bool, []int, error) {
	views, err := s.Prompts(ctx, workspaceID, scriptID)
	if err != nil {
		return false, nil, err
	}
	var notReady []int
	for _, v := range views {
		if v.Kind != kind {
			continue
		}
		if v.Status != domain.PromptReady {
			notReady = append(notReady, v.ShotNo)
		}
	}
	return len(notReady) == 0, notReady, nil
}

func buildComposeItems(shots []repo.Shot, ups map[int64]domain.Upstream,
	kinds []domain.PromptKind, shotIDs []int64) []domain.ComposeItem {
	want := map[int64]bool{}
	for _, id := range shotIDs {
		want[id] = true
	}
	var out []domain.ComposeItem
	for _, sh := range shots {
		if len(want) > 0 && !want[sh.ID] {
			continue
		}
		fp := ups[sh.ID].Fingerprint()
		for _, k := range kinds {
			status, storedFP := domain.PromptStatus(sh.ImagePromptStatus), sh.ImagePromptFingerprint
			if k == domain.PromptVideo {
				status, storedFP = domain.PromptStatus(sh.VideoPromptStatus), sh.VideoPromptFingerprint
			}
			out = append(out, domain.ComposeItem{
				ShotID: sh.ID, ShotNo: sh.ShotNo, Kind: k,
				Status: domain.EffectiveStatus(status, storedFP, fp),
			})
		}
	}
	return out
}

func toItemResults(items []domain.ComposeItem) []ComposeItemResult {
	out := make([]ComposeItemResult, 0, len(items))
	for _, it := range items {
		out = append(out, ComposeItemResult{
			ShotID: it.ShotID, ShotNo: it.ShotNo, Kind: it.Kind,
			Status: it.Status, Skipped: it.Skip, Reason: it.Reason,
		})
	}
	return out
}

// assetNames 把参考资产拼成一行，供模板的 {{assetProfiles}} 占位符用。
func assetNames(assets []domain.PromptAsset) string {
	if len(assets) == 0 {
		return "（无）"
	}
	parts := make([]string, 0, len(assets))
	for i, a := range assets {
		parts = append(parts, fmt.Sprintf("图%d=%s", i+1, a.Name))
	}
	return strings.Join(parts, "，")
}

// composeOutputContract 是输出格式契约（由代码固定）。
func composeOutputContract(kind domain.PromptKind) string {
	target := "一段用于**文生图/图生图**的提示词"
	if kind == domain.PromptVideo {
		target = "一段用于**视频生成**的提示词"
	}
	return "只输出" + target + "，不要解释、不要标题、不要代码块围栏。\n" +
		"保留输入里「参考图对应关系」的编号与顺序 —— 编号与实际传给模型的\n" +
		"参考图一一对应，改动编号会让模型把 A 的特征画到 B 身上。"
}

// composeFallbackSystem 是内置兜底（与 `prompt_template` 里那份要双写）。
const composeFallbackSystem = `你是提示词工程师。把给定的镜头信息改写成一段连贯、具体、可直接使用的生成提示词。
保留画面内容与运动边界，不要添加输入里没有的人物、道具或情节。`
