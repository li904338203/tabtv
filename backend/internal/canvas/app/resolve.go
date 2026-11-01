package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/canvas/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 输入解析编排。
//
// ⚠️ 这里是 GO-27 的执行点：**只认可信资源 ID**。
// 客户端说"用这张图"时给的是 mediaId / generationId，服务端自己去查它是什么、属于谁；
// 客户端**永远不能**给 URL 或 objectKey —— 那等于让调用方自己声明"我有权限"。

// OutputLookup 由生成域（P05）实现：按 ID 查一次产出的归属与状态。
//
// 放成端口而不是直接查表，是因为 generation 是生成域的表（08 §1 依赖方向）。
// 未注入时，凡是引用 generation 的输入一律判为"上游未就绪"——
// 宁可拦住，也不能因为"查不到就放行"而把别人的产出当成已授权资产。
type OutputLookup interface {
	// Lookup 返回产出的项目、节点、结果类型、状态与媒体 ID。
	Lookup(ctx context.Context, generationID int64) (OutputInfo, error)
}

type OutputInfo struct {
	ProjectID  int64
	NodeID     string
	ResultType string // image / audio / video / text
	Status     string // queued / running / success / failed / cancelled
	MediaID    int64
	Text       string
	// DurationMs 是第一个产物的音视频时长（毫秒，0 = 未知）。输入区间据此校验终点（SCD-34）。
	DurationMs int
}

// MediaLookup 由媒体域实现：按工作区查参考文件。
type MediaLookup interface {
	LookupInWorkspace(ctx context.Context, workspaceID, mediaID int64) (MediaInfo, error)
}

type MediaInfo struct {
	// WorkspaceID 是媒体所属工作区，0 = 平台素材：LookupInWorkspace 也会把平台素材查出来，
	// 只认本工作区文件的调用方（导演台场景，DRD-16）据此再拒。
	WorkspaceID int64
	Kind        string // image / audio / video
	Purpose     string
	// Width / Height 图片像素尺寸；0 表示未知。蒙版校验要用（NEW-13）。
	Width  int
	Height int
	// DurationMs 音视频时长（毫秒，0 = 未知）。输入区间据此校验终点（SCD-34）。
	DurationMs int
}

// MaterialLookup 由目录域实现：校验素材码存在且可用，并取提示词后缀。
type MaterialLookup interface {
	// PromptSuffix 回拼好的后缀与**查不到的码**。
	// 第二个返回值是关键：选了已下线或拼错的素材，必须报错而不是静默丢掉。
	PromptSuffix(ctx context.Context, styleCodes, effectCodes []string) (string, []string, error)
}

func (s *Service) SetOutputLookup(l OutputLookup)     { s.outputs = l }
func (s *Service) SetMediaLookup(l MediaLookup)       { s.mediaLookup = l }
func (s *Service) SetMaterialLookup(l MaterialLookup) { s.materials = l }

// ResolveRequest 是一次解析请求。
type ResolveRequest struct {
	ProjectID int64
	NodeID    string
	Operation string
	Inputs    []domain.DeclaredInput
	// noProject 只由 ResolveForAcceptNoProject 置位（MKT-T-35）：不属于任何项目的任务。
	// ⚠️ 不导出：画布接口传 projectId=0 必须照旧 404，不能借这个口子绕过项目归属。
	noProject bool
}

// ResolveResult 是解析结果。Issues 非空即不可提交。
type ResolveResult struct {
	Inputs       []domain.ResolvedInput `json:"inputs"`
	PromptSuffix string                 `json:"promptSuffix,omitempty"`
	Issues       []domain.InputIssue    `json:"issues"`
	Resolvable   bool                   `json:"resolvable"`
}

// ResolveInputs 把客户端声明的输入解析成可信、有序、类型兼容的清单。
//
// ⚠️ **收集全部问题再返回**，不是遇到第一个就退出：用户希望一次看清所有要改的地方，
// 而不是改一个报一个。
func (s *Service) ResolveInputs(ctx context.Context, ws int64, role collab.Role,
	req ResolveRequest) (ResolveResult, error) {
	if err := requireRole(role, collab.ActionGenerate); err != nil {
		return ResolveResult{}, err
	}
	// 项目归属先过一道：解析别人项目里的节点本身就不该成立。
	// 无项目任务（noProject）没有项目可查：它的输入只剩按工作区解析的参考文件与素材。
	if !req.noProject {
		if _, err := s.repo.Get(ctx, ws, req.ProjectID); err != nil {
			return ResolveResult{}, httpx.Err(errcode.NotFound)
		}
	}

	ordered, issues := domain.OrderInputs(req.Inputs)
	var resolved []domain.ResolvedInput
	var styleCodes, effectCodes []string

	for _, d := range ordered {
		// 区间只对媒体引用有意义（SCD-34）：文本、素材、音色带了区间直接拒。媒体引用的区间要等解析出
		// 种类与时长才能判，在 resolveGeneration / resolveReference 里。
		if d.RangeMs != nil && d.Kind != domain.InputGeneration && d.Kind != domain.InputReference {
			issues = append(issues, *domain.CheckRange(d, "", 0))
			continue
		}
		switch d.Kind {
		case domain.InputText:
			if d.Text == "" {
				issues = append(issues, domain.InputIssue{
					NodeID: d.SourceNodeID, Order: d.Order, Code: domain.IssueUpstreamNotReady,
					Message: "上游文本节点还没有内容",
				})
				continue
			}
			resolved = append(resolved, domain.ResolvedInput{
				Kind: domain.InputText, Order: d.Order,
				SourceNodeID: d.SourceNodeID, Text: d.Text,
			})

		case domain.InputGeneration:
			if req.noProject {
				// ⚠️ 「必须同项目」那条防线在 project_id = 0 时不成立：任何工作区的无项目任务都会
				// 被判成「同项目」。无项目任务要用别的产出，先把它存成参考文件（按工作区校验）。
				issues = append(issues, domain.InputIssue{
					NodeID: d.SourceNodeID, Order: d.Order, Code: domain.IssueForbidden,
					Message: "这次生成不属于任何项目，不能直接引用生成产出作为输入",
				})
				continue
			}
			in, iss := s.resolveGeneration(ctx, ws, req.ProjectID, d)
			if iss != nil {
				issues = append(issues, *iss)
				continue
			}
			resolved = append(resolved, in)

		case domain.InputReference:
			in, iss := s.resolveReference(ctx, ws, d)
			if iss != nil {
				issues = append(issues, *iss)
				continue
			}
			resolved = append(resolved, in)

		case domain.InputMaterial:
			// 素材只贡献提示词，不产生媒体输入。
			switch d.MaterialType {
			case "style":
				styleCodes = append(styleCodes, d.MaterialCode)
			case "effect":
				effectCodes = append(effectCodes, d.MaterialCode)
			default:
				issues = append(issues, domain.InputIssue{
					Order: d.Order, Code: domain.IssueTypeMismatch,
					Message: "素材类型只能是 style 或 effect",
				})
				continue
			}
			resolved = append(resolved, domain.ResolvedInput{
				Kind: domain.InputMaterial, Order: d.Order, MaterialCode: d.MaterialCode,
			})

		case domain.InputVoice:
			if d.VoiceCode == "" {
				issues = append(issues, domain.InputIssue{
					Order: d.Order, Code: domain.IssueNotFound, Message: "未选择音色",
				})
				continue
			}
			resolved = append(resolved, domain.ResolvedInput{
				Kind: domain.InputVoice, Order: d.Order, VoiceCode: d.VoiceCode,
			})

		default:
			issues = append(issues, domain.InputIssue{
				Order: d.Order, Code: domain.IssueTypeMismatch,
				Message: fmt.Sprintf("不支持的输入类型: %s", d.Kind),
			})
		}
	}

	// operation 级约束（数量、类型）放在归属解析之后：
	// 先确认每一条都是合法资源，再看这组资源合不合这个玩法。
	issues = append(issues, domain.ValidateForOperation(req.Operation, resolved)...)

	out := ResolveResult{Inputs: resolved, Issues: issues, Resolvable: len(issues) == 0}
	if out.Issues == nil {
		out.Issues = []domain.InputIssue{}
	}
	if s.materials != nil && (len(styleCodes) > 0 || len(effectCodes) > 0) {
		suffix, missing, err := s.materials.PromptSuffix(ctx, styleCodes, effectCodes)
		if err != nil {
			return ResolveResult{}, httpx.Wrap(errcode.Internal, err)
		}
		for _, code := range missing {
			out.Issues = append(out.Issues, domain.InputIssue{
				Code:    domain.IssueNotFound,
				Message: fmt.Sprintf("素材 %s 不存在或已下线，请重新选择", code),
			})
		}
		out.Resolvable = len(out.Issues) == 0
		// ⚠️ 提示词在**服务端**拼接后下发给上游，不回传给前端去拼：
		// 前端拼出来的词会落进 canvas_data，用户改一个字玩法就失效，
		// 历史节点也永远停在旧词（CD-21 同一条理由）。
		out.PromptSuffix = suffix
	}
	return out, nil
}

func (s *Service) resolveGeneration(ctx context.Context, ws, projectID int64,
	d domain.DeclaredInput) (domain.ResolvedInput, *domain.InputIssue) {
	if s.outputs == nil {
		// 生成域没接上时一律判未就绪 —— "查不到就放行"会把别人的产出当成已授权资产。
		return domain.ResolvedInput{}, &domain.InputIssue{
			NodeID: d.SourceNodeID, Order: d.Order, Code: domain.IssueUpstreamNotReady,
			Message: "生成任务链路尚未接入",
		}
	}
	info, err := s.outputs.Lookup(ctx, d.GenerationID)
	if err != nil {
		return domain.ResolvedInput{}, &domain.InputIssue{
			NodeID: d.SourceNodeID, Order: d.Order, Code: domain.IssueNotFound,
			Message: "引用的产出不存在",
		}
	}
	// ⚠️ **必须同项目**。跨项目的 generationId 能预览却不能作输入 ——
	// 项目复制时正是因此把 result.id 解绑成 sourceGenerationId（CD-19）。
	if info.ProjectID != projectID {
		return domain.ResolvedInput{}, &domain.InputIssue{
			NodeID: d.SourceNodeID, Order: d.Order, Code: domain.IssueForbidden,
			Message: "引用的产出属于另一个项目，不能直接作为输入",
		}
	}
	if info.Status != "success" {
		return domain.ResolvedInput{}, &domain.InputIssue{
			NodeID: d.SourceNodeID, Order: d.Order, Code: domain.IssueUpstreamNotReady,
			Message: upstreamMessage(d.SourceNodeID, info.Status),
		}
	}
	kind := domain.MediaKindFor(info.ResultType)
	if iss := domain.CheckRange(d, kind, info.DurationMs); iss != nil {
		return domain.ResolvedInput{}, iss
	}
	if kind == "" && info.ResultType == "text" {
		// 文本产出直接当文本输入用（图片反推 → 文生图这条链）。
		return domain.ResolvedInput{
			Kind: domain.InputText, Order: d.Order,
			SourceNodeID: d.SourceNodeID, Text: info.Text,
			GenerationID: d.GenerationID,
		}, nil
	}
	if info.MediaID == 0 {
		return domain.ResolvedInput{}, &domain.InputIssue{
			NodeID: d.SourceNodeID, Order: d.Order, Code: domain.IssueUpstreamNotReady,
			Message: "上游产出已完成但没有可用的媒体",
		}
	}
	in := domain.ResolvedInput{
		Kind: domain.InputGeneration, Order: d.Order, SourceNodeID: d.SourceNodeID,
		GenerationID: d.GenerationID, MediaID: info.MediaID, MediaKind: kind, RangeMs: d.RangeMs,
	}
	// 图片带上像素尺寸（与参考图同一来源）：图片工具条按源图定画幅、情绪调节按它裁切与贴回
	// （docs/agent/imagetools）。查不到只是「尺寸未知」，不影响作输入。
	if kind == "image" && s.mediaLookup != nil {
		if m, err := s.mediaLookup.LookupInWorkspace(ctx, ws, info.MediaID); err == nil {
			in.Width, in.Height = m.Width, m.Height
		}
	}
	return in, nil
}

func (s *Service) resolveReference(ctx context.Context, ws int64,
	d domain.DeclaredInput) (domain.ResolvedInput, *domain.InputIssue) {
	if s.mediaLookup == nil || d.ReferenceID == 0 {
		return domain.ResolvedInput{}, &domain.InputIssue{
			Order: d.Order, Code: domain.IssueNotFound, Message: "参考文件不存在",
		}
	}
	info, err := s.mediaLookup.LookupInWorkspace(ctx, ws, d.ReferenceID)
	if err != nil {
		// ⚠️ 不区分"不存在"与"不是你的"：区分开等于提供一个用 id 探测
		// 别人有哪些资源的接口（同 CD-23）。
		return domain.ResolvedInput{}, &domain.InputIssue{
			Order: d.Order, Code: domain.IssueNotFound, Message: "参考文件不存在",
		}
	}
	if iss := domain.CheckRange(d, info.Kind, info.DurationMs); iss != nil {
		return domain.ResolvedInput{}, iss
	}
	return domain.ResolvedInput{
		Kind: domain.InputReference, Order: d.Order,
		ReferenceID: d.ReferenceID, MediaID: d.ReferenceID, MediaKind: info.Kind,
		Width: info.Width, Height: info.Height, Role: d.Role, Purpose: info.Purpose, RangeMs: d.RangeMs,
	}, nil
}

// upstreamMessage 把上游状态翻成用户看得懂的话。
//
// ⚠️ `failed` 与 `queued` 要分开说：一个需要重试，一个只需要等。
// 统一说成"上游未就绪"会让用户一直等一个永远不会好的节点。
func upstreamMessage(node, status string) string {
	switch status {
	case "queued":
		return fmt.Sprintf("上游节点 %s 还在排队，请等它完成", node)
	case "running":
		return fmt.Sprintf("上游节点 %s 正在生成，请等它完成", node)
	case "failed":
		return fmt.Sprintf("上游节点 %s 生成失败，请先重试它", node)
	case "cancelled":
		return fmt.Sprintf("上游节点 %s 已取消，请重新生成", node)
	}
	return fmt.Sprintf("上游节点 %s 尚未就绪（%s）", node, status)
}

// ResolveForAccept 是给生成受理用的解析入口。
//
// 与 ResolveInputs 的区别只在**出入参形态**：受理侧拿到的是原始 JSON，
// 且不做角色校验（受理前已经过工作区中间件）。规则一份，不重复实现。
func (s *Service) ResolveForAccept(ctx context.Context, ws, projectID int64,
	nodeID, operation string, declared json.RawMessage) (AcceptedInputs, error) {
	return s.resolveAccept(ctx, ws, ResolveRequest{ProjectID: projectID, NodeID: nodeID,
		Operation: operation}, declared)
}

// ResolveForAcceptNoProject 是不属于任何项目的任务的解析入口（MKT-T-35「生成角色」，dev/05 MKD-28）。
//
// 与 ResolveForAccept 同一套规则，只差两点：不查项目归属；`generationId` 输入一律拒绝。
// 参考文件仍按**工作区**解析（他人工作区 / 平台素材被拒），素材码照常校验。
func (s *Service) ResolveForAcceptNoProject(ctx context.Context, ws int64, operation string,
	declared json.RawMessage) (AcceptedInputs, error) {
	return s.resolveAccept(ctx, ws, ResolveRequest{Operation: operation, noProject: true}, declared)
}

func (s *Service) resolveAccept(ctx context.Context, ws int64, req ResolveRequest,
	declared json.RawMessage) (AcceptedInputs, error) {
	var raw []struct {
		Kind         string `json:"kind"`
		Order        int    `json:"order"`
		SourceNodeID string `json:"sourceNodeId"`
		Text         string `json:"text"`
		GenerationID string `json:"generationId"`
		ReferenceID  string `json:"referenceId"`
		MaterialCode string `json:"materialCode"`
		MaterialType string `json:"materialType"`
		VoiceCode    string `json:"voiceCode"`
		// Role 显式角色，目前只有 `mask`（NEW-13）。
		Role string `json:"role"`
		// RangeMs 只用素材的哪一段（SCD-34）。⚠️ generation/domain.RedeclareInputs 里有同名字段，改一边要改另一边。
		RangeMs []int `json:"rangeMs"`
	}
	if len(declared) > 0 {
		if err := json.Unmarshal(declared, &raw); err != nil {
			return AcceptedInputs{Issues: []string{"inputs 不是合法的输入数组"}}, nil
		}
	}
	in := make([]domain.DeclaredInput, 0, len(raw))
	for _, x := range raw {
		d := domain.DeclaredInput{
			Kind: domain.InputKind(x.Kind), Order: x.Order, SourceNodeID: x.SourceNodeID,
			Text: x.Text, MaterialCode: x.MaterialCode, MaterialType: x.MaterialType,
			VoiceCode: x.VoiceCode, Role: x.Role, RangeMs: x.RangeMs,
		}
		if x.GenerationID != "" {
			d.GenerationID, _ = strconv.ParseInt(x.GenerationID, 10, 64)
		}
		if x.ReferenceID != "" {
			d.ReferenceID, _ = strconv.ParseInt(x.ReferenceID, 10, 64)
		}
		in = append(in, d)
	}

	req.Inputs = in
	res, err := s.ResolveInputs(ctx, ws, collab.RoleOwner, req)
	if err != nil {
		return AcceptedInputs{}, err
	}
	msgs := make([]string, 0, len(res.Issues))
	for _, i := range res.Issues {
		msgs = append(msgs, i.Message)
	}
	out, err := json.Marshal(res.Inputs)
	if err != nil {
		return AcceptedInputs{}, httpx.Wrap(errcode.Internal, err)
	}
	return AcceptedInputs{
		Inputs: out, PromptSuffix: res.PromptSuffix,
		RoleCounts: RoleCountsOf(res.Inputs), Issues: msgs, DirectorKinds: DirectorKindsOf(res.Inputs),
		RangeLengthMs: domain.RangeLengthMs(res.Inputs),
	}, nil
}

// AcceptedInputs 是给受理侧的解析结果。
type AcceptedInputs struct {
	Inputs       json.RawMessage
	PromptSuffix string
	RoleCounts   map[string]int
	Issues       []string
	// DirectorKinds 是输入里 3D 导演台产物的种类（capture / preview，固定顺序、去重），受理据此追加约束段（DRD-18）。
	// ⚠️ wiring 的 inputAdapter 是**逐字段**复制到生成域的：漏抄没有编译错误，约束段只会悄悄不出现。
	DirectorKinds []string
	// RangeLengthMs 是带区间的输入里最长那段的长度（毫秒，0 = 没有区间）。受理按它计价，不收客户端报的时长（SCD-34）。
	RangeLengthMs int
}

// DirectorKindsOf 按参考文件的 purpose（director_media.go 的常量）汇总导演台产物：截图（含深度图、骨架图）
// 记 capture，预演视频记 preview。
// 顺序固定（先图后视频），与约束段的拼接顺序一致；没有导演台输入时返回 nil。
func DirectorKindsOf(inputs []domain.ResolvedInput) []string {
	var capture, preview bool
	for _, in := range inputs {
		switch in.Purpose {
		case purposeDirectorCapture:
			capture = true
		case purposeDirectorPreview:
			preview = true
		}
	}
	var out []string
	if capture {
		out = append(out, "capture")
	}
	if preview {
		out = append(out, "preview")
	}
	return out
}

// RoleCountsOf 数出各角色的实际数量，供能力矩阵校验。
//
// ⚠️ 必须由**服务端按解析结果**数，不能信客户端报的数：信它的话，
// 能力矩阵的 min/max 等于没有 —— 少报一张图就能绕过去。
//
// ⚠️ 素材与音色**不计入**：它们只贡献提示词，不是媒体输入
// （与 ValidateForOperation 的口径保持一致，两处不一致会让用户在
// 「连线规则说可以、能力矩阵说不行」之间来回猜）。
func RoleCountsOf(inputs []domain.ResolvedInput) map[string]int {
	counts := map[string]int{}
	for _, in := range inputs {
		switch in.Kind {
		case domain.InputMaterial, domain.InputVoice:
			continue
		case domain.InputText:
			counts["text"]++
		default:
			if in.MediaKind != "" {
				counts[in.MediaKind]++
			}
		}
	}
	return counts
}
