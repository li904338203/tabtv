package app

import (
	"context"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	imagetooldomain "github.com/li904338203/tabtv/backend/internal/imagetool/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 图片工具栏的可用性与单价（NEW-12、FULL-04、GAP-01）。
//
// ⚠️ **可用性判定复用 `QuoteWithFacts`，不另写一份**。
// 「模型启用 ∧ Provider 就绪 ∧ 操作启用 ∧ 绑定启用 ∧ 有价 ∧ 能力矩阵
// ∧ 输入长度 ∧ 会员等级」这一串必须在**报价、提交、工具栏**三处给出同一个答案 ——
// 各写一份的结果是「工具栏说能用、点下去 422」或者反过来
// 「工具栏灰着而其实能用」，而两种都查不出原因（旧系统的发布检查就踩过：
// 检查端点说可以、启用却说没绑操作）。
//
// 代价是每个工具走一遍报价（9 个工具 ≈ 27 次查询）。这是一次面板打开时的
// 只读调用，用这点开销换「三处口径一定一致」是划算的。

// ImageToolAsk 是工具栏查询的上下文。
type ImageToolAsk struct {
	ModelCode     string
	MemberLevelID int64
	// ImageCount / HasText 是**当前节点上连着什么**，由前端报。
	// 只用于提示（见 domain.ToolProbe 的注释），真正的校验在受理链路上。
	ImageCount int
	HasText    bool
}

// ToolModel 编辑器工具可选的模型。
type ToolModel struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	ProviderName string `json:"providerName,omitempty"`
}

// ImageTool 是工具栏上的一项。
type ImageTool struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	PresetGroup string `json:"presetGroup"`
	Spec        string `json:"spec"`
	InputMode   string `json:"inputMode"`
	Operation   string `json:"operation"`
	Sort        int    `json:"sort"`
	// Available 现在能不能点。
	Available bool `json:"available"`
	// BlockedCode / BlockedReason 不能点的原因。**一定要给具体原因** ——
	// 只给一个布尔值的话，用户面对一排灰按钮无从下手（FE-07 同一条）。
	BlockedCode   string `json:"blockedCode,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
	// UserFixable 这条阻塞是不是用户自己能解决的（缺图/缺词）。
	UserFixable bool `json:"userFixable"`
	// Credits 单次调用的积分，仅在 Available 时有意义。
	//
	// ⚠️ 价格要在**点之前**看得见：点了才知道扣多少，是旧系统被反复投诉的一点。
	Credits int `json:"credits,omitempty"`
	// EstimatedSeconds 预估耗时。
	EstimatedSeconds int `json:"estimatedSeconds,omitempty"`
	// Models 编辑器工具此刻调得动的模型（预设工具不带）。
	Models []ToolModel `json:"models,omitempty"`
}

// ImageTools 列出全部图片工具及其可用性。
func (s *Service) ImageTools(ctx context.Context, ask ImageToolAsk) ([]ImageTool, error) {
	presets, err := s.repo.ListImagePresets(ctx, true)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	// ⚠️ 分组按**工作顺序**（设定图 → 质感调节 → 分镜叙事 → 空间机位），
	// 不按字典序 —— 字典序会把"空间与机位"排在"设定图"前面。
	// 复用 P09-01 那份排序，别在这里再写一遍顺序表：两份顺序表迟早会不一致，
	// 而症状是同一批工具在选择器和工具栏里的排列不同。
	domain.SortPresets(presets,
		func(p repo.ImagePreset) string { return p.PresetGroup },
		func(p repo.ImagePreset) int { return p.Sort })

	roles := domain.RoleCount{}
	if ask.ImageCount > 0 {
		roles[domain.RoleImage] = ask.ImageCount
	}
	if ask.HasText {
		roles[domain.RoleText] = 1
	}

	// ⚠️ 操作本身收不收图，与能力矩阵**不是一回事**。
	// `text2img` 的 in_node_types 是 `text,style` —— 连了一张图给它，
	// 受理时一定被拒，而能力矩阵可能是放行的（管理员完全可能给
	// text2img 配一条 image 角色）。少判这一条的症状就是
	// **工具栏说能用、点下去 422**，而这正是工具栏存在的理由。
	//
	// 这条是 P09-04 的逐工具验收矩阵抓出来的：单测抓不到它，
	// 因为它只在"真的连了一张图"时才出现。
	inputs, err := s.repo.OperationInputs(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}

	out := make([]ImageTool, 0, len(presets)+4)
	for _, p := range presets {
		out = append(out, s.probeTool(ctx, p, ask, roles, inputs[p.Operation]))
	}
	return append(out, s.editorTools(ctx, ask)...), nil
}

func (s *Service) probeTool(ctx context.Context, p repo.ImagePreset, ask ImageToolAsk,
	roles domain.RoleCount, accepts []string) ImageTool {
	name := p.Name
	if p.PresetGroup == "tool" && p.Code == imagetooldomain.ToolUpscale {
		// IMGB-2：这条旧预设是 img2img + 一句「锐化」的提示词，像素没有变多；与新的「高清」工具同名会混淆。
		name = "高清重绘"
	}
	t := ImageTool{
		Code: p.Code, Name: name, PresetGroup: p.PresetGroup, Spec: p.Spec,
		InputMode: p.InputMode, Operation: p.Operation, Sort: p.Sort,
	}
	// ① 用户这边缺不缺输入。先判这一条，因为它是用户自己能解决的 ——
	// 先报"模型不支持"会让他去找运营，而其实他连一张图就好了。
	if b := domain.CheckToolInput(domain.ToolProbe{
		InputMode: p.InputMode, ImageCount: ask.ImageCount, HasText: ask.HasText,
	}, p.Name); b != nil {
		t.BlockedCode, t.BlockedReason = b.Code, b.Detail
		t.UserFixable = domain.UserFixable(b.Code)
		return t
	}
	// 预设没绑操作时不判可用性：它只改参数、不决定调哪个能力。
	if p.Operation == "" || ask.ModelCode == "" {
		t.Available = true
		return t
	}
	// ② 这个操作收不收图。
	if b := domain.CheckOperationAcceptsMedia(accepts, ask.ImageCount, p.Name); b != nil {
		t.BlockedCode, t.BlockedReason = b.Code, b.Detail
		t.UserFixable = domain.UserFixable(b.Code)
		return t
	}

	// ③ 模型那边行不行 —— 走**和报价、受理同一条路径**。
	q, err := s.QuoteWithFacts(ctx, QuoteRequest{
		ModelCode: ask.ModelCode, OperationCode: p.Operation, Count: 1,
		MemberLevelID: ask.MemberLevelID, RoleCounts: roles,
	})
	if err != nil {
		t.BlockedCode, t.BlockedReason = describeBlock(err)
		return t
	}
	t.Available = true
	t.Credits = q.Credits
	t.EstimatedSeconds = q.EstimatedSeconds
	return t
}

// describeBlock 把报价路径的业务错误翻成工具栏能显示的原因。
//
// ⚠️ 取的是**业务码的名字**而不是 HTTP 状态：HTTP 200 + 业务码是本项目的约定，
// 只看状态码的话所有阻塞看起来都一样。内部错误不把原文露给用户
// （可能带库表名），但要留一句能让人知道该找谁的话。
func describeBlock(err error) (code, reason string) {
	var be *httpx.BizError
	if errors.As(err, &be) {
		if be.Code == errcode.Internal {
			return be.Code.Name(), "服务暂时不可用，请稍后再试"
		}
		return be.Code.Name(), be.Message
	}
	return errcode.Internal.Name(), "服务暂时不可用，请稍后再试"
}

// UsableModels 列出用户端可选的模型。
//
// ⚠️ 与 `/image-tools` 是**两个粒度**，不是重复：这里回答"这个模型值得
// 出现在下拉里吗"（启用、Provider 就绪、至少绑了一个能用的操作），
// 而工具栏回答"这个模型 + 这个操作现在能不能点"（还要有价、过能力矩阵、
// 够会员等级）。合成一个接口的话，下拉里就只剩下能跑当前这个工具的模型，
// 用户换个工具会发现模型列表也跟着变 —— 他会以为模型被删了。
//
// agentOnly 只列可作推理模型的（AGT-04，见 repo.ListUsableModels）。
func (s *Service) UsableModels(ctx context.Context, resultType string, agentOnly bool) (
	[]repo.UsableModel, error) {
	list, err := s.repo.ListUsableModels(ctx, resultType, agentOnly)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return list, nil
}

// ModelParams 是一个模型的型号级参数覆盖（合并的第三层），按参数 code 索引（P15-01）。
//
// 前端选定模型后取它，叠在 `/canvas/config` 已经合好的前两层上：清晰度、时长、画幅
// 能选什么由型号决定，不再是节点通用的那一套。
//
// ⚠️ 一行配坏了只跳过那一项并告警（前端那一项退回前两层），不让整个模型拿不到覆盖 ——
// 拿不到的话面板退回通用选项，型号能力又收不住了。
func (s *Service) ModelParams(ctx context.Context, modelCode string) (map[string]map[string]any, error) {
	rows, err := s.repo.ListModelParamSupports(ctx, modelCode)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, httpx.Err(errcode.NotFound)
	}
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		cfg, err := domain.ModelParamOverride(row.Override, row.AllowedValues, row.DefaultValue)
		if err != nil {
			if s.log != nil {
				s.log.WarnContext(ctx, "型号参数覆盖无法解析，这一项按通用配置处理",
					"model", modelCode, "param", row.ParamCode, "err", err)
			}
			continue
		}
		if len(cfg) > 0 {
			out[row.ParamCode] = cfg
		}
	}
	return out, nil
}

// ModelAvailability 是「一个模型 + 它绑定的全部操作」的可用性（MCPT-04 models，MCP-F-27）。
type ModelAvailability struct {
	Code             string
	Name             string
	Domain           string
	Status           string
	MinMemberLevelID int64
	// MaxDurationSec 单次最长时长（视频），nil = 未配置。
	MaxDurationSec *int
	// Available 至少一个操作现在能用。
	Available bool
	// BlockedCode / BlockedReason 只在**一个操作都没绑**时给出（有操作时原因逐操作给）。
	BlockedCode   string
	BlockedReason string
	Operations    []OperationAvailability
}

// OperationAvailability 是模型上一个操作的可用性与单价。
type OperationAvailability struct {
	Code          string
	Domain        string
	Available     bool
	BlockedCode   string
	BlockedReason string
	// UserFixable 用户自己能解决（升级会员 / 调参数）；其余要运营处理。按业务码分类，不匹配文案。
	UserFixable bool
	// Credits / Unit 是最小合法输入下的单次报价（count=1、时长取计价默认），仅 Available 时有意义。
	Credits          int
	Unit             string
	EstimatedSeconds int
}

// ModelsWithAvailability 列出**全部**模型（含停用的）× 已绑定的操作，逐个走 QuoteWithFacts
// 给出能不能用、为什么不能用、单价多少（MCP-F-27）。
//
// ⚠️ 与 UsableModels 是两个问题：那个只列「值得出现在下拉里」的，对外部模型而言等于把不可用的
// 悄悄藏起来 —— 它会以为平台没有这个模型，而其实只差会员等级或运营配一个价。
// ⚠️ 可用性**复用报价那条路径**（同 ImageTools），不另写一份判断。
// ⚠️ 探测输入取能力矩阵的**下限**（每个角色给 min_count 个）：不给输入的话图生视频这类
// 「至少一张图」的操作永远判成不可用，而那只是因为探测时没连图。
//
// resultType 按操作的产出域筛（与 UsableModels 同口径），operation 按操作码筛；空串不筛。
// agent_chat 不在列（它不是画布操作）。
func (s *Service) ModelsWithAvailability(ctx context.Context, resultType, operation string,
	memberLevelID int64) ([]ModelAvailability, error) {
	models, err := s.repo.ListModels(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	bindings, err := s.repo.AllModelOperations(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	ops, err := s.repo.ListOperations(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	opDomain := map[string]string{}
	for _, o := range ops {
		opDomain[o.Code] = o.Domain
	}
	out := []ModelAvailability{}
	for _, m := range models {
		ma := ModelAvailability{Code: m.Code, Name: m.Name, Domain: m.Domain, Status: m.Status,
			MaxDurationSec: m.MaxDurationSec}
		if m.MinMemberLevelID != nil {
			ma.MinMemberLevelID = *m.MinMemberLevelID
		}
		bound := 0
		for _, b := range bindings[m.Code] {
			if b.OperationCode == repo.OpAgentChat {
				continue
			}
			bound++
			if operation != "" && b.OperationCode != operation {
				continue
			}
			if resultType != "" && opDomain[b.OperationCode] != resultType {
				continue
			}
			oa := s.probeOperation(ctx, m.ID, m.Code, b.OperationCode, memberLevelID)
			oa.Domain = opDomain[b.OperationCode]
			ma.Available = ma.Available || oa.Available
			ma.Operations = append(ma.Operations, oa)
		}
		if bound == 0 {
			// 一个操作都没绑：筛选条件对它不成立，只在不筛时列出（并说清原因）。
			if operation != "" || (resultType != "" && m.Domain != resultType) {
				continue
			}
			ma.BlockedCode = errcode.CapabilityUnsupported.Name()
			ma.BlockedReason = "该模型尚未绑定任何操作，需要运营配置"
		} else if len(ma.Operations) == 0 {
			continue // 有绑定但都被筛掉了
		}
		if ma.Operations == nil {
			ma.Operations = []OperationAvailability{}
		}
		out = append(out, ma)
	}
	return out, nil
}

func (s *Service) probeOperation(ctx context.Context, modelID int64, modelCode, op string,
	memberLevelID int64) OperationAvailability {
	oa := OperationAvailability{Code: op}
	roles := domain.RoleCount{}
	caps, err := s.capabilitiesFor(ctx, modelID, op)
	if err != nil {
		oa.BlockedCode, oa.BlockedReason = describeBlock(err)
		return oa
	}
	for _, c := range caps {
		if c.MinCount > 0 {
			roles[c.Role] = c.MinCount
		}
	}
	q, err := s.QuoteWithFacts(ctx, QuoteRequest{
		ModelCode: modelCode, OperationCode: op, Count: 1,
		MemberLevelID: memberLevelID, RoleCounts: roles,
	})
	if err != nil {
		oa.BlockedCode, oa.BlockedReason = describeBlock(err)
		var be *httpx.BizError
		if errors.As(err, &be) {
			oa.UserFixable = be.Code == errcode.MemberLevelRequired || be.Code == errcode.InvalidArgument
		}
		return oa
	}
	oa.Available = true
	oa.Credits, oa.Unit, oa.EstimatedSeconds = q.Credits, q.Unit, q.EstimatedSeconds
	return oa
}

// ---------------------------------------------------------------- 图片节点工具条的编辑器工具

// UsableModelsFor 列出某个操作此刻调得动的模型（绑定 ∧ 启用 ∧ 报得出价，与受理同一个 QuoteWithFacts），按排序。
// 一张图作输入。
func (s *Service) UsableModelsFor(ctx context.Context, operation string) ([]string, error) {
	codes, err := s.repo.ListModelsForOperation(ctx, operation)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		if _, err := s.QuoteWithFacts(ctx, QuoteRequest{ModelCode: code, OperationCode: operation, Count: 1,
			RoleCounts: domain.RoleCount{domain.RoleImage: 1}}); err == nil {
			out = append(out, code)
		}
	}
	return out, nil
}

// editorTools 四个编辑器工具（docs/agent/imagetools IMGD-13，R-5）。它们**不是预设**，不在 image_preset 表里，
// 单独列；都用图片大模型走图生图：可选模型 = 此刻调得动图生图的模型，单价 = 第一个模型的图生图单价。
func (s *Service) editorTools(ctx context.Context, ask ImageToolAsk) []ImageTool {
	out := make([]ImageTool, 0, 4)
	var codes []string
	var listErr error
	if ask.ImageCount == 1 {
		codes, listErr = s.UsableModelsFor(ctx, imagetooldomain.OpImg2Img)
	}
	models := make([]ToolModel, 0, len(codes))
	for _, code := range codes {
		if m, err := s.repo.GetModel(ctx, code); err == nil {
			models = append(models, ToolModel{Code: code, Name: m.Name, ProviderName: m.ProviderCode})
		}
	}
	for i, info := range imagetooldomain.Tools() {
		t := ImageTool{Code: info.Code, Name: info.Name, PresetGroup: "editor", Spec: "editor",
			InputMode: "image_required", Operation: imagetooldomain.OpImg2Img, Sort: 1000 + i, Models: models}
		switch {
		case ask.ImageCount != 1:
			t.BlockedCode, t.BlockedReason, t.UserFixable = domain.ToolNeedsImage,
				"「"+info.Name+"」需要一张已生成的图片", true
		case listErr != nil || len(codes) == 0:
			t.BlockedCode, t.BlockedReason = errcode.ModelUnavailable.Name(), "还没有可用于图生图的图片模型，请联系运营"
		default:
			code := codes[0]
			if ask.ModelCode != "" {
				for _, c := range codes {
					if c == ask.ModelCode {
						code = c
					}
				}
			}
			q, err := s.QuoteWithFacts(ctx, QuoteRequest{ModelCode: code, OperationCode: imagetooldomain.OpImg2Img, Count: 1,
				MemberLevelID: ask.MemberLevelID, RoleCounts: domain.RoleCount{domain.RoleImage: 1}})
			if err != nil {
				t.BlockedCode, t.BlockedReason = describeBlock(err)
				break
			}
			t.Available, t.Credits, t.EstimatedSeconds = true, q.Credits, q.EstimatedSeconds
		}
		out = append(out, t)
	}
	return out
}
