// Package app 编排画布配置用例。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/li904338203/tabtv/backend/internal/catalog/domain"
	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	imagetooldomain "github.com/li904338203/tabtv/backend/internal/imagetool/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/secret"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ScopeCanvas 是画布配置的版本作用域。
const ScopeCanvas = "canvas"

type Service struct {
	repo   *repo.Repo
	nextID func() int64
	audit  *audit.Logger
	cipher *secret.Cipher
	cache  *ConfigCache
	log    *slog.Logger
}

func (s *Service) SetCache(c *ConfigCache)  { s.cache = c }
func (s *Service) SetLogger(l *slog.Logger) { s.log = l }

func New(r *repo.Repo, nextID func() int64) *Service {
	return &Service{repo: r, nextID: nextID}
}

func (s *Service) SetAudit(a *audit.Logger) { s.audit = a }

// Snapshot 是下发给前端的配置快照。
type Snapshot struct {
	Revision        int64                 `json:"revision"`
	NodeTypes       []repo.NodeType       `json:"nodeTypes"`
	ConnectionRules []repo.ConnectionRule `json:"connectionRules"`
	ImagePresets    []repo.ImagePreset    `json:"imagePresets"`
	VideoModes      []string              `json:"videoModes"`
	// ImageTools 图片节点工具条的界面配置（docs/agent/imagetools IMG-I-04）。
	// ⚠️ 每次现算、不进缓存：它是代码常量，不随配置版本变；进缓存的话老缓存里没有这个字段，
	// 要等到下一次配置变更才出现。
	ImageTools imagetooldomain.UIConfig `json:"imageTools"`
}

// Config 组装配置快照。
//
// ⚠️ 返回的是**服务端合成结果**，不是五张表的直接拼接（07 §3.3）：
// 参数的控件配置已按 param_def.config < node_param.config_override 合并完。
func (s *Service) Config(ctx context.Context) (Snapshot, error) {
	rev, err := s.repo.Revision(ctx, ScopeCanvas)
	if err != nil {
		return Snapshot{}, httpx.Wrap(errcode.Internal, err)
	}
	// 缓存键含 revision，命中即直接返回。
	var cached Snapshot
	if s.cache != nil && s.cache.Get(ctx, ScopeCanvas, rev, &cached) {
		cached.ImageTools = imagetooldomain.BuildUIConfig()
		return cached, nil
	}

	nodes, err := s.repo.ListNodeTypes(ctx, true)
	if err != nil {
		return Snapshot{}, httpx.Wrap(errcode.Internal, err)
	}
	// 合并控件配置。模型级覆盖（model_param_support）在选定模型后
	// 由生成域再叠一层，这里只合并前两层。
	for i := range nodes {
		for j := range nodes[i].Params {
			p := &nodes[i].Params[j]
			merged, err := domain.MergeConfig(p.Base, p.Override)
			if err != nil {
				return Snapshot{}, httpx.Wrap(errcode.Internal, err)
			}
			p.Config = merged
		}
	}
	rules, err := s.repo.ListConnectionRules(ctx)
	if err != nil {
		return Snapshot{}, httpx.Wrap(errcode.Internal, err)
	}
	presets, err := s.repo.ListImagePresets(ctx, true)
	if err != nil {
		return Snapshot{}, httpx.Wrap(errcode.Internal, err)
	}
	snap := Snapshot{
		Revision: rev, NodeTypes: nodes, ConnectionRules: rules,
		ImagePresets: presets, VideoModes: domain.VideoModes(),
	}
	if s.cache != nil {
		s.cache.Set(ctx, ScopeCanvas, rev, snap)
	}
	snap.ImageTools = imagetooldomain.BuildUIConfig()
	return snap, nil
}

// Revision 供 304 判定。
func (s *Service) Revision(ctx context.Context) (int64, error) {
	rev, err := s.repo.Revision(ctx, ScopeCanvas)
	if err != nil {
		return 0, httpx.Wrap(errcode.Internal, err)
	}
	return rev, nil
}

// RuleSet 供连线校验使用。
func (s *Service) RuleSet(ctx context.Context) (domain.RuleSet, error) {
	rules, err := s.repo.ListConnectionRules(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]domain.Rule, 0, len(rules))
	for _, r := range rules {
		out = append(out, domain.Rule{
			SourceType: r.SourceType, TargetType: r.TargetType,
			IsAllowed: r.IsAllowed, DefaultOperation: r.DefaultOperation,
		})
	}
	return domain.NewRuleSet(out), nil
}

// ValidateConnectionInput 是连线预校验请求。
type ValidateConnectionInput struct {
	TargetType string
	Mode       string
	Inputs     []domain.Input
}

type ValidateConnectionResult struct {
	Valid     bool   `json:"valid"`
	Operation string `json:"operation,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ValidateConnection 预校验连线。
//
// ⚠️ 这是**画布语义**层校验。具体型号能否接受这些输入由能力矩阵
// （model_input_capability）另行判定 —— 两层覆盖范围不同（GAP-22），
// 通过这里不代表一定能提交成功。
func (s *Service) ValidateConnection(ctx context.Context, in ValidateConnectionInput) (ValidateConnectionResult, error) {
	rs, err := s.RuleSet(ctx)
	if err != nil {
		return ValidateConnectionResult{}, err
	}
	v := rs.CheckConnection(in.TargetType, in.Mode, in.Inputs)
	return ValidateConnectionResult{Valid: v.Valid, Operation: v.Operation, Reason: v.Reason}, nil
}

// ---- 管理端 ----

func (s *Service) ListParamDefs(ctx context.Context) ([]repo.ParamDef, error) {
	p, err := s.repo.ListParamDefs(ctx)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	return p, nil
}

type ParamDefInput struct {
	Code    string
	Name    string
	Control string
	Config  json.RawMessage
	// IsEnabled nil = 不改启用状态（新建默认启用）；停用后靠它重新启用。
	IsEnabled *bool
}

// SaveParamDef 新建或更新参数定义。
//
// ⚠️ 控件配置在**写入时**校验（P03-01 出口）：
// 保存成功但前端渲染不出来，错误要等用户反馈才发现。
func (s *Service) SaveParamDef(ctx context.Context, operatorID int64, in ParamDefInput) error {
	if in.Code == "" || in.Name == "" {
		return httpx.Errf(errcode.InvalidArgument, "code 与 name 必填")
	}
	if err := domain.ValidateParamDefFields(in.Code, in.Name); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := domain.ValidateControlConfig(in.Control, in.Config); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	cfg := in.Config
	if len(cfg) == 0 {
		cfg = []byte("{}")
	}
	if err := s.repo.UpsertParamDef(ctx, s.nextID(), in.Code, in.Name, in.Control, cfg, in.IsEnabled); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bump(ctx, operatorID, "param_def.save", map[string]any{
		"code": in.Code, "control": in.Control,
	})
}

func (s *Service) DisableParamDef(ctx context.Context, operatorID int64, code string) error {
	if err := s.repo.DisableParamDef(ctx, code); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return httpx.Err(errcode.NotFound)
		}
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bump(ctx, operatorID, "param_def.disable", map[string]any{"code": code})
}

type ConnectionRuleInput struct {
	SourceType       string
	TargetType       string
	IsAllowed        bool
	DefaultOperation string
}

func (s *Service) SaveConnectionRule(ctx context.Context, operatorID int64, in ConnectionRuleInput) error {
	if in.SourceType == "" || in.TargetType == "" {
		return httpx.Errf(errcode.InvalidArgument, "sourceType 与 targetType 必填")
	}
	nodes, err := s.repo.ListNodeTypes(ctx, false)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	ops, err := s.repo.ListOperations(ctx)
	if err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	rn := make([]domain.RuleNode, 0, len(nodes))
	for _, n := range nodes {
		rn = append(rn, domain.RuleNode{Code: n.Code, Kind: n.Kind})
	}
	ro := make([]domain.RuleOperation, 0, len(ops))
	for _, o := range ops {
		ro = append(ro, domain.RuleOperation{Code: o.Code, InNodeTypes: o.InNodeTypes, OutNodeType: o.OutNodeType})
	}
	if err := domain.ValidateConnectionRuleConfig(in.SourceType, in.TargetType, in.IsAllowed,
		in.DefaultOperation, rn, ro); err != nil {
		return httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	if err := s.repo.UpsertConnectionRule(ctx, s.nextID(), repo.ConnectionRule{
		SourceType: in.SourceType, TargetType: in.TargetType,
		IsAllowed: in.IsAllowed, DefaultOperation: in.DefaultOperation,
	}); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	return s.bump(ctx, operatorID, "connection_rule.save", map[string]any{
		"source": in.SourceType, "target": in.TargetType, "allowed": in.IsAllowed,
	})
}

// bump 递增配置版本并写审计。
//
// ⚠️ 每个写配置的入口都必须走这里。漏掉 bump 就会让前端继续用旧配置，
// 而这正是旧系统"漏删缓存导致最长 30 分钟脏读"的同类问题（CD-14）。
func (s *Service) bump(ctx context.Context, operatorID int64, action string, after any) error {
	if err := s.repo.BumpRevision(ctx, ScopeCanvas, operatorID); err != nil {
		return httpx.Wrap(errcode.Internal, err)
	}
	if s.audit != nil {
		s.audit.Write(ctx, audit.Entry{
			ActorType: audit.ActorAdmin, ActorID: operatorID,
			Action: action, ObjectType: "canvas_config", After: after,
		})
	}
	return nil
}

// ---------------------------------------------------------------- 图片预设

// PresetAsk 与生成域的同名结构对应（那边定义端口，这边实现）。
type PresetAsk struct {
	Params   json.RawMessage
	HasImage bool
	Text     string
}

// PresetPrompt 按 `params.preset` 取配方并拼成要追加的提示词（NEW-12）。
//
// ⚠️ 没选预设时返回空串而不是报错：绝大多数生成不走预设。
//
// ⚠️ 选了一个**已下线或不存在**的预设时也返回空串（只记一条告警）：
// 历史节点里存着早就下线的 code 是常态（下线用 is_enabled=false 不删行），
// 为此把一次生成拒掉的话，用户会发现"以前能跑的节点今天点不动了"。
func (s *Service) PresetPrompt(ctx context.Context, ask PresetAsk) (string, error) {
	code, options := presetSelection(ask.Params)
	if code == "" {
		return "", nil
	}
	rows, err := s.repo.ListImagePresets(ctx, true)
	if err != nil {
		return "", httpx.Wrap(errcode.Internal, err)
	}
	var found *repo.ImagePreset
	for i := range rows {
		if rows[i].Code == code {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		if s.log != nil {
			s.log.WarnContext(ctx, "节点引用了不可用的图片预设，本次按无预设处理",
				"preset", code)
		}
		return "", nil
	}

	p := domain.Preset{
		Code: found.Code, Name: found.Name, Group: found.PresetGroup,
		Spec: found.Spec, InputMode: domain.PresetInputMode(found.InputMode),
		Prompt: found.Prompt, Dimensions: domain.ParseDimensions(found.Dimensions),
		Operation: found.Operation,
	}
	// ⚠️ 校验在**扣费之前**（调用方在受理链路的报价之前调这里）：
	// 不校验的话用户对一个空节点点"打光"，钱扣了、上游收到一句没有图的
	// 提示词，出来一张跟他的照片毫无关系的图。
	if err := domain.ValidatePresetInput(p, ask.HasImage, ask.Text); err != nil {
		return "", httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return domain.ComposePreset(p, options), nil
}

// presetSelection 从节点参数里读出预设码与各维度的选中值。
//
// 约定：`params.preset` 是码，`params.presetOptions` 是 `{维度码: 取值}`。
// ⚠️ 前端**只传这两个**，不传拼好的词 —— 拼好的词会落进 canvasData。
func presetSelection(raw json.RawMessage) (string, map[string]string) {
	if len(raw) == 0 {
		return "", nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", nil
	}
	code, _ := m["preset"].(string)
	if code == "" {
		return "", nil
	}
	options := map[string]string{}
	if opts, ok := m["presetOptions"].(map[string]any); ok {
		for k, v := range opts {
			if s, ok := v.(string); ok {
				options[k] = s
			}
		}
	}
	return code, options
}
