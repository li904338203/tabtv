package tools

import (
	"context"
	"fmt"
	"strings"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// image_tools：图片工具栏（设定图 / 打光 / 高清 / 抠图……）的可用性与单价，**复用 catalog.ImageTools**
// （它逐个工具走报价路径，与受理同一个答案，CD-48）。

type imageToolsIn struct {
	ModelCode  string `json:"modelCode,omitempty" jsonschema:"要用的图片模型代码；不给则只判断输入是否满足"`
	ImageCount int    `json:"imageCount,omitempty" jsonschema:"当前连着几张图（含蒙版）"`
	HasText    bool   `json:"hasText,omitempty" jsonschema:"是否有文字描述"`
}

type imageToolOut struct {
	Code             string `json:"code" jsonschema:"预设代码（生成时作为 params.preset）"`
	Name             string `json:"name"`
	Group            string `json:"group"`
	Spec             string `json:"spec,omitempty"`
	InputMode        string `json:"inputMode" jsonschema:"image_required / text_required / either"`
	Operation        string `json:"operation,omitempty"`
	Available        bool   `json:"available"`
	BlockedCode      string `json:"blockedCode,omitempty"`
	BlockedReason    string `json:"blockedReason,omitempty"`
	UserFixable      bool   `json:"userFixable,omitempty" jsonschema:"用户自己能解决（连一张图 / 写描述）"`
	Credits          int    `json:"credits,omitempty" jsonschema:"单次积分（以 quote 为准）"`
	EstimatedSeconds int    `json:"estimatedSeconds,omitempty"`
}

type imageToolsOut struct {
	Tools []imageToolOut `json:"tools"`
}

func imageToolsTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:    "image_tools",
		Toolset: domain.ToolsetCatalog,
		Description: "图片加工工具（设定图、质感、打光、高清、全景、多角度、涂抹重绘、抠图等）在给定模型与输入下" +
			"能否使用、单价与不可用原因。不花积分。",
		Methods: []app.Method{{Action: collab.ActionView}},
	}, func(ctx context.Context, _ string, in imageToolsIn) (imageToolsOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return imageToolsOut{}, "", err
		}
		if d.Catalog == nil {
			return imageToolsOut{}, "", unwired("Catalog")
		}
		if in.ImageCount < 0 || in.ImageCount > 16 {
			return imageToolsOut{}, "", httpx.Errf(errcode.InvalidArgument, "imageCount 取值 0~16")
		}
		list, err := d.Catalog.ImageTools(ctx, p, app.ImageToolQuery{ModelCode: strings.TrimSpace(in.ModelCode),
			ImageCount: in.ImageCount, HasText: in.HasText})
		if err != nil {
			return imageToolsOut{}, "", err
		}
		out := imageToolsOut{Tools: make([]imageToolOut, 0, len(list))}
		avail := 0
		for _, t := range list {
			out.Tools = append(out.Tools, imageToolOut{Code: t.Code, Name: t.Name, Group: t.PresetGroup, Spec: t.Spec,
				InputMode: t.InputMode, Operation: t.Operation, Available: t.Available, BlockedCode: t.BlockedCode,
				BlockedReason: t.BlockedReason, UserFixable: t.UserFixable, Credits: t.Credits,
				EstimatedSeconds: t.EstimatedSeconds})
			if t.Available {
				avail++
			}
		}
		return out, fmt.Sprintf("%d 个图片工具，%d 个当前可用", len(out.Tools), avail), nil
	})
}
