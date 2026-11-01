package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// list_assets（dev/01 §5.3）：素材库 / 角色·场景·道具资产库，分页，只回 ID / 名称 / 类型 / 缩略图。
// 描述与档案是外部内容且很长，不进模型上下文；要用哪个资产按 ID 引用即可。

const (
	maxAssetLimit     = 20
	defaultAssetLimit = 20
)

var assetSources = map[string]bool{
	app.AssetSourceMedia: true, app.AssetSourceCharacter: true,
	app.AssetSourceScene: true, app.AssetSourceProp: true,
}

var mediaKinds = map[string]bool{"": true, "image": true, "video": true, "audio": true}

// ListAssets 是 list_assets 工具。
type ListAssets struct{ Assets app.Assets }

func (ListAssets) Name() domain.ToolName { return domain.ToolListAssets }
func (ListAssets) Paid() bool            { return false }
func (ListAssets) Action() collab.Action { return collab.ActionView }

func (ListAssets) Description() string {
	return "列出本工作区的素材：source=media 为上传的参考素材（可按 kind 筛 image/video/audio），" +
		"character/scene/prop 为角色、场景、道具资产库。只返回 ID、名称、类型与缩略图媒体 ID；用 nextCursor 翻页。"
}

func (ListAssets) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"source":{"type":"string","enum":["media","character","scene","prop"],"description":"默认 media"},` +
		`"kind":{"type":"string","enum":["image","video","audio"],"description":"只对 media 有效"},` +
		`"cursor":{"type":"string","description":"上一页返回的 nextCursor"},` +
		`"limit":{"type":"integer","minimum":1,"maximum":20}` +
		`},"additionalProperties":false}`)
}

type listAssetsArgs struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

func (t ListAssets) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	var a listAssetsArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	if a.Source == "" {
		a.Source = app.AssetSourceMedia
	}
	if !assetSources[a.Source] {
		return ToolResult{}, fmt.Errorf("source 只能是 media / character / scene / prop")
	}
	if !mediaKinds[a.Kind] {
		return ToolResult{}, fmt.Errorf("kind 只能是 image / video / audio")
	}
	if a.Kind != "" && a.Source != app.AssetSourceMedia {
		return ToolResult{}, fmt.Errorf("kind 只对 source=media 有效")
	}
	if a.Limit <= 0 {
		a.Limit = defaultAssetLimit
	}
	if a.Limit > maxAssetLimit {
		a.Limit = maxAssetLimit
	}
	page, err := t.Assets.ListAssets(ctx, rc.WorkspaceID, app.AssetQuery{
		Source: a.Source, Kind: a.Kind, Cursor: a.Cursor, Limit: a.Limit})
	if err != nil {
		return ToolResult{}, fmt.Errorf("查询素材失败：%w", err)
	}
	if page.Items == nil {
		page.Items = []app.AssetItem{}
	}
	// 只回四个字段：适配器给多了也在这里收窄（名称截到 40 字，外部内容不可信）。
	for i := range page.Items {
		page.Items[i].Name, _ = ptCutRunes(page.Items[i].Name, 40)
	}
	return ToolResult{
		Content: ptJSON(page),
		Display: ptJSON(map[string]any{"source": a.Source, "count": len(page.Items)}),
	}, nil
}

var _ app.Tool = ListAssets{}
