package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 3D 导演台（docs/agent/director，DRT-29，DR-F-163 / DR-I-06）：director_create 建导演台节点与场景，
// director_build 对导演台做 AI 搭景并直接写入场景（来源 mcp）。
//
// ⚠️ 两个都**不花积分**（DRK-4），但都写画布 / 场景：画布工具集、Write + edit（与 canvas_apply 同形的单方法工具），
// 只读端点不可见（domain.Visible 自动挡）。director_build 调一次看图的文本模型，注解如实标 openWorldHint。
// ⚠️ 服务端**不能截图**（DRK-10）：结果固定带 notice，下游要等用户在浏览器里截图之后才有输入。
// ⚠️ 业务错误原样回给客户端（限流 42901 / 42902 带 retry_after_seconds，DR-F-164）。

// maxDirectorPromptRunes 与 director/app.MaxPromptChars 一致（那边也会拦，这里先给一句看得懂的话）。
// 参考图张数不在这里判：上限要与所绑模型取小（DR-PARAM-14），只有搭景服务知道。
const maxDirectorPromptRunes = 2000

type directorCreateIn struct {
	ProjectID      string         `json:"projectId" jsonschema:"项目 ID"`
	Title          string         `json:"title,omitempty" jsonschema:"节点标题（默认「3D 导演台」）"`
	Position       *canvasPointIn `json:"position,omitempty" jsonschema:"节点位置；不给则由服务端排在空位"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty" jsonschema:"幂等键：重试时带同一个值只会建出一个导演台节点"`
}

type directorCreateOut struct {
	ProjectID string `json:"projectId"`
	NodeID    string `json:"nodeId" jsonschema:"新建的导演台节点"`
	SceneID   string `json:"sceneId" jsonschema:"节点的场景（已写回节点的 params.sceneId）"`
	Message   string `json:"message"`
	links     []app.ResourceLink
}

func (o directorCreateOut) ResourceLinks() []app.ResourceLink { return o.links }

type directorBuildIn struct {
	ProjectID         string   `json:"projectId" jsonschema:"项目 ID"`
	NodeID            string   `json:"nodeId" jsonschema:"导演台节点 ID（director_create 返回的 nodeId，或 canvas_read 里类型为 director 的节点）"`
	Prompt            string   `json:"prompt" jsonschema:"想要搭建的场景：有哪些人物、谁在谁的左右前后、朝向与姿势、机位的景别与角度（最多 2000 字）"`
	ReferenceMediaIDs []string `json:"referenceMediaIds,omitempty" jsonschema:"参考图的媒体 ID（本工作区的图片，按顺序编号为图 1、图 2……，张数有上限）"`
}

type directorBuildOut struct {
	ProjectID string   `json:"projectId"`
	NodeID    string   `json:"nodeId"`
	SceneID   string   `json:"sceneId"`
	Version   int      `json:"version" jsonschema:"写入后的场景版本号"`
	Added     []string `json:"added" jsonschema:"新增的对象（场景里的对象 ID）"`
	Updated   []string `json:"updated" jsonschema:"修改的对象"`
	Removed   []string `json:"removed" jsonschema:"删除的对象（只会是 AI 上一次摆的、用户没动过的）"`
	Defaults  []string `json:"defaults" jsonschema:"描述里没说、按默认值补上的部分（转告用户）"`
	Notice    string   `json:"notice" jsonschema:"固定提醒：需要用户打开导演台截图后，下游才有输出"`
	Repair    string   `json:"repair,omitempty" jsonschema:"这次顺带给节点补上 / 分出了自己的场景时的说明"`
	links     []app.ResourceLink
}

func (o directorBuildOut) ResourceLinks() []app.ResourceLink { return o.links }

const directorCreateDescription = "在项目画布上新建一个 3D 导演台节点并建好它的场景（不花积分）。" +
	"导演台用素模、道具和机位摆出构图，截图后交给下游的图片 / 视频节点当构图参考。返回 nodeId 与 sceneId，" +
	"之后用 director_build 搭景。服务端不能截图：需要用户在 seeAitv 里打开导演台截图，下游才有输出。" +
	"重试时带同一个 idempotencyKey，避免建出两个节点。"

const directorBuildDescription = "对 3D 导演台节点做 AI 搭景（不花积分，但按用户限流：每小时有次数上限、同时只能跑一个）：" +
	"按描述（与可选的参考图）摆放角色、道具与机位，直接写入场景；用户手摆的、锁定的对象会保留。" +
	"节点还没有场景时先补建。与用户的编辑撞上时只重做一次，仍冲突就报错、不覆盖用户的改动。" +
	"搭完请把 notice 转告用户：需要用户打开导演台截图，下游才有输出。" +
	"被限流时返回 RATE_LIMITED / CONCURRENCY_LIMITED 与 retry_after_seconds，到时间之前不要重试。"

func directorCreateTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "director_create",
		Toolset:     domain.ToolsetCanvas,
		Description: directorCreateDescription,
		// 不破坏：只新建节点与场景。
		Methods: []app.Method{{Action: collab.ActionEdit, Write: true}},
	}, func(ctx context.Context, _ string, in directorCreateIn) (directorCreateOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return directorCreateOut{}, "", err
		}
		pid, err := parseID("projectId", in.ProjectID)
		if err != nil {
			return directorCreateOut{}, "", err
		}
		switch {
		case utf8.RuneCountInString(in.Title) > maxTitleRunes:
			return directorCreateOut{}, "", httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("title 最多 %d 字", maxTitleRunes))
		case len(in.IdempotencyKey) > 200:
			return directorCreateOut{}, "", httpx.Errf(errcode.InvalidArgument, "idempotencyKey 最多 200 字符")
		}
		if d.Directors == nil {
			return directorCreateOut{}, "", unwired("Directors")
		}
		var pos *canvasgen.Position
		if in.Position != nil {
			pos = &canvasgen.Position{X: in.Position.X, Y: in.Position.Y}
		}
		batch := domain.ObjectBatchID(p.GrantID, strings.TrimSpace(in.IdempotencyKey), randomSuffix())
		nodeID, sceneID, err := d.Directors.Create(ctx, p, app.DirectorCreateInput{ProjectID: pid,
			Title: strings.TrimSpace(in.Title), Position: pos, BatchID: batch})
		if err != nil {
			return directorCreateOut{}, "", objectError(err, "场景")
		}
		out := directorCreateOut{ProjectID: idStr(pid), NodeID: nodeID, SceneID: idStr(sceneID),
			Message: "导演台已建好（0 积分）。下一步用 director_build 搭景；搭完提醒用户打开导演台截图。",
			links:   []app.ResourceLink{canvasLink(pid)}}
		return out, fmt.Sprintf("已建导演台节点 %s（场景 %s）", nodeID, out.SceneID), nil
	})
}

func directorBuildTool(d Deps) app.Tool {
	return app.Define(app.Tool{
		Name:        "director_build",
		Toolset:     domain.ToolsetCanvas,
		Description: directorBuildDescription,
		// 不破坏：只替换 AI 上一次摆的、用户没动过的对象（DR-F-105）；调一次外部模型但不花钱。
		Methods: []app.Method{{Action: collab.ActionEdit, Write: true, OpenWorld: true}},
	}, func(ctx context.Context, _ string, in directorBuildIn) (directorBuildOut, string, error) {
		p, err := principal(ctx)
		if err != nil {
			return directorBuildOut{}, "", err
		}
		pid, err := parseID("projectId", in.ProjectID)
		if err != nil {
			return directorBuildOut{}, "", err
		}
		nodeID, prompt := strings.TrimSpace(in.NodeID), strings.TrimSpace(in.Prompt)
		switch {
		case nodeID == "":
			return directorBuildOut{}, "", httpx.Errf(errcode.InvalidArgument, "nodeId 必填：要搭景的导演台节点")
		case prompt == "":
			return directorBuildOut{}, "", httpx.Errf(errcode.InvalidArgument, "prompt 不能为空：描述想要搭建的场景")
		case utf8.RuneCountInString(prompt) > maxDirectorPromptRunes:
			return directorBuildOut{}, "", httpx.Errf(errcode.InvalidArgument,
				fmt.Sprintf("prompt 最多 %d 字", maxDirectorPromptRunes))
		}
		refs, err := parseIDs("referenceMediaIds", in.ReferenceMediaIDs)
		if err != nil {
			return directorBuildOut{}, "", err
		}
		if d.Directors == nil {
			return directorBuildOut{}, "", unwired("Directors")
		}
		res, err := d.Directors.Build(ctx, p, app.DirectorBuildInput{ProjectID: pid, NodeID: nodeID, Prompt: prompt,
			ReferenceMediaIDs: refs, BatchID: domain.ObjectBatchID(p.GrantID, "", randomSuffix())})
		if err != nil {
			return directorBuildOut{}, "", directorNodeError(nodeID, err)
		}
		out := directorBuildOut{ProjectID: idStr(pid), NodeID: nodeID, SceneID: idStr(res.SceneID), Version: res.Version,
			Added: nonNil(res.Added), Updated: nonNil(res.Updated), Removed: nonNil(res.Removed),
			Defaults: nonNil(res.Defaults), Notice: res.Notice, Repair: res.Repair,
			links: []app.ResourceLink{canvasLink(pid)}}
		return out, fmt.Sprintf("已搭景：新增 %d、修改 %d、删除 %d 个对象（场景 v%d）。%s", len(out.Added), len(out.Updated),
			len(out.Removed), res.Version, res.Notice), nil
	})
}

// directorNodeError 把「按节点取场景」的错误说清楚：节点不存在 404、不是导演台 400；其余（含限流）原样返回。
func directorNodeError(nodeID string, err error) error {
	var nd *canvasgen.NotDirectorError
	switch {
	case errors.Is(err, canvasgen.ErrNodeNotFound):
		return httpx.Errf(errcode.NotFound, "画布上没有节点 "+nodeID+"：先用 canvas_read 确认，或用 director_create 新建一个导演台")
	case errors.As(err, &nd):
		return httpx.Errf(errcode.InvalidArgument, "节点 "+nodeID+" 是 "+nd.Type+"，不是导演台：只能对导演台节点搭景")
	}
	return objectError(err, "场景")
}
