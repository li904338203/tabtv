package wiring

// 画布 Agent 规划阶段免费工具（AGT-23）的端口适配器与装配。

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
)

// AgentNodeGenerations 把 generation/repo.LatestByNode 接成 agentapp.NodeGenerations。
type AgentNodeGenerations struct{ Repo *genrepo.Repo }

func (a AgentNodeGenerations) LatestByNode(ctx context.Context, ws, projectID int64) (
	map[string]agentapp.NodeGeneration, error) {
	rows, err := a.Repo.LatestByNode(ctx, ws, projectID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]agentapp.NodeGeneration, len(rows))
	for _, r := range rows {
		out[r.NodeID] = agentapp.NodeGeneration{GenerationID: r.GenerationID, Status: r.Status,
			ErrorCode: r.ErrorCode, OutputMediaID: r.OutputMediaID, OutputType: r.OutputType}
	}
	return out, nil
}

// AgentAssets 把素材库（media）与脚本资产库（script）接成 agentapp.Assets。
//
// ⚠️ 两边都按工作区查（ListMine / Library 自带 workspace 条件）。只回 ID / 名称 / 类型 / 缩略图：
// 资产描述与档案是外部内容且很长，不进模型上下文。
type AgentAssets struct {
	Media  *mediaapp.Service
	Script *scriptapp.Service
}

// maxAssetOffset 是脚本资产库翻页的上限（Library 一次最多取 200 条，按偏移量游标翻页）。
const maxAssetOffset = 180

func (a AgentAssets) ListAssets(ctx context.Context, ws int64, q agentapp.AssetQuery) (agentapp.AssetPage, error) {
	if q.Source == agentapp.AssetSourceMedia {
		var cursorID int64
		if q.Cursor != "" {
			v, err := strconv.ParseInt(q.Cursor, 10, 64)
			if err != nil || v <= 0 {
				return agentapp.AssetPage{}, fmt.Errorf("cursor 不合法")
			}
			cursorID = v
		}
		rows, more, err := a.Media.ListMine(ctx, ws, q.Kind, q.Limit, cursorID)
		if err != nil {
			return agentapp.AssetPage{}, err
		}
		page := agentapp.AssetPage{Items: make([]agentapp.AssetItem, 0, len(rows))}
		for _, m := range rows {
			kind := mediaapp.KindOf(m.ContentType)
			it := agentapp.AssetItem{ID: strconv.FormatInt(m.ID, 10), Type: kind}
			if kind == "image" {
				it.ThumbMediaID = it.ID
			}
			page.Items = append(page.Items, it)
		}
		if more && len(rows) > 0 {
			page.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
		}
		return page, nil
	}
	offset := 0
	if q.Cursor != "" {
		v, err := strconv.Atoi(q.Cursor)
		if err != nil || v < 0 || v > maxAssetOffset {
			return agentapp.AssetPage{}, fmt.Errorf("cursor 不合法")
		}
		offset = v
	}
	rows, err := a.Script.Library(ctx, ws, q.Source, "", offset+q.Limit+1)
	if err != nil {
		return agentapp.AssetPage{}, err
	}
	page := agentapp.AssetPage{Items: []agentapp.AssetItem{}}
	for i := offset; i < len(rows) && i < offset+q.Limit; i++ {
		r := rows[i]
		it := agentapp.AssetItem{ID: strconv.FormatInt(r.ID, 10), Name: r.Name, Type: r.Type}
		if r.MediaObjectID != 0 {
			it.ThumbMediaID = strconv.FormatInt(r.MediaObjectID, 10)
		}
		page.Items = append(page.Items, it)
	}
	if next := offset + q.Limit; len(rows) > next && next <= maxAssetOffset {
		page.NextCursor = strconv.Itoa(next)
	}
	return page, nil
}

// AgentSkillRenderer 用目录域的占位符规则渲染技能正文（缺变量即报错，不留字面量）。
type AgentSkillRenderer struct{}

func (AgentSkillRenderer) RenderSkill(content string, vars map[string]string) (string, error) {
	return catalogdomain.RenderPrompt(content, vars)
}

// AgentPlanningToolsInput 是规划工具的装配入参。
type AgentPlanningToolsInput struct {
	AgentRepo *agentrepo.Repo
	Canvas    *canvasapp.Service
	GenRepo   *genrepo.Repo
	Media     *mediaapp.Service
	Script    *scriptapp.Service
	Catalog   *catalogapp.Service
	Billing   *billingapp.Service
	// Marketing 营销会话规划（MKT-T-20）：选项钉版本与判定；nil = 营销会话的 submit_plan 报「未接入」。
	Marketing *marketingapp.Service
	// MaxVariants 营销会话单次变体上限（SEEAITV_MARKETING_MAX_VARIANTS，MKT-T-27）；≤ 0 按缺省 4。
	MaxVariants int
	Bus         BusPublisher
	NextID      func() int64
	Logger      *slog.Logger
}

// NewAgentPlanningTools 装出九个免费工具（read_canvas … read_skill_ref）。
//
// ⚠️ 报价用 AgentPlanQuoter —— 与 API 侧批准时重新报价（AGT-26）同一个实现。
func NewAgentPlanningTools(in AgentPlanningToolsInput) []agentapp.Tool {
	d := agenttools.PlanningDeps{
		Store:       in.AgentRepo,
		Canvas:      AgentCanvas{in.Canvas},
		Generations: AgentNodeGenerations{in.GenRepo},
		Assets:      AgentAssets{Media: in.Media, Script: in.Script},
		Pricer:      AgentPlanQuoter{Catalog: in.Catalog},
		Ops:         AgentOperationCatalog{Catalog: in.Catalog},
		Balances:    AgentAccounts{Billing: in.Billing},
		Documents:   AgentDocuments{in.Canvas},
		Skills:      AgentSkillRenderer{},
		MaxVariants: in.MaxVariants,
		NextID:      in.NextID,
		Logger:      in.Logger,
	}
	if in.Bus != nil {
		d.Events = AgentEvents{in.Bus}
	}
	if in.Marketing != nil && in.Catalog != nil {
		d.Scene = AgentPlanScene{Catalog: in.Catalog, Marketing: in.Marketing}
	}
	return agenttools.PlanningTools(d)
}

var (
	_ agentapp.NodeGenerations = AgentNodeGenerations{}
	_ agentapp.Assets          = AgentAssets{}
	_ agentapp.PlanPricer      = AgentPlanQuoter{}
	_ agentapp.Balances        = AgentAccounts{}
	_ agentapp.SkillRenderer   = AgentSkillRenderer{}
)

// NewAgentMarketingTools 装出营销场景的只读工具（MKT-T-18）：list_marketing_options。
// 只在营销会话下发（domain.toolSpecs 的 Scenes），注册与否不影响通用会话的工具列表。
func NewAgentMarketingTools(sc AgentSceneContext) []agentapp.Tool {
	return []agentapp.Tool{agenttools.ListMarketingOptions{Options: sc}}
}
