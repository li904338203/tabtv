package domain

import (
	"fmt"
	"sort"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
)

// InputRole 是能力矩阵里的输入角色。
type InputRole string

const (
	RoleText       InputRole = "text"
	RoleImage      InputRole = "image"
	RoleAudio      InputRole = "audio"
	RoleVideo      InputRole = "video"
	RoleFirstFrame InputRole = "first_frame"
	RoleLastFrame  InputRole = "last_frame"
	RoleReference  InputRole = "reference"
)

var allRoles = map[InputRole]bool{
	RoleText: true, RoleImage: true, RoleAudio: true, RoleVideo: true,
	RoleFirstFrame: true, RoleLastFrame: true, RoleReference: true,
}

// Capability 是「某模型在某操作下能接受什么输入」的一条声明。
//
// ⚠️ 这张表是修 GAP-22 的关键。规则层（connection_rule + 视频模式）表达
// **画布语义**，本表表达**该型号真能收什么**，两层覆盖范围本就不同：
//   - 通用 ref2video 规则允许 audio、不允许 video；
//   - 而 Seedance 拒收 audio，其 adapter 又有参考 video 分支；
//   - BBLABU 不支持 first_last_frame。
//
// 校验顺序固定：连线规则 → **能力矩阵** → 参数范围 → 会员等级 → 计价 → 扣费。
// AC-10 要求这些在**调用与扣费之前**就给出明确错误，而不是等上游返回 400。
type Capability struct {
	Role        InputRole
	MinCount    int
	MaxCount    int
	AllowedMIME []string
}

// protocolRoleOf 把能力矩阵的细粒度角色折成协议层的媒体类别。
//
// ⚠️ 两层的粒度**故意不同**：矩阵要能表达「首帧」与「参考图」是不同的位置
// （bblabu 恰恰缺的就是首帧这个位置），而协议层只关心"收不收得了图片"。
// 折叠时 `reference` 既可能是图也可能是视频，因此单独处理。
func protocolRoleOf(r InputRole) (protocol.InputRole, bool) {
	switch r {
	case RoleText:
		return protocol.RoleText, true
	case RoleImage, RoleFirstFrame, RoleLastFrame:
		return protocol.RoleImage, true
	case RoleAudio:
		return protocol.RoleAudio, true
	case RoleVideo:
		return protocol.RoleVideo, true
	}
	return "", false
}

// CheckProtocolOperation 校验「这个协议做不做得了这个操作」。
//
// ⚠️ 这是 AC-10 的入口侧落地：不在这里拦的话，一个
// 「bblabu 模型绑定 first_last_frame」的配置能存进去、能启用、能报价、能扣费，
// 直到 Worker 构造请求时才发现那套接口根本没有首帧的位置。
func CheckProtocolOperation(protocolCode, operation string) error {
	if protocol.SupportsOperation(protocolCode, operation) {
		return nil
	}
	ops := protocol.OperationsOf(protocolCode)
	return fmt.Errorf("协议 %s 不支持操作 %s（它支持：%s）",
		protocolCode, operation, strings.Join(ops, "、"))
}

// CheckProtocolPath 校验「配置的提交地址支不支持这个操作」。
//
// ⚠️ 协议层的能力声明是**按协议**的，说不出「这个型号配的是哪个端点」。
// 可灵就是这样：`ref2video` 只有 omni 端点做得了，而同一个协议下
// 普通端点的模型也能绑上它 —— 于是任务受理成功、扣了费，
// 到 Worker 构造请求时才失败退款。用户看到的是"生成失败"，
// 而真正的问题是运营把操作绑到了错的端点上。
//
// 在发布检查里跑一遍路径改写，就能把它提前到配置时。
func CheckProtocolPath(protocolCode, operation, submitPath string) error {
	p, ok := protocol.Get(protocolCode)
	if !ok || submitPath == "" {
		return nil
	}
	_, err := protocol.ResolveSubmitPath(p, protocol.Job{OperationCode: operation}, submitPath)
	return err
}

// CheckProtocolRole 校验「这个协议在这个操作下收不收这种输入」。
func CheckProtocolRole(protocolCode, operation string, role InputRole) error {
	if err := CheckProtocolOperation(protocolCode, operation); err != nil {
		return err
	}
	if role == RoleReference {
		// 参考输入可能是图也可能是视频，任一支持即可。
		if protocol.SupportsRole(protocolCode, operation, protocol.RoleImage) ||
			protocol.SupportsRole(protocolCode, operation, protocol.RoleVideo) {
			return nil
		}
		return fmt.Errorf("协议 %s 在操作 %s 下不接受参考输入", protocolCode, operation)
	}
	pr, ok := protocolRoleOf(role)
	if !ok {
		return fmt.Errorf("未知输入角色: %q", role)
	}
	if protocol.SupportsRole(protocolCode, operation, pr) {
		return nil
	}
	return fmt.Errorf("协议 %s 在操作 %s 下不接受 %s 输入（它接受：%s）",
		protocolCode, operation, role, joinRoles(protocol.RolesOf(protocolCode, operation)))
}

func joinRoles(roles []protocol.InputRole) string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	if len(out) == 0 {
		return "无"
	}
	return strings.Join(out, "、")
}

func ValidateCapability(c Capability) error {
	if !allRoles[c.Role] {
		return fmt.Errorf("未知输入角色: %q", c.Role)
	}
	if c.MinCount < 0 {
		return fmt.Errorf("minCount 不能为负")
	}
	if c.MaxCount < c.MinCount {
		return fmt.Errorf("maxCount(%d) 不能小于 minCount(%d)", c.MaxCount, c.MinCount)
	}
	if c.MaxCount == 0 && c.MinCount == 0 {
		// 全 0 等于"这个角色一个都不能传"，应该直接不配置这条，
		// 留着会让运营以为配了能力其实没配。
		return fmt.Errorf("minCount 与 maxCount 同时为 0 无意义，请删除该条")
	}
	return nil
}

// CapabilitySet 是某模型某操作的全部能力声明。
type CapabilitySet map[InputRole]Capability

func NewCapabilitySet(caps []Capability) CapabilitySet {
	m := make(CapabilitySet, len(caps))
	for _, c := range caps {
		m[c.Role] = c
	}
	return m
}

// RoleCount 是一次请求里各角色的实际输入数量。
type RoleCount map[InputRole]int

// Check 按能力矩阵校验一次请求的输入。
//
// 返回的错误直接面向用户，必须说清**哪个角色、要求多少、实际多少**——
// "不支持该组合"这种话让用户无从调整。
func (cs CapabilitySet) Check(counts RoleCount) error {
	// 声明了但没给够 / 给多了。
	roles := make([]InputRole, 0, len(cs))
	for r := range cs {
		roles = append(roles, r)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })

	for _, role := range roles {
		c := cs[role]
		n := counts[role]
		if n < c.MinCount {
			return fmt.Errorf("该模型此操作需要至少 %d 个 %s 输入，实际 %d 个", c.MinCount, role, n)
		}
		if n > c.MaxCount {
			return fmt.Errorf("该模型此操作最多接受 %d 个 %s 输入，实际 %d 个", c.MaxCount, role, n)
		}
	}

	// 给了但模型根本不接受这个角色 —— 这是 GAP-22 最典型的场景
	// （规则层放行了 audio，而该型号压根不收）。
	used := make([]InputRole, 0, len(counts))
	for r := range counts {
		used = append(used, r)
	}
	sort.Slice(used, func(i, j int) bool { return used[i] < used[j] })
	for _, role := range used {
		if counts[role] == 0 {
			continue
		}
		if _, ok := cs[role]; !ok {
			return fmt.Errorf("该模型此操作不接受 %s 输入", role)
		}
	}
	return nil
}

// CheckMIME 校验单个输入的 MIME 是否在允许列表内。
func (cs CapabilitySet) CheckMIME(role InputRole, mime string) error {
	c, ok := cs[role]
	if !ok {
		return fmt.Errorf("该模型此操作不接受 %s 输入", role)
	}
	if len(c.AllowedMIME) == 0 {
		return nil // 未声明即不限
	}
	for _, m := range c.AllowedMIME {
		if strings.EqualFold(m, mime) {
			return nil
		}
	}
	return fmt.Errorf("%s 输入不支持 %s 格式，允许: %s", role, mime, strings.Join(c.AllowedMIME, ", "))
}

// PublishBlocker 是模型无法发布的一条具体原因。
//
// FE-07 要求发布页**直接展示阻塞原因**，而不是只给一个"不可发布"。
type PublishBlocker struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// PublishInput 是发布检查所需的事实。
type PublishInput struct {
	ModelEnabled   bool
	ProviderReady  bool
	Protocol       string
	SubmitPath     string
	QueryPath      string
	EnabledOps     []string
	OpsWithPricing map[string]bool
	OpsWithCaps    map[string]bool
	MemberLevelOK  bool
}

// CheckPublishable 计算可调用能力的合取式（07 §4.2 / 06 §7）：
//
//	model.enabled ∧ provider.ready ∧ operation.enabled ∧ model_operation.enabled
//	∧ (精确 pricing ∨ 兜底 pricing) ∧ 参数规则 ∧ 会员等级 ∧ 协议支持
//
// ⚠️ 发布条件**不是一个 status 开关**。逐条返回阻塞原因，
// 让运营知道还差什么，而不是反复试。
func CheckPublishable(in PublishInput) []PublishBlocker {
	var out []PublishBlocker

	if !in.ProviderReady {
		out = append(out, PublishBlocker{"provider_not_ready", "所属 Provider 未启用或未配置凭据"})
	}
	if err := ValidateModelProtocol(in.Protocol, in.SubmitPath, in.QueryPath); err != nil {
		out = append(out, PublishBlocker{"protocol_invalid", err.Error()})
	}
	if len(in.EnabledOps) == 0 {
		out = append(out, PublishBlocker{"no_operation", "至少需要绑定并启用一个操作"})
	}
	for _, op := range in.EnabledOps {
		// ⚠️ 协议层的能力边界也是发布阻塞项：绑了协议做不了的操作，
		// 配得再全也跑不起来，而且要到 Worker 构造请求时才暴露。
		if err := CheckProtocolOperation(in.Protocol, op); err != nil {
			out = append(out, PublishBlocker{"protocol_operation_unsupported", err.Error()})
		}
		// 端点变体也要对得上：同一协议下，有些操作只有特定端点做得了。
		if err := CheckProtocolPath(in.Protocol, op, in.SubmitPath); err != nil {
			out = append(out, PublishBlocker{"protocol_path_unsupported", err.Error()})
		}
		if !in.OpsWithPricing[op] {
			// 启用的能力不能缺价：缺价意味着报价接口会失败，
			// 用户点生成时才看到错误。
			out = append(out, PublishBlocker{"pricing_missing",
				fmt.Sprintf("操作 %s 既无精确价也无兜底价", op)})
		}
		if !in.OpsWithCaps[op] {
			out = append(out, PublishBlocker{"capability_missing",
				fmt.Sprintf("操作 %s 未声明输入能力矩阵，无法在扣费前校验输入", op)})
		}
	}
	if !in.MemberLevelOK {
		out = append(out, PublishBlocker{"member_level_invalid", "会员门槛指向的等级不存在或已停用"})
	}
	return out
}

// CheckAgentCapable 校验「这个模型能不能标成支持 Agent」（AGT-04，AG-14）。
//
// 推理模型要做多轮 + 工具 + 流式调用：只有文本域、且协议实现了 ChatProtocol 的才行。
// ⚠️ 协议判据只有 `protocol.IsChatCapable` 一处（由注册表派生），这里不另写协议名单（GAP-09）。
func CheckAgentCapable(modelDomain, protocolCode string) error {
	if modelDomain != "text" {
		return fmt.Errorf("只有文本模型能作推理模型（当前类型 %s）", modelDomain)
	}
	if !protocol.IsChatCapable(protocolCode) {
		return fmt.Errorf("协议 %s 不支持多轮对话与工具调用，不能作推理模型（请改用 OpenAI Chat 或 Anthropic Messages 协议）",
			protocolCode)
	}
	return nil
}

// CapabilityInheritance：操作自己没声明输入能力时，沿用同一模型另一个操作的能力矩阵。
//
// agent_chat（Agent 推理，docs/agent）用的就是画布里的文本模型，输入形态与 text2text 一致；
// 让运营为同一个模型再配一份能力矩阵只会出现「忘了配 → 报价一律 42203」（2026-10-16 实测）。
// 自己声明了的优先，不覆盖。
var CapabilityInheritance = map[string]string{
	"agent_chat": "text2text",
	// 3D 导演台 AI 搭景（DRT-19）：同样是同步调一次看图的文本模型，输入形态（文本 + 参考图）与 text2text 一致。
	// 不继承的话，运营给模型绑上 scene_build 之后还要再配一份能力矩阵，忘了配就是「AI 搭景暂不可用」。
	"scene_build": "text2text",
	// 智能剪辑的 AI 剪辑（docs/agent/smartcut SCT-08）：同步调一次看图的文本模型（素材缩略图拼图 + 文字），
	// 输入形态与 text2text 一致。
	"smartcut_plan": "text2text",
}

// InheritCapabilityFlags 把「哪些操作声明了能力矩阵」按 CapabilityInheritance 补齐：
// 自己没声明、而被继承的那个操作声明了的，算作已声明。
//
// ⚠️ 发布检查要用它，与报价（capabilitiesFor）是**同一条继承规则**：报价那边认继承、
// 发布这边不认的话，只绑了 agent_chat 的推理模型报价能过，却**启用不了**（报
// 「操作 agent_chat 未声明输入能力矩阵」）—— 运营照着提示去给 agent_chat 单配一份，
// 正是继承规则想免掉的那一步（MCPT-11E 第 220 步实测）。
func InheritCapabilityFlags(withCaps map[string]bool) map[string]bool {
	out := make(map[string]bool, len(withCaps)+len(CapabilityInheritance))
	for op, ok := range withCaps {
		out[op] = ok
	}
	for op, base := range CapabilityInheritance {
		if !out[op] && withCaps[base] {
			out[op] = true
		}
	}
	return out
}
