package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 创造页「直接生成」（AGT-42，AG-02/AG-20；开发 01 §10）。
//
// 一次请求：预报价 → 一条 insert 建项目 + 单节点 → **事务外**受理 → 把受理结果写回节点。
//
// ⚠️ 受理（generation.Accept）自己管事务与 outbox，而 Provider/MQ 不入事务（§5.3），
// 所以建项目与受理**没法原子回滚**：受理失败时项目保留、节点标失败（原话写在节点上），
// 跳转后用户在画布上直接重试。不做「受理失败删项目」—— 删掉用户可能已经看到的东西更糟。
//
// ⚠️ 放在 canvas 域：它拥有项目。生成受理与报价走下面的端口，由组装根接上，
// 本域不 import 生成域与目录域（08 §1 依赖方向）。

// QuickAccepter 由组装根实现：报价、余额、受理三件事。
//
// ⚠️ 报价与受理必须是**同一条路径**（目录域 QuoteWithFacts / 生成域 Accept）：
// 另写一份的话「发送前显示的金额」与「账本里扣的」会有两个答案（AG-20）。
type QuickAccepter interface {
	Quote(ctx context.Context, ask QuickQuoteAsk) (QuickQuote, error)
	// Balance 付费主体当前可用积分。报价只管「能不能做、多少钱」，余额是账本的事。
	Balance(ctx context.Context, payerID int64) (int, error)
	Accept(ctx context.Context, ask QuickAcceptAsk) (QuickAccepted, error)
}

type QuickQuoteAsk struct {
	ModelCode     string
	OperationCode string
	Count         int
	DurationSec   int
	Text          string
	MemberLevelID int64
	RoleCounts    map[string]int
}

type QuickQuote struct {
	Credits int
}

type QuickAcceptAsk struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	ProjectID      int64
	NodeID         string
	OperationCode  string
	ModelCode      string
	IdempotencyKey string
	MemberLevelID  int64
	Count          int
	DurationSec    int
	Text           string
	Params         json.RawMessage
	DeclaredInputs json.RawMessage
}

type QuickAccepted struct {
	GenerationID int64
	Status       string
	Credits      int
}

func (s *Service) SetQuickAccepter(a QuickAccepter) { s.quick = a }

// QuickRef 一份附件：**只有 ID**（GO-27），不收 URL / objectKey。
type QuickRef struct {
	ReferenceID int64
	Order       int
	// Name 只用于节点上的显示（参考胶囊的文件名），不参与任何校验。
	Name string
}

// QuickInput 是一次直接生成请求。
type QuickInput struct {
	WorkspaceID    int64
	PayerAccountID int64
	UserID         int64
	Role           collab.Role
	MemberLevelID  int64
	IdempotencyKey string
	Mode           string
	ModelCode      string
	// Operation 用户在参考模式里显式选的操作（可空，空则按附件推荐）。
	Operation string
	Text      string
	Params    json.RawMessage
	Refs      []QuickRef
}

// QuickResult 是结果。Accepted=false 时项目与节点已经建好，Error 是受理的原话。
type QuickResult struct {
	ProjectID    int64
	NodeID       string
	GenerationID int64
	Accepted     bool
	Replayed     bool
	Credits      int
	ErrorCode    errcode.Code
	Error        string
}

// MaxQuickRefs 附件上限。真正的张数约束在能力矩阵与参数配置（maxCount）里，
// 这里只挡「一次塞几百个 ID」这种不可能是正常操作的请求。
const MaxQuickRefs = 16

// MaxQuickIdemKeyLen 与列宽 varchar(64) 一致。
const MaxQuickIdemKeyLen = 64

// QuickGenerate 创造页直接生成。
func (s *Service) QuickGenerate(ctx context.Context, in QuickInput) (QuickResult, error) {
	// ⚠️ 建项目要 edit、受理要 generate，两道都过：路由层已经标了 generate，
	// 这里再判一次是因为 app 方法也可能被别的入口（将来的 Agent）调用。
	if err := requireRole(in.Role, collab.ActionEdit); err != nil {
		return QuickResult{}, err
	}
	if !in.Role.Can(collab.ActionGenerate) {
		return QuickResult{}, httpx.Errf(errcode.PermissionDenied, "你在该工作区是只读，无法发起创作")
	}
	if err := validateQuickKey(in.IdempotencyKey); err != nil {
		return QuickResult{}, err
	}
	if s.quick == nil {
		return QuickResult{}, httpx.Errf(errcode.Internal, "生成受理未接入")
	}
	mode, err := domain.ParseQuickMode(in.Mode)
	if err != nil {
		return QuickResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if strings.TrimSpace(in.ModelCode) == "" {
		return QuickResult{}, httpx.Errf(errcode.InvalidArgument, "请选择模型")
	}
	if strings.TrimSpace(in.Text) == "" && len(in.Refs) == 0 {
		return QuickResult{}, httpx.Errf(errcode.InvalidArgument, "请先写下想生成的内容")
	}
	if len(in.Refs) > MaxQuickRefs {
		return QuickResult{}, httpx.Errf(errcode.InvalidArgument,
			"附件最多 "+strconv.Itoa(MaxQuickRefs)+" 个")
	}

	// ① 幂等重放：同一个键已经建过项目 → 返回那个项目，不再建第二个。
	if p, err := s.repo.FindByCreateIdemKey(ctx, in.WorkspaceID, in.IdempotencyKey); err == nil {
		return s.quickReplay(ctx, in, mode, p)
	} else if !errors.Is(err, repo.ErrNotFound) {
		return QuickResult{}, httpx.Wrap(errcode.Internal, err)
	}

	plan, err := s.quickPlan(ctx, in, mode)
	if err != nil {
		return QuickResult{}, err
	}

	// ② 预报价 + 余额：失败**直接返回，不建项目**（余额不足、模型不可用、会员等级不够……）。
	// 受理时还会再报一次价 —— 这一次只是为了在建项目之前就把能预见的失败拦下来，
	// 否则用户每点一次发送、钱不够，项目列表里就多一个空项目。
	q, err := s.quick.Quote(ctx, QuickQuoteAsk{
		ModelCode: in.ModelCode, OperationCode: plan.operation,
		Count: plan.count, DurationSec: plan.duration, Text: in.Text,
		MemberLevelID: in.MemberLevelID, RoleCounts: plan.roleCounts,
	})
	if err != nil {
		return QuickResult{}, err
	}
	if q.Credits > 0 {
		bal, err := s.quick.Balance(ctx, in.PayerAccountID)
		if err != nil {
			return QuickResult{}, httpx.Wrap(errcode.Internal, err)
		}
		if bal < q.Credits {
			return QuickResult{}, httpx.Errf(errcode.InsufficientCredits,
				"积分不足：本次需要 "+strconv.Itoa(q.Credits)+"，当前 "+strconv.Itoa(bal)+
					"，还差 "+strconv.Itoa(q.Credits-bal)).WithDetails(map[string]any{
				"required": q.Credits, "balance": bal, "shortfall": q.Credits - bal,
			})
		}
	}

	// ③ 一条 insert：项目 + 单节点 + 幂等键。
	nodeID := domain.QuickNodeID(time.Now().UnixMilli())
	node, err := domain.BuildQuickNode(nodeID, mode, domain.QuickNodeTitle(mode), plan.nodeParams)
	if err != nil {
		return QuickResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	canvas := domain.QuickCanvas(node)
	if err := domain.ValidateCanvas(canvas); err != nil {
		return QuickResult{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	p := repo.Project{
		ID: s.nextID(), WorkspaceID: in.WorkspaceID, CreatedBy: in.UserID,
		Name: domain.QuickProjectName(in.Text), CanvasData: canvas,
	}
	if err := s.repo.InsertWithIdemKey(ctx, p, in.IdempotencyKey); err != nil {
		if errors.Is(err, repo.ErrDuplicate) {
			// 并发的同键请求抢先插入了：按重放返回它建的那个。
			existing, e2 := s.repo.FindByCreateIdemKey(ctx, in.WorkspaceID, in.IdempotencyKey)
			if e2 != nil {
				return QuickResult{}, httpx.Errf(errcode.Conflict, "并发创建冲突，请重试")
			}
			return s.quickReplay(ctx, in, mode, existing)
		}
		return QuickResult{}, httpx.Wrap(errcode.Internal, err)
	}
	s.writeAudit(ctx, in.UserID, "project.quick_create", p.ID, map[string]any{
		"mode": string(mode), "operation": plan.operation, "modelCode": in.ModelCode,
	})

	// ④ 事务外受理 + ⑤ 把结果写回节点。
	res := s.quickAccept(ctx, in, plan, p.ID, nodeID)
	return res, nil
}

// quickPlan 是一次请求里「与项目无关」的部分：操作码、节点参数、受理输入。
type quickPlanned struct {
	operation   string
	count       int
	duration    int
	roleCounts  map[string]int
	nodeParams  json.RawMessage
	acceptParam json.RawMessage
	declared    json.RawMessage
}

func (s *Service) quickPlan(ctx context.Context, in QuickInput, mode domain.QuickMode) (quickPlanned, error) {
	params := map[string]json.RawMessage{}
	if len(in.Params) > 0 && string(in.Params) != "null" {
		if err := json.Unmarshal(in.Params, &params); err != nil || params == nil {
			return quickPlanned{}, httpx.Errf(errcode.InvalidArgument, "params 必须是 JSON 对象")
		}
	}

	// 附件的真实类型**服务端查**（不信客户端报的类型，GO-27）。
	// 查不到的不计数也不在这里报错：受理时输入解析会给出那句原话（「参考文件不存在」），
	// 节点标失败、项目保留 —— 与画布上点生成遇到同一问题时的表现一致。
	imageCount := 0
	type refView struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	refs := make([]refView, 0, len(in.Refs))
	declared := make([]map[string]any, 0, len(in.Refs))
	for i, r := range in.Refs {
		if r.ReferenceID <= 0 {
			return quickPlanned{}, httpx.Errf(errcode.InvalidArgument, "附件 ID 非法")
		}
		kind := ""
		if s.mediaLookup != nil {
			if info, err := s.mediaLookup.LookupInWorkspace(ctx, in.WorkspaceID, r.ReferenceID); err == nil {
				kind = info.Kind
			}
		}
		if kind != "" && kind != "image" {
			return quickPlanned{}, httpx.Errf(errcode.InvalidArgument,
				"直接生成的附件目前只支持图片，第 "+strconv.Itoa(i+1)+" 个附件是"+kindLabel(kind))
		}
		if kind == "image" {
			imageCount++
		}
		order := r.Order
		if order <= 0 {
			order = i + 1
		}
		id := strconv.FormatInt(r.ReferenceID, 10)
		refs = append(refs, refView{ID: id, Type: "image", Name: trimName(r.Name)})
		declared = append(declared, map[string]any{"kind": "reference", "referenceId": id, "order": order})
	}

	op, err := domain.OperationFor(mode, imageCount, in.Operation)
	if err != nil {
		return quickPlanned{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}

	// 受理用的参数：与画布节点生成时发的同形（`mergeMaterialReferences` 去掉了 reference）。
	params["model"], _ = json.Marshal(in.ModelCode)
	if mode == domain.QuickVideo {
		// 视频节点的操作由 `params.mode` 决定（前端 operationForNode），写进去画布上才对得上。
		params["mode"], _ = json.Marshal(op)
	}
	delete(params, "reference")
	text := strings.TrimSpace(in.Text)
	if text != "" {
		// 音频节点的正文在 `text`，图片/视频在 `prompt`（前端 ParamPanel 的 textarea 同此）。
		key := "prompt"
		if mode == domain.QuickAudio {
			key = "text"
		}
		params[key], _ = json.Marshal(text)
	}
	acceptParams, err := json.Marshal(params)
	if err != nil {
		return quickPlanned{}, httpx.Wrap(errcode.Internal, err)
	}
	if len(refs) > 0 {
		params["reference"], _ = json.Marshal(refs)
	}
	nodeParams, err := json.Marshal(params)
	if err != nil {
		return quickPlanned{}, httpx.Wrap(errcode.Internal, err)
	}
	declaredRaw, _ := json.Marshal(declared)
	count, duration := domain.QuickQuantities(mode, acceptParams)
	rc := map[string]int{}
	if imageCount > 0 {
		rc["image"] = imageCount
	}
	return quickPlanned{
		operation: op, count: count, duration: duration, roleCounts: rc,
		nodeParams: nodeParams, acceptParam: acceptParams, declared: declaredRaw,
	}, nil
}

// quickAccept 事务外受理，并把结果写回节点（版本 CAS = 1）。
func (s *Service) quickAccept(ctx context.Context, in QuickInput, plan quickPlanned,
	projectID int64, nodeID string) QuickResult {
	out := QuickResult{ProjectID: projectID, NodeID: nodeID}
	acc, err := s.quick.Accept(ctx, QuickAcceptAsk{
		WorkspaceID: in.WorkspaceID, PayerAccountID: in.PayerAccountID, UserID: in.UserID,
		ProjectID: projectID, NodeID: nodeID, OperationCode: plan.operation, ModelCode: in.ModelCode,
		// ⚠️ 加前缀：与画布上那个节点后续手动生成用的键（浏览器随机生成）不会撞，
		// 而同一次「发送意图」的重放落在同一个 generation 上，不会扣第二次。
		IdempotencyKey: "quick:" + in.IdempotencyKey,
		MemberLevelID:  in.MemberLevelID,
		Count:          plan.count, DurationSec: plan.duration, Text: strings.TrimSpace(in.Text),
		Params: plan.acceptParam, DeclaredInputs: plan.declared,
	})
	var result map[string]any
	if err != nil {
		code, msg := errcode.Internal, err.Error()
		var biz *httpx.BizError
		if errors.As(err, &biz) {
			code, msg = biz.Code, biz.Error()
		}
		out.ErrorCode, out.Error = code, msg
		// ⚠️ 节点上写服务端原话：用户跳转后看到的是「为什么没跑起来」，而不是一个空节点。
		result = map[string]any{"status": "failed", "error": msg, "errorCode": strings.ToLower(code.Name())}
	} else {
		out.Accepted, out.GenerationID, out.Credits = true, acc.GenerationID, acc.Credits
		status := acc.Status
		if status == "" {
			status = "queued"
		}
		// 跳转后由 generation store 的 restore() 按 result.id 接着跟踪。
		result = map[string]any{"id": strconv.FormatInt(acc.GenerationID, 10), "status": status, "progress": 0}
	}
	raw, _ := json.Marshal(result)
	ok, mErr := s.repo.MarkNodeResult(ctx, in.WorkspaceID, projectID, nodeID, raw, 1, in.UserID)
	if mErr != nil || !ok {
		// 用户已经打开并保存过（版本不再是 1）或写入失败：放弃这一笔，不覆盖他的画布。
		// 受理本身已经发生，画布上的生成 store 仍能从任务列表对上账。
		s.logWarnf(ctx, "直接生成：结果未写回节点", projectID, mErr)
	}
	return out
}

// quickReplay 同键重放：返回已建的项目，**不再建第二个**。
//
// 节点上已经有受理结果 → 原样回报，不再受理。
// 节点上还没有结果（第一次请求在受理前中断，或并发的另一个请求正在受理）→ 再走一次受理：
// 受理的幂等键是同一个（`quick:<键>`），命中已有任务就是重放，不会扣第二次费。
func (s *Service) quickReplay(ctx context.Context, in QuickInput, mode domain.QuickMode,
	p repo.Project) (QuickResult, error) {
	nodeID, result := firstNode(p.CanvasData)
	out := QuickResult{ProjectID: p.ID, NodeID: nodeID, Replayed: true}
	if nodeID == "" {
		return out, nil
	}
	if result.Status != "" {
		switch result.Status {
		case "failed":
			out.Error = result.Error
			if out.Error == "" {
				out.Error = "生成未能受理"
			}
		default:
			out.Accepted = true
			out.GenerationID, _ = strconv.ParseInt(result.ID, 10, 64)
		}
		return out, nil
	}
	if p.Status != "active" {
		return out, nil
	}
	plan, err := s.quickPlan(ctx, in, mode)
	if err != nil {
		return QuickResult{}, err
	}
	res := s.quickAccept(ctx, in, plan, p.ID, nodeID)
	res.Replayed = true
	return res, nil
}

type nodeResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func firstNode(canvas json.RawMessage) (string, nodeResult) {
	var doc struct {
		Nodes []struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
		} `json:"nodes"`
	}
	if json.Unmarshal(canvas, &doc) != nil || len(doc.Nodes) == 0 {
		return "", nodeResult{}
	}
	var r nodeResult
	_ = json.Unmarshal(doc.Nodes[0].Result, &r)
	return doc.Nodes[0].ID, r
}

func validateQuickKey(key string) error {
	if key == "" {
		return httpx.Errf(errcode.MissingIdempotencyKey, "必须提供 Idempotency-Key")
	}
	if len(key) > MaxQuickIdemKeyLen {
		return httpx.Errf(errcode.MissingIdempotencyKey,
			"Idempotency-Key 长度超过上限 "+strconv.Itoa(MaxQuickIdemKeyLen))
	}
	for _, r := range key {
		// 与生成域同一套字符集：键会进日志、进对账，任意字节要各处转义。
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == ':' || r == '.'
		if !ok {
			return httpx.Errf(errcode.MissingIdempotencyKey, "Idempotency-Key 只允许字母、数字与 - _ : .")
		}
	}
	return nil
}

func kindLabel(kind string) string {
	switch kind {
	case "video":
		return "视频"
	case "audio":
		return "音频"
	}
	return kind
}

func trimName(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 255 {
		s = string(r[:255])
	}
	return s
}

func (s *Service) logWarnf(ctx context.Context, msg string, id int64, err error) {
	if s.log != nil {
		s.log.WarnContext(ctx, msg, "projectId", id, "error", err)
	}
}
