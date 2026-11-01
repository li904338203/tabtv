package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 3D 导演台工具（docs/agent/director，DRT-29，dev/01 §3.12~3.13）：director_create 建节点与场景，
// director_build 对导演台做 AI 搭景并直接写入场景。
//
// ⚠️ 两个都**免费**（DRK-4），所以不走 PaidCall、不占额度；但都写画布 / 场景，只在执行态下发（DRD-32）。
// ⚠️ **不能截图**（DRK-10）：截图只能在浏览器里生成。搭完要提醒用户打开导演台截图，下游才有输出（DR-F-162）。
// ⚠️ 搭景的业务错误（限流、方案无法解析、不可用、写入冲突）原样交给模型转述（DR-F-164），见 directorError。
// 「建节点 → 建场景 → 写回」与「按节点取场景（自愈）」在 internal/canvasgen，与 MCP 共用。

// ---------------------------------------------------------------- director_create

// DirectorCreate 是 director_create：先在画布上建导演台节点，再按节点建场景，把 sceneId 写回节点。
type DirectorCreate struct{ d MultiDeps }

func (DirectorCreate) Name() domain.ToolName { return domain.ToolDirectorCreate }
func (DirectorCreate) Paid() bool            { return false }
func (DirectorCreate) Action() collab.Action { return collab.ActionEdit }

func (DirectorCreate) Description() string {
	return "在画布上新建一个 3D 导演台节点并建好场景（免费）。导演台用素模、道具和机位摆出构图，截图后交给下游的" +
		"图片 / 视频节点当构图参考。返回 nodeId 与 sceneId，之后用 director_build 搭景。" +
		"你不能截图：搭完要提醒用户打开导演台截图，下游才有输出。"
}

func (DirectorCreate) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"title":{"type":"string","description":"节点标题（默认「3D 导演台」）"},` +
		`"position":{"type":"object","properties":{"x":{"type":"number"},"y":{"type":"number"}}}}}`)
}

type directorCreateArgs struct {
	Title    string    `json:"title"`
	Position *position `json:"position"`
}

func (t DirectorCreate) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolDirectorCreate); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Canvas == nil || t.d.Directors == nil {
		return app.ToolResult{}, errors.New("导演台服务未接入")
	}
	var a directorCreateArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	var pos *canvasgen.Position
	if a.Position != nil {
		pos = &canvasgen.Position{X: a.Position.X, Y: a.Position.Y}
	}
	o := objectOrch(t.d.Canvas)
	o.Logger = t.d.logger()
	nodeID, sceneID, err := canvasgen.CreateDirectorNode(ctx, o, multiCaller(rc),
		canvasgen.DirectorNodeInput{Title: a.Title, Position: pos}, genDirectorScenes{t.d.Directors})
	var oe *canvasgen.ObjectError
	if errors.As(err, &oe) {
		return app.ToolResult{}, fmt.Errorf("导演台节点 %s 已建好，但建场景失败：%v。不要再建一个：对它调用 director_build "+
			"时会先补建场景，用户打开导演台时也会自动补上", oe.NodeID, oe.Err)
	}
	if err != nil {
		return app.ToolResult{}, ensureNodeError(err)
	}
	content := map[string]any{"nodeId": nodeID, "sceneId": idS(sceneID),
		"next": "用 director_build 搭景；搭完请提醒用户打开导演台截图"}
	return app.ToolResult{Content: ptJSON(content),
		Display: ptJSON(map[string]any{"nodeId": nodeID, "sceneId": idS(sceneID)}), Outputs: 1}, nil
}

// ---------------------------------------------------------------- director_build

// DirectorBuild 是 director_build：按节点取场景（缺了先补建），AI 搭景并以乐观锁写入（来源 agent）。
type DirectorBuild struct{ d MultiDeps }

func (DirectorBuild) Name() domain.ToolName { return domain.ToolDirectorBuild }
func (DirectorBuild) Paid() bool            { return false }
func (DirectorBuild) Action() collab.Action { return collab.ActionEdit }

func (DirectorBuild) Description() string {
	return "对 3D 导演台节点做 AI 搭景（免费，但按用户限流：每小时有次数上限、同时只能跑一个）：按描述（与可选的参考图）" +
		"摆放角色、道具与机位，直接写入场景；用户手摆的、锁定的对象会保留。节点还没有场景时先补建。" +
		"搭完一定要把结果里的 notice 告诉用户：需要用户打开导演台截图，下游才有输出。" +
		"被限流或方案用不了时，结果里有原因（限流还有 retry_after_seconds），原样转告用户，不要立刻重试。"
}

func (DirectorBuild) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["nodeId","prompt"],"properties":{` +
		`"nodeId":{"type":"string","description":"导演台节点 ID（director_create 返回的 nodeId，或 read_canvas 里 type 为 director 的节点）"},` +
		`"prompt":{"type":"string","description":"想要搭建的场景：有哪些人物、谁在谁的左右前后、朝向与姿势、机位的景别与角度"},` +
		`"referenceMediaIds":{"type":"array","items":{"type":"string"},"description":"参考图的媒体 ID（本工作区的图片，按顺序编号为图 1、图 2……，张数有上限）"}}}`)
}

type directorBuildArgs struct {
	NodeID            string   `json:"nodeId"`
	Prompt            string   `json:"prompt"`
	ReferenceMediaIDs []flexID `json:"referenceMediaIds"`
}

func (t DirectorBuild) Run(ctx context.Context, rc app.RunContext, raw json.RawMessage) (app.ToolResult, error) {
	if err := requireExecuting(rc, domain.ToolDirectorBuild); err != nil {
		return app.ToolResult{}, err
	}
	if t.d.Canvas == nil || t.d.Directors == nil {
		return app.ToolResult{}, errors.New("导演台服务未接入")
	}
	var a directorBuildArgs
	if err := multiDecode(raw, &a); err != nil {
		return app.ToolResult{}, err
	}
	a.NodeID, a.Prompt = strings.TrimSpace(a.NodeID), strings.TrimSpace(a.Prompt)
	if a.NodeID == "" {
		return app.ToolResult{}, errors.New("nodeId 必填：要搭景的导演台节点（director_create 返回的 nodeId）")
	}
	if a.Prompt == "" {
		return app.ToolResult{}, errors.New("prompt 不能为空：描述想要搭建的场景（人物、站位、机位）")
	}
	o := objectOrch(t.d.Canvas)
	o.Logger = t.d.logger()
	sceneID, repair, err := canvasgen.DirectorSceneOfNode(ctx, o, multiCaller(rc), a.NodeID,
		genDirectorScenes{t.d.Directors})
	if err != nil {
		return app.ToolResult{}, directorNodeError(a.NodeID, err)
	}
	out, err := t.d.Directors.BuildAndApply(ctx, app.DirectorBuildRequest{
		WorkspaceID: rc.WorkspaceID, ProjectID: rc.ProjectID, UserID: rc.UserID, SessionID: rc.SessionID,
		Role: rc.Role, MemberLevelID: rc.MemberLevelID, SceneID: sceneID, Prompt: a.Prompt,
		ReferenceMediaIDs: ids64(a.ReferenceMediaIDs),
	})
	if err != nil {
		return app.ToolResult{}, directorError("AI 搭景", err)
	}
	content := map[string]any{"nodeId": a.NodeID, "sceneId": idS(out.SceneID), "version": out.Version,
		"added": nonNilIDs(out.Added), "updated": nonNilIDs(out.Updated), "removed": nonNilIDs(out.Removed),
		"defaults": nonNilIDs(out.Defaults), "notice": out.Notice,
		"next": "把 notice 告诉用户：你不能截图，用户打开导演台截图之后，下游节点才能拿它生成"}
	if repair != "" {
		content["repair"] = repair
	}
	display := map[string]any{"nodeId": a.NodeID, "sceneId": idS(out.SceneID), "version": out.Version,
		"added": len(out.Added), "updated": len(out.Updated), "removed": len(out.Removed), "notice": out.Notice}
	return app.ToolResult{Content: ptJSON(content), Display: ptJSON(display), Outputs: 1}, nil
}

func nonNilIDs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// directorNodeError 把「按节点取场景」的错误翻成给模型的话：节点不存在、不是导演台说清该怎么办；
// 建场景的业务错误走 directorError；其余（读画布失败等）照常报错。补写 sceneId 失败只告警，不会到这里。
func directorNodeError(nodeID string, err error) error {
	var nd *canvasgen.NotDirectorError
	switch {
	case errors.Is(err, canvasgen.ErrNodeNotFound):
		return fmt.Errorf("画布上没有节点 %s：先用 read_canvas 确认，或用 director_create 新建一个导演台", nodeID)
	case errors.As(err, &nd):
		return fmt.Errorf("节点 %s 是 %s，不是导演台：只能对导演台节点搭景，先用 director_create 新建一个", nodeID, nd.Type)
	}
	return directorError("取导演台的场景", err)
}

// directorError 把导演台的业务错误原样交给模型（DR-F-164）：错误码、原因与详情（限流的 retry_after_seconds /
// scope / limit、不可用的 userFixable、解析失败的 reason）都保留，再附一句该怎么做 —— 模型据此转述，
// 而不是一遍遍重试。步骤的 error_code 记错误码名（stepTagOf 取 ToolRejection.Code）。不是业务错误的照常报错。
func directorError(what string, err error) error {
	var be *httpx.BizError
	if !errors.As(err, &be) {
		return fmt.Errorf("%s失败：%v", what, err)
	}
	details := make(map[string]any, len(be.Details)+2)
	for k, v := range be.Details {
		details[k] = v
	}
	details["code"] = int(be.Code)
	if next := directorNext(be); next != "" {
		details["next"] = next
	}
	code := be.Code.Name()
	if code == "" {
		code = errcode.Internal.Name()
	}
	return &domain.ToolRejection{Code: code, Message: be.Error(), Details: details}
}

// directorNext 是各类错误该怎么做（给模型的指引，原因本身在 message 里）。
func directorNext(be *httpx.BizError) string {
	switch be.Code {
	case errcode.RateLimited:
		return "把原因与需要等待的时间（retry_after_seconds 秒）告诉用户；到时间之前不要再调用 director_build"
	case errcode.ConcurrencyLimited:
		return "上一次搭景还没结束：等 retry_after_seconds 秒再试，不要连续重试"
	case errcode.DirectorDraftInvalid:
		return "这次的搭景方案用不了：换个更具体的说法（有哪些人物、谁在谁的哪一边、机位景别）再调用一次"
	case errcode.CapabilityUnsupported:
		if fixable, _ := be.Details["userFixable"].(bool); fixable {
			return "AI 搭景现在用不了，但用户自己能解决：把原因告诉用户，不要重试"
		}
		return "AI 搭景现在用不了，需要运营处理：把原因告诉用户，不要重试；用户仍可以打开导演台手动摆位"
	case errcode.VersionConflict:
		return "场景刚被用户或别人改过，这次搭景没有写入（不会覆盖别人的改动）：告诉用户，等他确认后再调用一次"
	}
	return ""
}

// ---------------------------------------------------------------- Agent 端口 → canvasgen 端口（私有适配器）

// genDirectorScenes 把 agent 的 Directors 端口包成 canvasgen.DirectorScenes（同 genCanvasPort）。
type genDirectorScenes struct{ d app.Directors }

func (p genDirectorScenes) Create(ctx context.Context, c canvasgen.Caller, nodeID string, source int64) (int64, error) {
	return p.d.CreateScene(ctx, app.DirectorSceneRequest{
		WorkspaceID: c.WorkspaceID, ProjectID: c.ProjectID, UserID: c.UserID, SessionID: c.Origin.SessionID,
		Role: c.Role, CanvasNodeID: nodeID, SourceSceneID: source,
	})
}

func (p genDirectorScenes) SceneNode(ctx context.Context, c canvasgen.Caller, sceneID int64) (string, error) {
	nodeID, err := p.d.SceneNode(ctx, c.WorkspaceID, c.ProjectID, sceneID, c.Role)
	if errors.Is(err, app.ErrDirectorSceneNotFound) {
		return "", canvasgen.ErrDirectorSceneNotFound
	}
	return nodeID, err
}
