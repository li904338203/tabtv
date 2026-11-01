package wiring

// 本文件是 AGT-01 从 cmd/api/main.go **原样搬来**的跨域适配器（只改包名，
// 以及 ProjectGuard 为供 main 继续使用而导出）。⚠️ 只搬不改：
// 改动任何一行都应另开任务，否则 E2E 回归就失去了"行为逐字不变"的意义。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	billingdomain "github.com/li904338203/tabtv/backend/internal/billing/domain"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	collabdomain "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediadomain "github.com/li904338203/tabtv/backend/internal/media/domain"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	workflowapp "github.com/li904338203/tabtv/backend/internal/workflow/app"
	workflowdomain "github.com/li904338203/tabtv/backend/internal/workflow/domain"
)

// ---------------------------------------------------------------- 整图 DAG 适配器

// canvasSource 把画布域接成计划器的端口（**带归属校验**）。
type canvasSource struct{ svc *canvasapp.Service }

func (c canvasSource) CanvasOf(ctx context.Context, ws, projectID int64) (
	json.RawMessage, error) {
	// 用 owner 角色查：这里只关心「项目在不在这个工作区」，
	// 成员的读写权限由更外层的工作区中间件把关（同 ProjectGuard）。
	p, err := c.svc.Get(ctx, ws, projectID, collabdomain.RoleOwner)
	if err != nil {
		return nil, err
	}
	return p.CanvasData, nil
}

// operationResolver 把连线规则接成计划器的端口。
//
// ⚠️ 走的是**连线校验用的同一个** `ValidateConnection`：
// 另起一条判定路径的话会出现"连线时说这条能走 ref2video、
// 执行时按 img2video 跑"，而两边各自都"没错"。
type operationResolver struct{ svc *catalogapp.Service }

func (o operationResolver) ResolveOperation(ctx context.Context,
	targetType, mode string, sourceTypes []string) (string, error) {
	inputs := make([]catalogdomain.Input, 0, len(sourceTypes))
	for i, st := range sourceTypes {
		inputs = append(inputs, catalogdomain.Input{SourceType: st, Order: i + 1})
	}
	res, err := o.svc.ValidateConnection(ctx, catalogapp.ValidateConnectionInput{
		TargetType: targetType, Mode: mode, Inputs: inputs,
	})
	if err != nil {
		return "", err
	}
	if !res.Valid {
		return "", errors.New(res.Reason)
	}
	return res.Operation, nil
}

// nodeQuoter 把目录域的报价接成计划器的端口。
//
// ⚠️ 复用 `QuoteWithFacts`，与生成受理走的是**同一条报价路径**：
// 另写一份的话，预览说 5 积分、真跑扣 8 积分，而两处都"没错"。
type nodeQuoter struct{ svc *catalogapp.Service }

func (q nodeQuoter) QuoteNode(ctx context.Context, ask workflowapp.QuoteAsk) (
	workflowdomain.Quote, error) {
	res, err := q.svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: ask.ModelCode, OperationCode: ask.OperationCode,
		Count: ask.Count, DurationSec: ask.DurationSec, Text: ask.Text,
		MemberLevelID: ask.MemberLevelID, RoleCounts: toRoleCount(ask.RoleCounts),
	})
	if err != nil {
		return workflowdomain.Quote{}, err
	}
	return workflowdomain.Quote{
		Credits: res.Credits, EstimatedSeconds: res.EstimatedSeconds,
	}, nil
}

// resultLookup 把"每个节点最近一次成功任务"接成计划器的复用依据。
//
// ⚠️ 指纹**必须调用 `workflowdomain.Fingerprint`**，不能在这里照着实现一遍：
// 计划器算"现在的设置"、这里算"上次产出用的"，两份实现永远对不上 ——
// 于是每次运行都把所有节点重跑一遍，用户每跑一次图就多花一次钱，
// 而系统没有任何报错。
type resultLookup struct{ repo *genrepo.Repo }

func (l resultLookup) LatestResults(ctx context.Context, projectID int64) (
	map[string]workflowapp.NodeResult, error) {
	rows, err := l.repo.LatestSuccessByNode(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]workflowapp.NodeResult, len(rows))
	for _, s := range rows {
		out[s.NodeID] = workflowapp.NodeResult{
			GenerationID: s.GenerationID,
			// ⚠️ 直接读**受理时存下来的那个值**，不在这里反算：
			// 计划器手上是画布节点的 params 与上游节点 ID，这里是解析后的
			// params 与产出 ID —— 形态不同，反算出来的永远对不上
			// （见迁移 00018 的说明，那是联调实测出来的）。
			Fingerprint: s.InputFingerprint,
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 脚本域适配器

// textModelAdapter 把 Provider 域的同步文本入口接成脚本域的端口。
type textModelAdapter struct{ p *providerapp.HTTPProvider }

func (a textModelAdapter) Complete(ctx context.Context, in scriptapp.TextRequest) (string, error) {
	return a.p.CompleteText(ctx, providerapp.TextInput{
		ModelCode: in.ModelCode, System: in.System, User: in.User,
	})
}

// textQuoteAdapter 把目录域的报价接成脚本域的端口。
type textQuoteAdapter struct{ svc *catalogapp.Service }

func (a textQuoteAdapter) QuoteText(ctx context.Context, in scriptapp.TextQuoteAsk) (
	scriptapp.TextQuote, error) {
	d, err := a.svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode,
		Count: 1, Text: in.Text, MemberLevelID: in.MemberLevelID,
	})
	if err != nil {
		return scriptapp.TextQuote{}, err
	}
	// ⚠️ MaxInputChars 返 0：长度上限**已经由报价侧按模型配置校验过**了
	// （catalog 的 Quote 里那一条）。这里再查一遍就成了两个事实来源，
	// 改一处忘另一处时行为会不一致。脚本域自己只保留"不能为空"那条。
	return scriptapp.TextQuote{Credits: d.Credits, ModelID: d.ModelID}, nil
}

// scriptLedger 把账本接成脚本域的同步扣费端口。
type scriptLedger struct {
	repo       *billingrepo.Repo
	compensate time.Duration
}

func (l scriptLedger) Consume(ctx context.Context, payerID, userID int64, amount int,
	refType, refID, reason string) error {
	_, err := l.repo.Consume(ctx, billingrepo.ConsumeInput{
		PayerID: payerID, Amount: amount, RefType: refType, RefID: refID,
		UserID: userID, Reason: reason,
	})
	if errors.Is(err, billingdomain.ErrInsufficient) {
		// ⚠️ 翻成业务码：不翻的话「积分不足」会以 500 的面目出现，
		// 用户以为系统坏了，而这本是一个他能自己解决的问题。
		return httpx.Errf(errcode.InsufficientCredits, err.Error())
	}
	return err
}

func (l scriptLedger) Refund(ctx context.Context, payerID int64,
	refType, refID, reason string) error {
	_, err := l.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: l.compensate,
	})
	// 幂等命中不是错误：退款的一次性由账本唯一索引保证。
	if errors.Is(err, billingrepo.ErrDuplicate) {
		return nil
	}
	return err
}

// assetGenAdapter 把生成域接成脚本域的资产出图端口。
//
// ⚠️ 走的是**同一条受理路径**（报价 → 能力校验 → 扣费 → outbox），
// 不给资产图开小灶：开小灶的话，能力矩阵、会员门槛、并发上限这些
// 都要在第二个地方再实现一遍，而那一份早晚会漏掉某条规则。
type assetGenAdapter struct{ svc *genapp.Service }

func (a assetGenAdapter) Generate(ctx context.Context, in scriptapp.AssetGenRequest) (
	scriptapp.AssetGenResult, error) {
	values := map[string]any{}
	for k, v := range in.Params {
		values[k] = v
	}
	// 提示词由脚本域按版式拼好，**最后写**：参数里不许带一段别的提示词把它顶掉。
	values["prompt"] = in.Prompt
	params, _ := json.Marshal(values)
	res, err := a.svc.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
		UserID: in.UserID, ProjectID: in.ProjectID, NodeID: in.NodeID,
		OperationCode: "asset_gen", ModelCode: in.ModelCode,
		IdempotencyKey: in.IdempotencyKey, Count: 1, Text: in.Prompt,
		Params: params, DeclaredInputs: json.RawMessage("[]"),
		// ⚠️ 必须带上 assetId：Worker 靠它把出好的图回写到 script_asset。
		// 不带的话资产会永远停在 generating —— 图已经归档了，
		// 但列表里那一项一直转圈，用户既看不到图也不敢重试。
		ScriptAssetID: in.AssetID,
	})
	if err != nil {
		return scriptapp.AssetGenResult{}, err
	}
	// Replayed + Status 交给脚本域判断：命中的是上一次**失败**的任务时要换一把键重来（CD-77）。
	return scriptapp.AssetGenResult{
		GenerationID: res.Generation.ID, Replayed: res.Replayed, Status: res.Generation.Status,
	}, nil
}

// groupGenAdapter 把组子项的生成接到统一受理入口（P07-04）。
//
// 同 assetGenAdapter：不给组任务开小灶 —— 开了的话能力矩阵、会员门槛、
// 并发上限都要在第二个地方再实现一遍，而那一份早晚会漏掉某条规则。
type groupGenAdapter struct{ svc *genapp.Service }

func (a groupGenAdapter) Generate(ctx context.Context, in scriptapp.GroupGenRequest) (
	scriptapp.GroupGenResult, error) {
	// ⚠️ 提示词必须写进 params：Worker 只从 `params.prompt`（+ 文本输入 + 后缀）拼提示词，
	// 受理入口的 Text 只用来报价与校验预设。此前这里只传了 Text —— 每一条分镜图 / 视频子项
	// 都是「没有提示词」地发出去的，图片编辑上游传完几张参考图才回 `400 prompt is required`（CD-81）。
	params, err := withPrompt(in.Params, in.Prompt)
	if err != nil {
		return scriptapp.GroupGenResult{}, fmt.Errorf("子项参数不是合法的 JSON 对象：%w", err)
	}
	inputs := in.DeclaredInputs
	if len(inputs) == 0 {
		inputs = json.RawMessage("[]")
	}
	res, err := a.svc.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
		UserID: in.UserID, ProjectID: in.ProjectID, NodeID: in.NodeID,
		OperationCode: in.OperationCode, ModelCode: in.ModelCode,
		IdempotencyKey: in.IdempotencyKey, Count: 1, DurationSec: in.DurationSec,
		Text: in.Prompt, Params: params, DeclaredInputs: inputs,
		// ⚠️ 必须带上 groupItemId：Worker 靠它把终态回写到组子项。
		// 不带的话子项永远停在 queued —— 产物已经归档了，
		// 但组里那一格一直转圈（P07-02 在资产上踩过同一个坑）。
		GroupItemID: in.GroupItemID, ShotID: in.ShotID,
	})
	if err != nil {
		return scriptapp.GroupGenResult{}, err
	}
	// Replayed + Status 交给脚本域判断：命中的是上一次**失败**的任务时要换一把键重来（CD-77）。
	return scriptapp.GroupGenResult{
		GenerationID: res.Generation.ID, Credits: res.Generation.CreditsCost,
		Replayed: res.Replayed, Status: res.Generation.Status,
	}, nil
}

// withPrompt 把脚本域拼好的提示词写进参数，**最后写**：参数里不许带一段别的提示词把它顶掉。
// 其余键按原始字节保留（不经 any 转一道，数字与未知字段原样回去）。
func withPrompt(raw json.RawMessage, prompt string) (json.RawMessage, error) {
	values := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
	}
	if values == nil {
		// 快照是字面量 null 时 Unmarshal 会把 map 置空
		values = map[string]json.RawMessage{}
	}
	text, err := json.Marshal(prompt)
	if err != nil {
		return nil, err
	}
	values["prompt"] = text
	return json.Marshal(values)
}

// groupCancelAdapter 把生成取消接成脚本域的端口。
type groupCancelAdapter struct{ svc *genapp.Service }

func (a groupCancelAdapter) Cancel(ctx context.Context, ws, genID int64, reason string) error {
	return a.svc.Cancel(ctx, ws, genID, reason)
}

// presetAdapter 把目录域的图片预设接成生成域的端口。
//
// ⚠️ 端口由**生成域**定义（谁定义端口谁拥有契约）：拼提示词是
// "这次生成长什么样"的一部分，目录域只负责把配方取出来。
type presetAdapter struct{ svc *catalogapp.Service }

func (a presetAdapter) PresetPrompt(ctx context.Context, ask genapp.PresetAsk) (
	string, error) {
	return a.svc.PresetPrompt(ctx, catalogapp.PresetAsk{
		Params: ask.Params, HasImage: ask.HasImage, Text: ask.Text,
	})
}

// nodeGenAdapter 把整图节点的生成接到统一受理入口。
//
// 同 assetGenAdapter / groupGenAdapter：不给整图开小灶 —— 开了的话
// 能力矩阵、会员门槛、并发上限都要在第三个地方再实现一遍。
type nodeGenAdapter struct{ svc *genapp.Service }

func (a nodeGenAdapter) Generate(ctx context.Context, in workflowapp.NodeGenRequest) (
	int64, int, error) {
	res, err := a.svc.Accept(ctx, genapp.AcceptInput{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID,
		UserID: in.UserID, ProjectID: in.ProjectID, NodeID: in.NodeID,
		OperationCode: in.OperationCode, ModelCode: in.ModelCode,
		IdempotencyKey: in.IdempotencyKey, Count: in.Count,
		DurationSec: in.DurationSec, Params: in.Params,
		DeclaredInputs: in.DeclaredInputs, InputFingerprint: in.InputFingerprint,
	})
	if err != nil {
		return 0, 0, err
	}
	return res.Generation.ID, res.Generation.CreditsCost, nil
}

// ProjectGuard 把画布域的项目归属校验接成脚本域**与时间线域**的端口。
// （AGT-01：时间线仍在 cmd/api 装配，故导出并提供 NewProjectGuard。）
//
// ⚠️ 一个适配器供两个端口用：两边的接口方法同名同签名，Go 的结构化接口
// 让它天然成立。写两份一模一样的适配器的话，早晚有人只改了其中一份。
type ProjectGuard struct{ svc *canvasapp.Service }

// NewProjectGuard 供组装根（时间线域）复用同一个适配器。
func NewProjectGuard(svc *canvasapp.Service) ProjectGuard { return ProjectGuard{svc} }

func (g ProjectGuard) EnsureProject(ctx context.Context, workspaceID, projectID int64) error {
	// 用 owner 角色查：这里只关心「项目在不在这个工作区」，
	// 成员的读写权限由更外层的工作区中间件把关。
	_, err := g.svc.Get(ctx, workspaceID, projectID, collabdomain.RoleOwner)
	return err
}

// mediaLookupAdapter 把媒体域的查询接成画布域的端口。
//
// ⚠️ 适配器在**组装根**里写，不写进任一领域：让 canvas 去 import media 的类型，
// 或让 media 去实现 canvas 定义的接口，都会把两个域焊死在一起（08 §1）。
// outputLookupAdapter 把生成域接成画布域的产出查询端口。
//
// ⚠️ 适配器写在组装根：让画布域 import 生成域的类型，或让生成域实现
// 画布域的接口，都会把两个域焊死在一起（08 §1）。
type outputLookupAdapter struct{ repo *genrepo.Repo }

func (a outputLookupAdapter) Lookup(ctx context.Context, id int64) (canvasapp.OutputInfo, error) {
	ref, err := a.repo.LookupOutput(ctx, id)
	if err != nil {
		return canvasapp.OutputInfo{}, err
	}
	return canvasapp.OutputInfo{
		ProjectID: ref.ProjectID, NodeID: ref.NodeID, ResultType: ref.ResultType,
		Status: ref.Status, MediaID: ref.MediaID, Text: ref.Text, DurationMs: ref.DurationMs,
	}, nil
}

type mediaLookupAdapter struct{ svc *mediaapp.Service }

func (a mediaLookupAdapter) LookupInWorkspace(ctx context.Context, ws, id int64) (
	canvasapp.MediaInfo, error) {
	info, err := a.svc.LookupInWorkspace(ctx, ws, id)
	if err != nil {
		return canvasapp.MediaInfo{}, err
	}
	return canvasapp.MediaInfo{
		WorkspaceID: info.WorkspaceID, Kind: info.Kind, Purpose: info.Purpose,
		Width: info.Width, Height: info.Height, DurationMs: info.DurationMs,
	}, nil
}

// directorMediaAdapter 把媒体域接成画布域的导演台上传端口（3D 导演台 DRT-03）。
//
// ⚠️ 删除走 media/app.Service.Delete（先跑全部引用检查器再软删）：上传成功、插记录失败时软删的是一个
// 还没有任何引用的文件；直接改 media_object.status 会绕过别处的引用保护。
type directorMediaAdapter struct{ svc *mediaapp.Service }

// DirectorMediaStore 给不经过 BuildCore 的画布服务用（Worker 的回收站清理）。
//
// ⚠️ 传进来的媒体服务必须已经注册了全部引用检查器：清理之后的释放走它的 Delete，
// 没有检查器的话，复制出来的项目还在用的截图会被一起软删。
func DirectorMediaStore(m *mediaapp.Service) canvasapp.DirectorMediaStore {
	return directorMediaAdapter{m}
}

func (a directorMediaAdapter) Upload(ctx context.Context, in canvasapp.DirectorUploadInput) (
	canvasapp.UploadedMedia, error) {
	up, err := a.svc.Upload(ctx, mediaapp.UploadInput{
		WorkspaceID: in.WorkspaceID, UserID: in.UserID, Purpose: in.Purpose,
		DeclaredMIME: in.DeclaredMIME, FileName: in.FileName, Size: in.Size, Body: in.Body,
		ExpectKind: mediadomain.Kind(in.ExpectKind), MaxBytes: in.MaxBytes,
	})
	if err != nil {
		return canvasapp.UploadedMedia{}, err
	}
	return canvasapp.UploadedMedia{ID: up.ID, MIME: up.MIME, Width: up.Width, Height: up.Height,
		DurationMs: up.DurationMs}, nil
}

func (a directorMediaAdapter) Delete(ctx context.Context, workspaceID, mediaID int64) error {
	return a.svc.Delete(ctx, workspaceID, mediaID)
}

type materialLookupAdapter struct{ svc *catalogapp.Service }

func (a materialLookupAdapter) PromptSuffix(ctx context.Context, styles, effects []string) (
	string, []string, error) {
	return a.svc.MaterialPromptSuffixChecked(ctx, styles, effects)
}

// cancelRefunder 取消时的退款端口。幂等命中不算失败 ——
// 退款的一次性由账本唯一索引保证，这里再报一次只会让调用方误以为失败。
type cancelRefunder struct {
	repo       *billingrepo.Repo
	compensate time.Duration
}

func (c cancelRefunder) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := c.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID, Reason: reason,
		CompensationValidFor: c.compensate,
	})
	if errors.Is(err, billingrepo.ErrDuplicate) {
		return nil
	}
	return err
}

// quoterAdapter 把目录域的报价接成生成域的端口。
type quoterAdapter struct{ svc *catalogapp.Service }

func (a quoterAdapter) QuoteForAccept(ctx context.Context, in genapp.QuoteAsk) (
	genapp.QuoteAnswer, error) {
	var params map[string]any
	if len(in.Params) > 0 {
		// 解不开的参数不在这里拦：受理前面已经按对象校验过，这里只为取清晰度 / 画幅。
		_ = json.Unmarshal(in.Params, &params)
	}
	q, err := a.svc.QuoteWithFacts(ctx, catalogapp.QuoteRequest{
		ModelCode: in.ModelCode, OperationCode: in.OperationCode,
		Count: in.Count, DurationSec: in.DurationSec, Text: in.Text,
		MemberLevelID: in.MemberLevelID, RoleCounts: toRoleCount(in.RoleCounts),
		Params: params, AppendixChars: in.AppendixChars,
	})
	if err != nil {
		return genapp.QuoteAnswer{}, err
	}
	return genapp.QuoteAnswer{
		Credits: q.Credits, EstimatedSeconds: q.EstimatedSeconds, ResultType: q.ResultType,
		ModelID: q.ModelID, OperationID: q.OperationID, ProviderCode: q.ProviderCode,
		APIProtocol: q.APIProtocol, PriceUnit: q.PriceUnit, PriceCredits: q.PriceCredits,
	}, nil
}

func toRoleCount(m map[string]int) catalogdomain.RoleCount {
	out := catalogdomain.RoleCount{}
	for k, v := range m {
		out[catalogdomain.InputRole(k)] = v
	}
	return out
}

// inputAdapter 把画布域的输入解析接成生成域的端口。
// 同时实现 genapp.NoProjectInputResolver（营销「生成角色」，MKT-T-35）。
type inputAdapter struct{ svc *canvasapp.Service }

var _ genapp.NoProjectInputResolver = inputAdapter{}

func (a inputAdapter) ResolveForAcceptNoProject(ctx context.Context, ws int64, operation string,
	declared json.RawMessage) (genapp.ResolvedInputs, error) {
	out, err := a.svc.ResolveForAcceptNoProject(ctx, ws, operation, declared)
	if err != nil {
		return genapp.ResolvedInputs{}, err
	}
	return toResolvedInputs(out), nil
}

func (a inputAdapter) ResolveForAccept(ctx context.Context, ws, projectID int64,
	nodeID, operation string, declared json.RawMessage) (genapp.ResolvedInputs, error) {
	out, err := a.svc.ResolveForAccept(ctx, ws, projectID, nodeID, operation, declared)
	if err != nil {
		return genapp.ResolvedInputs{}, err
	}
	return toResolvedInputs(out), nil
}

// toResolvedInputs 把画布域的解析结果**逐字段**抄成生成域的形状。
//
// ⚠️ 漏抄一个字段没有编译错误：DirectorKinds 漏了的话导演台约束段只会悄悄不出现（DRD-18）。
// `TestInputAdapter_带上DirectorKinds` 用反射核对两边同名字段都抄过来了，将来再加字段也会当场红。
func toResolvedInputs(out canvasapp.AcceptedInputs) genapp.ResolvedInputs {
	return genapp.ResolvedInputs{
		Inputs: out.Inputs, PromptSuffix: out.PromptSuffix,
		RoleCounts: out.RoleCounts, Issues: out.Issues, DirectorKinds: out.DirectorKinds,
		RangeLengthMs: out.RangeLengthMs,
	}
}
