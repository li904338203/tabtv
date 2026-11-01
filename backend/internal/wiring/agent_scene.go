package wiring

import (
	"context"
	"log/slog"
	"strconv"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
)

// 营销会话的 Agent 上下文与只读工具（MKT-T-18）：agent 定义 SceneContext / MarketingOptionLister，
// 这里接到营销域（摘要格式与可见性判定各只有一份）与目录域（场景段模板）。

// AssetNames 按工作区取角色资产名（脚本域；Worker 用 BuildCore 里那一份脚本服务）。
type AssetNames interface {
	AssetName(ctx context.Context, ws, assetID int64) (string, error)
}

// ScriptAssetNames 把脚本服务接成 AssetNames。
type ScriptAssetNames struct{ Script *scriptapp.Service }

// AssetName 实现 AssetNames。
func (s ScriptAssetNames) AssetName(ctx context.Context, ws, id int64) (string, error) {
	a, err := s.Script.AssetInWorkspace(ctx, ws, id)
	if err != nil {
		return "", err
	}
	return a.Name, nil
}

// AgentSceneContext 实现 agentapp.SceneContext 与 agentapp.MarketingOptionLister。
type AgentSceneContext struct {
	Marketing *marketingapp.Service
	Prompts   *catalogapp.Service
	// Assets 为 nil 时角色资产一律按「角色资产」写（不查名字）。
	Assets AssetNames
	// Variants 单次变体上限（SEEAITV_MARKETING_MAX_VARIANTS）；≤ 0 按 4。
	Variants int
	Logger   *slog.Logger
}

// AgentSceneInput 是装配入参。
type AgentSceneInput struct {
	Marketing *marketingapp.Service
	Prompts   *catalogapp.Service
	Script    *scriptapp.Service
	Variants  int
	Logger    *slog.Logger
}

// NewAgentSceneContext 装出营销会话的上下文端口。
//
// ⚠️ 不看 SEEAITV_MARKETING_ENABLED：总开关只管入口（营销页、建会话）；关掉之后，已经建好的
// 营销会话在 Worker 里继续跑时仍要拿到场景段与营销输入（同 MKT-T-21 的 Appendix）。
func NewAgentSceneContext(in AgentSceneInput) AgentSceneContext {
	c := AgentSceneContext{Marketing: in.Marketing, Prompts: in.Prompts, Variants: in.Variants, Logger: in.Logger}
	if in.Script != nil {
		c.Assets = ScriptAssetNames{Script: in.Script}
	}
	return c
}

// RenderScenePrompt 实现 SceneContext（DB 行 > 代码兜底）。
func (a AgentSceneContext) RenderScenePrompt(ctx context.Context, code, modelCode string,
	vars map[string]string) (string, error) {
	return a.Prompts.RenderPrompt(ctx, code, modelCode, vars)
}

// FallbackScenePrompt 实现 SceneContext（只用代码兜底）。
func (a AgentSceneContext) FallbackScenePrompt(code string, vars map[string]string) (string, error) {
	return a.Prompts.RenderFallbackPrompt(code, vars)
}

// MaxVariants 实现 SceneContext。
func (a AgentSceneContext) MaxVariants() int {
	if a.Variants <= 0 {
		return 4
	}
	return a.Variants
}

// unknownCharacterAsset 是取不到名字的角色资产（已删除 / 不在本工作区）在摘要里的写法：
// 不写成「上传的人物图」（那是另一种来源），也不报错中断整段摘要。
const unknownCharacterAsset = "角色资产（已不可用）"

// MarketingSummary 实现 SceneContext：按会话上钉住的 scene_input 拼「## 营销输入」正文。
//
// ⚠️ 只给名称与说明（MKT-F-43）：选项的名称 / 说明取**钉住的那个版本**（版本历史，不看是否在投 ——
// 会话开始后运营改了说明，Agent 看到的仍是用户当时选的那一版）；推荐模型与推荐参数版本历史里没有，
// 取**当前**那一版（只是智能匹配的参考，已下线的创意不给推荐）。片段原文与媒体 ID 都不出这里。
func (a AgentSceneContext) MarketingSummary(ctx context.Context, ws int64, si agentapp.SceneInput,
	visible int) (string, error) {
	in := mktdomain.SummaryInput{
		ProductName: si.Product.Name, SellingPoints: si.Product.SellingPoints,
		ProductImageCount: len(si.Products), VisibleImageCount: visible, MaxVariants: a.MaxVariants(),
		Characters: make([]mktdomain.SummaryCharacter, 0, len(si.Characters)),
	}
	// 商品库会话（MKT-T-37）才有描述行：旧会话没有 productId，整行不出现（上下文与改动前逐字一致）。
	if si.ProductID != "" {
		d := si.Product.Description
		in.ProductDescription = &d
	}
	for _, c := range si.Characters {
		if c.CharacterID != "" {
			// 角色库角色：名称与形象 / 音色文字来自快照；参考音频只说「有」，ID 不进上下文（MKD-33）。
			in.Characters = append(in.Characters, mktdomain.SummaryCharacter{Library: true, Name: c.Name,
				Appearance: c.Appearance, VoiceText: c.VoiceText, HasVoice: c.VoiceMediaID != ""})
			continue
		}
		in.Characters = append(in.Characters, mktdomain.SummaryCharacter{AssetName: a.characterName(ctx, ws, c)})
	}
	seen := map[string]bool{}
	for _, p := range []struct {
		kind mktdomain.Kind
		pick *agentapp.ScenePick
	}{{mktdomain.KindCreative, si.Picks.Creative}, {mktdomain.KindHook, si.Picks.Hook},
		{mktdomain.KindStyle, si.Picks.Style}} {
		if p.pick == nil {
			continue
		}
		seen[string(p.kind)+":"+p.pick.Code] = true
		in.Options = append(in.Options, a.summaryOption(ctx, p.kind, *p.pick))
	}
	// 规划时新钉的选项（04 文档 P-48）：同一 code 只列一次（建会话时选过的不重复）。
	for _, p := range si.Pins {
		key := p.Kind + ":" + p.Code
		if seen[key] {
			continue
		}
		seen[key] = true
		o := a.summaryOption(ctx, mktdomain.Kind(p.Kind), agentapp.ScenePick{Code: p.Code, Version: p.Version})
		o.FromPlan = true
		in.Options = append(in.Options, o)
	}
	return mktdomain.SlotSummary(in), nil
}

func (a AgentSceneContext) characterName(ctx context.Context, ws int64, c agentapp.SceneRef) string {
	if c.Kind != "asset" {
		return "" // 用户上传的人物图：SlotSummary 写「上传的人物图 #n」
	}
	if a.Assets == nil {
		return unknownCharacterAsset
	}
	id, err := strconv.ParseInt(c.ID, 10, 64)
	if err != nil || id <= 0 {
		return unknownCharacterAsset
	}
	name, err := a.Assets.AssetName(ctx, ws, id)
	if err != nil || name == "" {
		a.logger().Debug("营销输入：角色资产名取不到", "assetId", id, "error", err)
		return unknownCharacterAsset
	}
	return name
}

func (a AgentSceneContext) summaryOption(ctx context.Context, kind mktdomain.Kind,
	p agentapp.ScenePick) mktdomain.SummaryOption {
	o := mktdomain.SummaryOption{Kind: kind, Name: p.Name, Code: p.Code}
	snaps, err := a.Marketing.Snapshot(ctx, []mktdomain.Pick{{Kind: kind, Code: p.Code, Version: p.Version}})
	if err == nil && len(snaps) == 1 {
		o.Name, o.Description = snaps[0].Name, snaps[0].Description
	} else {
		// 钉住的版本取不到（数据坏了）：退回建会话时记下的名称，不编造说明。
		a.logger().Warn("营销输入：钉住的选项版本取不到", "kind", kind, "code", p.Code, "version", p.Version,
			"error", err)
	}
	if o.Name == "" {
		o.Name = p.Code
	}
	if kind != mktdomain.KindCreative {
		return o
	}
	cur, err := a.Marketing.GetOption(ctx, p.Code)
	if err != nil {
		return o // 已下线 / 不在时段：不给推荐，规划照常（生成时仍按钉住的版本拼片段）
	}
	for _, m := range cur.RecommendedModels {
		if m.Available && m.Name != "" {
			o.RecommendedModels = append(o.RecommendedModels, m.Name)
		}
	}
	if rp, err := mktdomain.ParseRecommendedParams(cur.RecommendedParams); err == nil {
		o.AspectRatio, o.DurationSec = rp.AspectRatio, rp.DurationSec
	}
	return o
}

// ListMarketingOptions 实现 MarketingOptionLister：走模板墙同一个接口（只返回此刻在投的，MKD-13）。
// Kind 为空时三类各取一页。
func (a AgentSceneContext) ListMarketingOptions(ctx context.Context,
	q agentapp.MarketingOptionsQuery) (agentapp.MarketingOptionsResult, error) {
	kinds := mktdomain.Kinds
	if q.Kind != "" {
		kinds = []mktdomain.Kind{mktdomain.Kind(q.Kind)}
	}
	out := agentapp.MarketingOptionsResult{Items: []agentapp.MarketingOption{}}
	for _, k := range kinds {
		page, err := a.Marketing.ListOptions(ctx, marketingapp.OptionsQuery{
			Kind: k, Category: q.Category, Q: q.Q, Limit: q.Limit})
		if err != nil {
			return agentapp.MarketingOptionsResult{}, err
		}
		for _, o := range page.Items {
			out.Items = append(out.Items, agentapp.MarketingOption{
				Kind: string(o.Kind), Code: o.Code, Name: o.Name, Description: o.Description,
				CategoryCode: o.CategoryCode, Tags: o.Tags, IsAudioRequired: o.IsAudioRequired,
			})
		}
		if page.NextCursor != "" {
			out.More = true
		}
	}
	return out, nil
}

func (a AgentSceneContext) logger() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

var (
	_ agentapp.SceneContext          = AgentSceneContext{}
	_ agentapp.MarketingOptionLister = AgentSceneContext{}
)
