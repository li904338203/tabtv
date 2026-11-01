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

// models（MCP-F-27）：**不可用的模型照样列出并写原因**。只列能用的话，外部模型会以为平台没有
// 这个模型 —— 而其实只差会员等级或运营配一个价。单价与可用性都来自服务端报价那条路径。

type modelsIn struct {
	ResultType string `json:"resultType,omitempty" jsonschema:"list：按产出类型筛（text / image / video / audio）"`
	Operation  string `json:"operation,omitempty" jsonschema:"list：按操作码筛（如 text2img、img2video）"`
	Code       string `json:"code,omitempty" jsonschema:"get：模型代码"`
}

type modelOperationOut struct {
	Code             string `json:"code" jsonschema:"操作码"`
	ResultType       string `json:"resultType,omitempty"`
	Available        bool   `json:"available"`
	BlockedCode      string `json:"blockedCode,omitempty" jsonschema:"不可用的原因码（业务码名）"`
	BlockedReason    string `json:"blockedReason,omitempty"`
	UserFixable      bool   `json:"userFixable,omitempty" jsonschema:"用户自己能解决（升级会员 / 调整参数）；否则需要运营处理"`
	Credits          int    `json:"credits,omitempty" jsonschema:"单次最小输入的报价积分（以 quote 为准）"`
	Unit             string `json:"unit,omitempty" jsonschema:"计价单位：per_time / per_image / per_second / per_kchar"`
	EstimatedSeconds int    `json:"estimatedSeconds,omitempty"`
}

type modelOut struct {
	Code             string              `json:"code"`
	Name             string              `json:"name"`
	ResultType       string              `json:"resultType"`
	Status           string              `json:"status" jsonschema:"enabled / disabled"`
	MinMemberLevelID string              `json:"minMemberLevelId,omitempty" jsonschema:"需要的最低会员等级 ID"`
	MaxDurationSec   *int                `json:"maxDurationSec,omitempty" jsonschema:"视频单次最长时长（秒）"`
	Available        bool                `json:"available" jsonschema:"至少一个操作现在能用"`
	BlockedCode      string              `json:"blockedCode,omitempty"`
	BlockedReason    string              `json:"blockedReason,omitempty"`
	Operations       []modelOperationOut `json:"operations"`
}

type modelsOut struct {
	Models []modelOut `json:"models"`
}

func modelsTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:    "models",
		Toolset: domain.ToolsetCatalog,
		Description: "模型目录。list：列出全部模型及其各操作能否使用、单价与不可用原因（不可用的也会列出）；" +
			"get：取单个模型。实际扣费以 quote 报价为准。不花积分。",
		Methods: []app.Method{{Name: "list", Action: collab.ActionView}, {Name: "get", Action: collab.ActionView}},
	}, func(ctx context.Context, method string, in modelsIn) (modelsOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return modelsOut{}, "", err
		}
		if d.Catalog == nil {
			return modelsOut{}, "", unwired("Catalog")
		}
		f := app.ModelFilter{ResultType: strings.TrimSpace(in.ResultType), Operation: strings.TrimSpace(in.Operation)}
		if method == "get" {
			if strings.TrimSpace(in.Code) == "" {
				return modelsOut{}, "", httpx.Errf(errcode.InvalidArgument, "get 需要 code")
			}
			f = app.ModelFilter{Code: strings.TrimSpace(in.Code)}
		}
		list, err := d.Catalog.Models(ctx, p, f)
		if err != nil {
			return modelsOut{}, "", err
		}
		if method == "get" && len(list) == 0 {
			return modelsOut{}, "", httpx.Errf(errcode.NotFound, "模型 "+f.Code+" 不存在")
		}
		out := modelsOut{Models: make([]modelOut, 0, len(list))}
		avail := 0
		for _, m := range list {
			mo := modelOut{Code: m.Code, Name: m.Name, ResultType: m.Domain, Status: m.Status,
				MinMemberLevelID: idStr(m.MinMemberLevelID), MaxDurationSec: m.MaxDurationSec,
				Available: m.Available, BlockedCode: m.BlockedCode, BlockedReason: m.BlockedReason,
				Operations: make([]modelOperationOut, 0, len(m.Operations))}
			for _, o := range m.Operations {
				mo.Operations = append(mo.Operations, modelOperationOut{Code: o.Code, ResultType: o.Domain,
					Available: o.Available, BlockedCode: o.BlockedCode, BlockedReason: o.BlockedReason,
					UserFixable: o.UserFixable, Credits: o.Credits, Unit: o.Unit, EstimatedSeconds: o.EstimatedSeconds})
			}
			if m.Available {
				avail++
			}
			out.Models = append(out.Models, mo)
		}
		return out, fmt.Sprintf("%d 个模型，其中 %d 个当前可用（不可用的见 operations[].blockedReason）",
			len(out.Models), avail), nil
	})
}
