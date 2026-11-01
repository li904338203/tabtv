package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// generate（MCPT-10，MCP-F-30/40~51/63）：建（或复用本授权建的）节点 → **用户确认费用** → 受理 →
// link_generation，立即返回任务 ID。
//
// ⚠️ 入参里**没有任何布尔确认参数**（MCP-F-45）：确认只有两种凭证 —— 基线的 confirmQuoteId + 回显
// confirmCredits（用户在客户端的工具调用审批框里看到的就是这个金额），或 elicitation 表单里用户本人的回答。
// ⚠️ 用户确认之前**不建节点**（Begin 只报价）：拒绝了不留一个空节点在画布上。
// ⚠️ 已用的 quote 再提交是**重放**而不是报错（开发 01 §12 R14）：客户端第一次的响应丢了时，重试必须拿回
// 原任务 ID，否则模型会以为失败、换个参数再生成一次（再扣一次钱）。
// ⚠️ 画布写入与受理沿用 internal/canvasgen（与站内 Agent 同一份）：节点先落盘、受理后必须 Link、
// 参数用画布编码 —— 这条链上踩过的坑不再踩第二次。

const (
	maxPromptRunes   = 20000
	maxGenerateCount = 20
	maxDurationSec   = 600
	maxGenInputs     = 20
)

// generateInputIn 是一条声明的输入（**只有 ID，没有 URL**：服务端按可信 ID 解析，GO-27）。
type generateInputIn struct {
	Kind         string `json:"kind" jsonschema:"text / generation / reference / material / voice"`
	Order        int    `json:"order" jsonschema:"顺序（从 1 起、连续）：决定首尾帧与参考图编号"`
	SourceNodeID string `json:"sourceNodeId,omitempty" jsonschema:"上游节点 ID（kind 为 text / generation 时）"`
	Text         string `json:"text,omitempty" jsonschema:"上游文本节点的文字（kind 为 text）"`
	GenerationID string `json:"generationId,omitempty" jsonschema:"上游已完成任务的 ID（kind 为 generation）"`
	ReferenceID  string `json:"referenceId,omitempty" jsonschema:"已上传参考文件的媒体 ID（kind 为 reference）"`
	MaterialCode string `json:"materialCode,omitempty" jsonschema:"风格 / 特效素材代码（kind 为 material）"`
	MaterialType string `json:"materialType,omitempty" jsonschema:"style / effect（kind 为 material）"`
	VoiceCode    string `json:"voiceCode,omitempty" jsonschema:"音色代码（kind 为 voice）"`
	Role         string `json:"role,omitempty" jsonschema:"显式角色，目前只有 mask（蒙版）"`
}

type generateIn struct {
	ProjectID     string            `json:"projectId" jsonschema:"项目 ID（见 projects）"`
	NodeID        string            `json:"nodeId,omitempty" jsonschema:"已有节点：只能是本凭据创建且用户没改过的节点；不给则新建"`
	NodeType      string            `json:"nodeType,omitempty" jsonschema:"新建节点的类型：image / video / audio / text；不给时按操作的产出类型自动确定（给了必须与之一致）"`
	Title         string            `json:"title,omitempty" jsonschema:"新节点标题"`
	OperationCode string            `json:"operationCode" jsonschema:"操作码，如 text2img / img2img / text2video / img2video / tts"`
	ModelCode     string            `json:"modelCode" jsonschema:"模型代码（见 models）"`
	Prompt        string            `json:"prompt,omitempty" jsonschema:"描述文字（原样计价、不截断）"`
	Params        map[string]any    `json:"params,omitempty" jsonschema:"其余节点参数（画幅、分辨率等）"`
	Inputs        []generateInputIn `json:"inputs,omitempty" jsonschema:"输入（按 order），最多 20 条"`
	DurationSec   int               `json:"durationSec,omitempty" jsonschema:"时长（秒），按秒计价的视频 / 音频用"`
	Count         int               `json:"count,omitempty" jsonschema:"张数 / 次数"`
	// ⚠️ 两个确认字段都不是布尔：回显的是金额，用户在审批框里看得到要花多少。
	ConfirmQuoteID string `json:"confirmQuoteId,omitempty" jsonschema:"确认凭证：上一次调用返回的 quoteId（或 quote 工具对同一组参数签发的 quoteId）"`
	ConfirmCredits *int   `json:"confirmCredits,omitempty" jsonschema:"回显报价金额，必须等于该 quoteId 的金额；与 confirmQuoteId 一起给"`
}

type needsConfirmationOut struct {
	QuoteID        string `json:"quoteId" jsonschema:"确认凭证，一次性"`
	Credits        int    `json:"credits" jsonschema:"本次要花的积分"`
	Balance        int    `json:"balance" jsonschema:"付费主体当前可用积分"`
	After          int    `json:"after" jsonschema:"确认后剩余"`
	PayerAccountID string `json:"payerAccountId" jsonschema:"费用承担账户"`
	ExpiresAt      string `json:"expiresAt" jsonschema:"quoteId 过期时间（UTC）"`
	Summary        string `json:"summary" jsonschema:"要做什么（给用户看）"`
	// ApprovalURL 页面确认（MCPT-13）：凭据要求用户本人在 seeAitv 审批页上批准，客户端的回显金额不算同意。
	ApprovalURL string `json:"approvalUrl,omitempty" jsonschema:"凭据要求在 seeAitv 页面确认时的审批页地址：发给用户打开批准，批准后再带 confirmQuoteId / confirmCredits 重调"`
}

type generateOut struct {
	Status string `json:"status" jsonschema:"queued：已受理；needs_confirmation：需用户确认费用（未扣费）；declined：用户拒绝（未扣费）；重放时为任务当前状态"`
	// 受理（或重放）后才有。
	GenerationID string `json:"generationId,omitempty" jsonschema:"任务 ID（用 tasks 查询）"`
	NodeID       string `json:"nodeId,omitempty" jsonschema:"任务所在的画布节点"`
	Credits      int    `json:"credits" jsonschema:"本次扣的积分（重放时为原任务的积分，本次未再扣）；未受理时为 0"`
	Replayed     bool   `json:"replayed" jsonschema:"true：这个确认之前已经受理过，返回的是原任务，本次没有扣费"`
	// NeedsConfirmation 基线确认：把金额告诉用户，同意后带 confirmQuoteId / confirmCredits 原样重调。
	NeedsConfirmation *needsConfirmationOut `json:"needsConfirmation,omitempty"`
	Message           string                `json:"message" jsonschema:"下一步该做什么"`

	elicit *app.ElicitRequest
}

// ElicitRequest 让 Define 把 elicitation 表单交给 transport（app.ElicitCarrier）。
func (o generateOut) ElicitRequest() *app.ElicitRequest { return o.elicit }

const generateDescription = "在画布上提交一次生成（图片 / 视频 / 音频 / 文本），会扣积分。" +
	"直接调用，不要先在聊天里问用户；平台会请用户确认费用。" +
	"不给 nodeId 时在画布上新建一个节点；nodeId 只能是本凭据创建且用户没改过的节点。" +
	"首次调用返回 status=needs_confirmation 与 quoteId、金额（没有扣费）：把金额告诉用户，用户同意后带 " +
	"confirmQuoteId 与 confirmCredits（等于报价金额）、其余参数完全不变再调用一次；quote 工具对同一组参数签发的单项 " +
	"quoteId 也可直接使用。支持表单确认的客户端会直接弹出确认表单。凭据要求在 seeAitv 页面确认时，结果带 approvalUrl：" +
	"把它发给用户，用户在页面上批准后再带 confirmQuoteId 与 confirmCredits 重调（在客户端里确认不算数）。受理后立即返回任务 ID：这是后台任务，" +
	"宿主超时不代表失败，不要重新提交，用 tasks 查询进度与结果。"

func generateTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "generate",
		Toolset:     domain.ToolsetGeneration,
		Description: generateDescription,
		Methods:     []app.Method{{Action: collab.ActionGenerate, Paid: true, Write: true}},
	}, func(ctx context.Context, _ string, in generateIn) (generateOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return generateOut{}, "", err
		}
		if d.CanvasGen == nil || d.Confirm == nil {
			return generateOut{}, "", unwired("CanvasGen / Confirm")
		}
		return runGenerate(ctx, d, p, in)
	})
}

func runGenerate(ctx context.Context, d Deps, p app.Principal, in generateIn) (generateOut, string, error) {
	projectID, req, err := generateRequest(in, func(op, declared string) (string, error) {
		return d.CanvasGen.NewNodeType(ctx, op, declared)
	})
	if err != nil {
		return generateOut{}, "", err
	}
	c := mcpCaller(p, projectID)
	prep, err := d.CanvasGen.Prepare(ctx, c, req)
	if err != nil {
		return generateOut{}, "", genError(err, req.NodeID, 0)
	}
	// 指纹：quote 单项与 generate 共用的规范形态（app.CanonicalGenerateItem 写明了逐字段规则）。
	fp, err := app.GenerateFingerprint(app.QuoteItem{ModelCode: prep.ModelCode, Operation: prep.OperationCode,
		Count: prep.Count, DurationSec: prep.DurationSec, Text: in.Prompt, Inputs: prep.RoleCounts})
	if err != nil {
		return generateOut{}, "", httpx.Wrap(errcode.Internal, err)
	}
	ask := app.ConfirmAsk{Tool: domain.QuoteToolGenerate, Fingerprint: fp,
		Price: func(ctx context.Context) (app.Priced, error) {
			credits, err := d.CanvasGen.Quote(ctx, c, prep)
			if err != nil {
				return app.Priced{}, err
			}
			return app.Priced{Credits: credits, Detail: generateDetail(projectID, prep, in), Summary: generateSummary(prep, in)}, nil
		}}

	el := app.ElicitationFrom(ctx)
	proof, declined, err := generateProof(ctx, d, p, in, el)
	if err != nil {
		return generateOut{}, "", err
	}
	if declined {
		out := generateOut{Status: "declined", Message: "用户在确认表单里拒绝了这次生成，没有扣费。不要重试，除非用户改变主意。"}
		return out, "用户已拒绝，未扣费", nil
	}
	if proof == nil {
		pd, err := d.Confirm.Begin(ctx, p, ask, el.Supported)
		if err != nil {
			return generateOut{}, "", err
		}
		return needsConfirmation(pd)
	}

	v, err := d.Confirm.Verify(ctx, p, ask, *proof)
	if err != nil {
		return generateOut{}, "", err
	}
	c.BatchID = v.IdemKey // 建节点那一批 = 幂等键；link 那一批由 canvasgen 派生为 `{key}:link`
	if v.Replay {
		t, found, err := d.CanvasGen.FindAccepted(ctx, p.PayerAccountID, v.IdemKey)
		if err != nil {
			return generateOut{}, "", httpx.Wrap(errcode.Internal, err)
		}
		if found {
			// 补挂一次（同批次幂等）：上一次可能在受理之后、Link 之前断掉了。
			lc := c
			lc.ProjectID = t.ProjectID
			d.CanvasGen.Link(ctx, lc, t.NodeID, t.GenerationID)
			out := generateOut{Status: t.Status, GenerationID: idStr(t.GenerationID), NodeID: t.NodeID,
				Credits: t.Credits, Replayed: true, Message: replayMessage}
			return out, fmt.Sprintf("这个确认已受理过：任务 %s，本次没有扣费", out.GenerationID), nil
		}
		// quote 已用而任务不存在：CAS 之后、受理之前崩溃过。照常受理一次（同一幂等键，仍只会有一个任务），
		// 但价格必须仍是用户确认的那个 —— 变了就签发新报价，不按新价受理。
		price, err := ask.Price(ctx)
		if err != nil {
			return generateOut{}, "", err
		}
		if price.Credits != v.Credits {
			return generateOut{}, "", d.Confirm.PriceChanged(ctx, p, ask, v.Credits, price)
		}
	}

	nodeID, err := d.CanvasGen.EnsureNode(ctx, c, prep)
	if err != nil {
		return generateOut{}, "", genError(err, req.NodeID, v.Credits)
	}
	r, err := d.CanvasGen.Accept(ctx, c, prep, nodeID, v.IdemKey)
	if err != nil {
		return generateOut{}, "", genError(err, nodeID, v.Credits)
	}
	d.CanvasGen.Link(ctx, c, nodeID, r.GenerationID)
	replayed := v.Replay || r.Replayed
	if !replayed {
		app.ReportCredits(ctx, r.Credits)
	}
	status := r.Status
	if status == "" {
		status = "queued"
	}
	out := generateOut{Status: status, GenerationID: idStr(r.GenerationID), NodeID: nodeID, Credits: r.Credits,
		Replayed: replayed, Message: queuedMessage}
	if replayed {
		out.Message = replayMessage
	}
	return out, fmt.Sprintf("已提交后台任务 %s（节点 %s，%d 积分）：宿主超时不代表失败，不要重新提交，用 tasks 查询",
		out.GenerationID, nodeID, r.Credits), nil
}

const (
	queuedMessage = "已受理（后台任务）。宿主超时或连接中断不代表失败，不要重新提交；用 tasks 按 generationId 查询进度与结果。"
	replayMessage = "这个确认之前已经受理过，返回的是原任务，本次没有扣费。不要重新提交；用 tasks 按 generationId 查询。"
)

// generateProof 取本次调用的确认凭证：基线（confirmQuoteId + confirmCredits）优先，其次 elicitation 的回答。
// 都没有时返回 nil（要 Begin）。用户在表单里拒绝 / 取消时 declined=true（quote 保持未用、不扣费）。
func generateProof(ctx context.Context, d Deps, p app.Principal, in generateIn, el app.ElicitContext) (
	*domain.Proof, bool, error) {
	return confirmProof(ctx, d, p, in.ConfirmQuoteID, in.ConfirmCredits, el)
}

func needsConfirmation(pd app.Pending) (generateOut, string, error) {
	if pd.Elicit != nil {
		// 表单由 transport 交给客户端（input_required / elicitation/create），这里不给数据。
		return generateOut{elicit: pd.Elicit}, "", nil
	}
	nc := &needsConfirmationOut{QuoteID: idStr(pd.QuoteID), Credits: pd.Credits, Balance: pd.Balance, After: pd.After,
		PayerAccountID: idStr(pd.PayerAccountID), ExpiresAt: ts(pd.ExpiresAt), Summary: pd.Summary,
		ApprovalURL: pd.ApprovalURL}
	if pd.ApprovalURL != "" {
		msg := fmt.Sprintf("此凭据要求用户在 seeAitv 页面确认费用：%s，%d 积分（当前余额 %d，确认后剩余 %d）。还没有扣费。"+
			"把审批链接 %s 发给用户，请用户打开并点「确认」；用户批准后带 confirmQuoteId=%q、confirmCredits=%d、"+
			"其余参数完全不变再调用 generate（%s 前有效）。在客户端里确认不算数，不要自行重调直到用户说已批准。",
			pd.Summary, pd.Credits, pd.Balance, pd.After, pd.ApprovalURL, nc.QuoteID, pd.Credits, nc.ExpiresAt)
		return generateOut{Status: "needs_confirmation", NeedsConfirmation: nc, Message: msg},
			fmt.Sprintf("需要在 seeAitv 页面确认：%d 积分，审批链接 %s", pd.Credits, pd.ApprovalURL), nil
	}
	msg := fmt.Sprintf("需要用户确认费用：%s，%d 积分（当前余额 %d，确认后剩余 %d）。还没有扣费。把金额告诉用户；"+
		"用户同意后带 confirmQuoteId=%q、confirmCredits=%d、其余参数完全不变再调用 generate（%s 前有效）。",
		pd.Summary, pd.Credits, pd.Balance, pd.After, nc.QuoteID, pd.Credits, nc.ExpiresAt)
	return generateOut{Status: "needs_confirmation", NeedsConfirmation: nc, Message: msg},
		fmt.Sprintf("需要确认：%d 积分（quoteId %s）", pd.Credits, nc.QuoteID), nil
}

// generateRequest 校验入参（缺模型 / 操作码一律拒，不猜默认值）并组 canvasgen.Request。
// 不给 nodeId 时新建节点的类型由 newNodeType 定（按目录 out_node_type 推导或核对，见 canvasgen.ResolveNewNodeType）。
func generateRequest(in generateIn, newNodeType func(op, declared string) (string, error)) (int64, canvasgen.Request, error) {
	projectID, err := parseID("projectId", in.ProjectID)
	if err != nil {
		return 0, canvasgen.Request{}, err
	}
	model, op, nodeID := strings.TrimSpace(in.ModelCode), strings.TrimSpace(in.OperationCode), strings.TrimSpace(in.NodeID)
	switch {
	case model == "":
		return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument, "缺少 modelCode：先用 models 查可用的模型")
	case op == "":
		return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument, "缺少 operationCode")
	case in.Count < 0 || in.Count > maxGenerateCount:
		return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("count 只能是 0~%d", maxGenerateCount))
	case in.DurationSec < 0 || in.DurationSec > maxDurationSec:
		return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument,
			fmt.Sprintf("durationSec 只能是 0~%d", maxDurationSec))
	case len(in.Inputs) > maxGenInputs:
		return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("inputs 最多 %d 条", maxGenInputs))
	case utf8.RuneCountInString(in.Prompt) > maxPromptRunes:
		return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("prompt 最多 %d 字", maxPromptRunes))
	case utf8.RuneCountInString(in.Title) > maxTitleRunes:
		return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("title 最多 %d 字", maxTitleRunes))
	}
	params := map[string]any{}
	for k, v := range in.Params {
		params[k] = v
	}
	nodeType := ""
	if nodeID == "" {
		t, err := newNodeType(op, in.NodeType)
		var nte *canvasgen.NodeTypeError
		switch {
		case errors.As(err, &nte):
			return 0, canvasgen.Request{}, httpx.Errf(errcode.InvalidArgument, nte.Msg)
		case err != nil:
			return 0, canvasgen.Request{}, httpx.Wrap(errcode.Internal, err)
		}
		nodeType = t
	} // 改已有节点时以画布上的类型为准（Prepare 读出来）
	// 节点参数与前端节点同形（model / prompt；视频的操作由 mode 决定），同 Agent 的 generate。
	params = canvasgen.BuildNodeParams(nodeType, model, op, in.Prompt, params)
	req := canvasgen.Request{NodeID: nodeID, NodeType: nodeType, Title: in.Title, OperationCode: op, ModelCode: model,
		Prompt: in.Prompt, Params: params, DurationSec: in.DurationSec, Count: in.Count}
	if len(in.Inputs) > 0 {
		b, err := json.Marshal(in.Inputs)
		if err != nil {
			return 0, canvasgen.Request{}, httpx.Wrap(errcode.Internal, err)
		}
		req.Inputs = b
	}
	return projectID, req, nil
}

func mcpCaller(p app.Principal, projectID int64) canvasgen.Caller {
	return canvasgen.Caller{
		WorkspaceID: p.WorkspaceID, ProjectID: projectID, UserID: p.UserID, Role: p.Role,
		PayerAccountID: p.PayerAccountID, MemberLevelID: p.MemberLevelID,
		Origin:   canvasgen.Origin{Kind: canvasgen.OriginMCP, GrantID: p.GrantID, ClientName: p.ClientName},
		LogAttrs: []any{"mcpGrant", p.GrantID},
	}
}

// genError 把画布 / 受理的错误翻成 MCP 的业务码。need 是用户确认的金额（余额不足时说差额）。
func genError(err error, nodeID string, need int) error {
	if errors.Is(err, canvasgen.ErrNodeNotOwned) {
		return httpx.Errf(errcode.McpNodeNotOwned, fmt.Sprintf("节点 %s 不是本凭据创建、或用户之后改过它：MCP 不修改用户的节点。"+
			"请不传 nodeId，在旁边新建一个节点", nodeID)).WithDetails(map[string]any{"nodeId": nodeID})
	}
	var ins *canvasgen.InsufficientCreditsError
	if errors.As(err, &ins) {
		msg := "余额不足，没有扣费；请告诉用户补足积分后再试"
		d := map[string]any{"required": need}
		if ins.Balance >= 0 {
			d["balance"], d["shortfall"] = ins.Balance, domain.Shortfall(need, ins.Balance)
			msg = fmt.Sprintf("余额不足，还差 %d 积分（本次需要 %d，当前余额 %d），没有扣费；请告诉用户补足积分后再试",
				domain.Shortfall(need, ins.Balance), need, ins.Balance)
		}
		return httpx.Errf(errcode.InsufficientCredits, msg).WithDetails(d)
	}
	if errors.Is(err, canvasgen.ErrNoNodeID) {
		return httpx.Wrap(errcode.Internal, err)
	}
	return err
}

// generateDetail 是 mcp_quote.detail.input：报价时的输入。⚠️ 提示词只记长度（码点），不记原文。
func generateDetail(projectID int64, prep app.GenPrepared, in generateIn) map[string]any {
	d := map[string]any{"projectId": idStr(projectID), "modelCode": prep.ModelCode, "operation": prep.OperationCode,
		"textChars": utf8.RuneCountInString(in.Prompt)}
	if prep.NodeID != "" {
		d["nodeId"] = prep.NodeID
	} else {
		d["nodeType"] = prep.NodeType
	}
	if prep.Count > 0 {
		d["count"] = prep.Count
	}
	if prep.DurationSec > 0 {
		d["durationSec"] = prep.DurationSec
	}
	if len(prep.RoleCounts) > 0 {
		d["inputs"] = prep.RoleCounts
	}
	return d
}

// generateSummary 是「做什么」（确认文案）。不含提示词原文：它也进 mcp_quote.detail。
func generateSummary(prep app.GenPrepared, in generateIn) string {
	s := fmt.Sprintf("用模型 %s 执行 %s", prep.ModelCode, prep.OperationCode)
	if prep.NodeID != "" {
		s += "（更新节点 " + prep.NodeID + "）"
	} else if prep.NodeType != "" {
		s += "（新建" + prep.NodeType + "节点）"
	}
	var parts []string
	if prep.Count > 0 {
		parts = append(parts, fmt.Sprintf("%d 张/次", prep.Count))
	}
	if prep.DurationSec > 0 {
		parts = append(parts, fmt.Sprintf("%d 秒", prep.DurationSec))
	}
	if n := utf8.RuneCountInString(in.Prompt); n > 0 {
		parts = append(parts, fmt.Sprintf("描述 %d 字", n))
	}
	if n := len(in.Inputs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d 个输入", n))
	}
	if len(parts) > 0 {
		s += "，" + strings.Join(parts, "，")
	}
	return s
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
