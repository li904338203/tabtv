package wiring

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"

	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 营销域的端口适配（MKT-T-03；dev/01 §2.2「谁要用谁定义端口」）。
//
// marketing 不 import catalog / agent / media：推荐模型、默认技能、封面媒体的事实由这里
// 从各域取来、翻成 marketing/app 自己定义的形状。

// MarketingAdminInput 是管理端营销服务的依赖。
type MarketingAdminInput struct {
	Repo    *marketingrepo.Repo
	Catalog *catalogapp.Service
	Agent   *agentrepo.Repo
	Media   *mediaapp.Service
	NextID  func() int64
	Audit   marketingapp.Auditor
}

// NewMarketingAdmin 装配管理端营销服务。
func NewMarketingAdmin(in MarketingAdminInput) *marketingapp.Admin {
	a := marketingapp.NewAdmin(in.Repo, MarketingModels{Catalog: in.Catalog},
		MarketingSkills{Agent: in.Agent}, MarketingMedia{Media: in.Media}, in.NextID)
	if in.Audit != nil {
		a.SetAudit(in.Audit)
	}
	return a
}

// MarketingModels 把模型目录接成 marketing/app.ModelLookup。
//
// 「是否支持对应产出」= 模型**启用的**操作绑定里，有没有 operation.out_node_type 落在
// 选项适用产出里的那一个（只看绑定、不看定价与能力矩阵：那是报价的事，这里只是给运营的提示）。
type MarketingModels struct{ Catalog *catalogapp.Service }

// LookupModels 实现 ModelLookup。
func (m MarketingModels) LookupModels(ctx context.Context, codes []string) (map[string]marketingapp.ModelFacts, error) {
	want := make(map[string]bool, len(codes))
	for _, c := range codes {
		want[c] = true
	}
	models, err := m.Catalog.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := m.Catalog.ModelBindings(ctx)
	if err != nil {
		return nil, err
	}
	ops, err := m.Catalog.ListOperations(ctx)
	if err != nil {
		return nil, err
	}
	outOf := make(map[string]string, len(ops))
	for _, o := range ops {
		if o.Status == "" || o.Status == "enabled" {
			outOf[o.Code] = o.OutNodeType
		}
	}
	out := make(map[string]marketingapp.ModelFacts)
	for _, x := range models {
		if !want[x.Code] {
			continue
		}
		f := marketingapp.ModelFacts{Code: x.Code, Name: x.Name, Domain: x.Domain, Enabled: x.Status == "enabled"}
		seen := map[string]bool{}
		for _, b := range bindings[x.Code] {
			if t := outOf[b.OperationCode]; b.IsEnabled && t != "" && !seen[t] {
				seen[t] = true
				f.OutputTypes = append(f.OutputTypes, t)
			}
		}
		out[x.Code] = f
	}
	return out, nil
}

// MarketingSkills 把 Agent 技能表接成 marketing/app.SkillLookup。
//
// 场景取 agent_skill.scenes（MKT-T-05）：不含 marketing 的技能在营销页的技能列表里不会出现。
type MarketingSkills struct{ Agent *agentrepo.Repo }

// LookupSkills 实现 SkillLookup。
func (s MarketingSkills) LookupSkills(ctx context.Context, codes []string) (map[string]marketingapp.SkillFacts, error) {
	out := make(map[string]marketingapp.SkillFacts, len(codes))
	for _, c := range codes {
		sk, err := s.Agent.GetSkillByCode(ctx, c)
		if errors.Is(err, agentrepo.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[c] = marketingapp.SkillFacts{Code: sk.Code, Enabled: sk.IsEnabled, ScenesKnown: true,
			Scenes: sk.SceneList()}
	}
	return out, nil
}

// MarketingMedia 把媒体域接成 marketing/app.MediaLookup。
//
// 只查平台级素材（workspace_id = 0）：管理端上传的封面不属于任何工作区。用户工作区的文件
// 在这里一律「查不到」—— 选项封面是公开的，引用用户的文件等于把它公开出去。
type MarketingMedia struct{ Media *mediaapp.Service }

// LookupMedia 实现 MediaLookup。
func (m MarketingMedia) LookupMedia(ctx context.Context, id int64) (marketingapp.MediaFacts, bool, error) {
	o, err := m.Media.Info(ctx, 0, id)
	if err != nil {
		// Info 对「不存在 / 不是平台素材 / 已删除」统一回 NotFound。
		return marketingapp.MediaFacts{}, false, nil
	}
	return marketingapp.MediaFacts{ID: o.ID, WorkspaceID: o.WorkspaceID, Purpose: o.Purpose,
		ContentType: o.ContentType}, true, nil
}

// MarketingCatalog 把模型目录接成 marketing/app.ModelCatalog（MKT-T-04）。
//
// ⚠️ 口径就是用户端 `/models?resultType=` 那一份（catalog.UsableModels，agentOnly=false）：
// 「按模型」筛选项与模型下拉必须对同一个模型给出同一个答案（MKT-AC-13）。
type MarketingCatalog struct{ Catalog *catalogapp.Service }

// UsableModels 实现 ModelCatalog。
func (m MarketingCatalog) UsableModels(ctx context.Context, outputType string) ([]marketingapp.ModelRef, error) {
	list, err := m.Catalog.UsableModels(ctx, outputType, false)
	if err != nil {
		return nil, err
	}
	out := make([]marketingapp.ModelRef, 0, len(list))
	for _, x := range list {
		out = append(out, marketingapp.ModelRef{Code: x.Code, Name: x.Name})
	}
	return out, nil
}

// MarketingServiceInput 是用户端营销服务的依赖。
type MarketingServiceInput struct {
	Repo     *marketingrepo.Repo
	Catalog  *catalogapp.Service
	Settings marketingapp.Settings
}

// NewMarketingService 装配用户端营销服务（配置、模板墙、详情、公开封面、Resolve / Snapshot）。
func NewMarketingService(in MarketingServiceInput) *marketingapp.Service {
	return marketingapp.NewService(in.Repo, MarketingCatalog{Catalog: in.Catalog},
		MarketingModels{Catalog: in.Catalog}, in.Settings)
}

// NewMarketingServiceFor 按配置装出用户端营销服务（Worker 侧的规划工具用，MKT-T-20）。
// 只用到 Resolve / Snapshot：不装提示词渲染（约束段在受理链路里由 newMarketingSource 那一份拼）。
func NewMarketingServiceFor(cfg config.Config, pool *pgxpool.Pool, catalog *catalogapp.Service) *marketingapp.Service {
	return NewMarketingService(MarketingServiceInput{
		Repo: marketingrepo.New(pool), Catalog: catalog,
		Settings: marketingapp.Settings{MaxProductImages: cfg.Marketing.MaxProductImages,
			MaxVariants: cfg.Marketing.MaxVariants, AgentEnabled: cfg.Agent.Enabled},
	})
}

// marketingAppendixAdapter 把营销域接成生成域的 MarketingSource（MKT-T-21、MKD-6）。
//
// ⚠️ 端口由**生成域**定义（谁定义端口谁拥有契约），同 presetAdapter：拼提示词是
// "这次生成长什么样"的一部分，营销域只负责按钉住的版本把原文与约束段取出来。
type marketingAppendixAdapter struct {
	svc *marketingapp.Service
	// pins 判断选项版本是否被本项目的营销会话确认过（04 文档 P-46）；nil = 不把关（只在测试里）。
	pins *agentrepo.Repo
}

func (a marketingAppendixAdapter) Appendix(ctx context.Context, scope genapp.MarketingScope,
	params json.RawMessage) (genapp.MarketingAppendix, error) {
	if err := a.checkPinned(ctx, scope, params); err != nil {
		return genapp.MarketingAppendix{}, err
	}
	r, err := a.svc.Appendix(ctx, params)
	if err != nil {
		return genapp.MarketingAppendix{}, err
	}
	return genapp.MarketingAppendix{Text: r.Text, Picks: r.Picks}, nil
}

// checkPinned 只放行本项目营销会话确认过的选项版本（04 文档 P-46）。
//
// ⚠️ 受理只按 (code, version) 取历史原文、不看是否在投 —— 那是对的（钉住的版本就是用户确认过的配方），
// 但它也意味着任何生成入口（POST /generations、画布节点、MCP、通用会话的 generate）只要在 params 里写上
// 一个 (code, version)，就能把那个选项的片段原文拼进自己的生成，草稿与已下线的版本也一样；
// 再让文本模型复述一遍就拿到了原文。确认过的才放行：营销项目里的手动重跑、受控重试照常可用。
// 一样都没选（picks 为空）只追加场景约束段，不涉及选项原文，不拦。
func (a marketingAppendixAdapter) checkPinned(ctx context.Context, scope genapp.MarketingScope,
	params json.RawMessage) error {
	if a.pins == nil {
		return nil
	}
	picks, present, err := marketingdomain.ParsePicks(params)
	if err != nil || !present || len(picks) == 0 {
		return nil // 形状错误由营销域报（同一条规则只写一处）
	}
	for _, p := range picks {
		ok, err := a.pins.PickPinnedInProject(ctx, scope.WorkspaceID, scope.ProjectID, string(p.Kind), p.Code, p.Version)
		if err != nil {
			return httpx.Wrap(errcode.Internal, err)
		}
		if !ok {
			return httpx.Errf(errcode.InvalidArgument,
				"营销选项 "+string(p.Kind)+":"+p.Code+" 不是本项目营销会话确认过的配方，不能用在这次生成里")
		}
	}
	return nil
}

// newMarketingSource 装出受理链路的营销段来源（BuildCore 内调用，API 与 Worker 同一处）。
//
// 约束段经目录域渲染（DB 行优先、代码兜底），不钉版本（dev/01 §12 #4）。
// ⚠️ 不看 SEEAITV_MARKETING_ENABLED：总开关只管入口（营销页、建会话）；关掉之后，
// 已经钉住版本的节点在画布上手动重跑、或运维受控重试，仍要拿到同一份配方。
func newMarketingSource(cfg config.Config, pool *pgxpool.Pool, catalog *catalogapp.Service) genapp.MarketingSource {
	svc := NewMarketingService(MarketingServiceInput{
		Repo: marketingrepo.New(pool), Catalog: catalog,
		Settings: marketingapp.Settings{MaxProductImages: cfg.Marketing.MaxProductImages,
			MaxVariants: cfg.Marketing.MaxVariants, AgentEnabled: cfg.Agent.Enabled},
	})
	svc.SetPrompts(catalog)
	return marketingAppendixAdapter{svc: svc, pins: agentrepo.New(pool)}
}
