// Package app 编排 3D 导演台的 AI 搭景（docs/agent/director，DRT-19，DRD-22~26）。
//
// 场景存在画布域（DRD-11），这里经端口读写；模型、目录、图片、提示词也都经端口接进来（谁定义谁拥有契约）。
//
// ⚠️ AI 搭景**免费**（DRK-4）：这个包不依赖账本（`TestDirectorApp_不依赖账本` 用 go list -deps 钉住）。
// 免费靠配置成立（scene_build 的兜底价 0 积分），运营改成非 0 时显式拒绝，不悄悄忽略（DRD-22）。
// 真要收费，是改决定与代码，不是改一个价格。
package app

import (
	"context"
	"encoding/json"
	"time"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/director/repo"
)

// Actor 是发起搭景的人：网页（只有 UserID）、Agent（AgentSessionID 非 0）或外部 MCP 客户端（McpGrantID 非 0）。
// 后两者的 UserID 是「代谁搭的」那个人：限流按他算，场景的最后编辑者也记他。
type Actor struct {
	UserID         int64
	AgentSessionID int64
	McpGrantID     int64
}

// CallSource 是调用记录的来源（director_llm_call.source：web / agent / mcp）。
func (a Actor) CallSource() string {
	switch {
	case a.AgentSessionID != 0:
		return "agent"
	case a.McpGrantID != 0:
		return "mcp"
	}
	return "web"
}

// SceneState 是搭景要用的场景现状。
type SceneState struct {
	ID           int64
	ProjectID    int64
	CanvasNodeID string
	Version      int
	Body         json.RawMessage
}

// SceneStore 由画布域实现（wiring 适配 canvas/app）。
type SceneStore interface {
	// SceneForBuild 取一个要被搭景的场景：角色要有 edit、项目不在回收站（40905）、场景属于本项目（否则 404）。
	SceneForBuild(ctx context.Context, ws, projectID, sceneID int64, role collab.Role) (SceneState, error)
	// SaveScene 以 version 乐观锁写入（Agent / MCP 路径），保存成功时画布域发 `director.scene_updated`。
	// 冲突回 *httpx.BizError（40902），调用方据此重做一次。
	SaveScene(ctx context.Context, ws, projectID, sceneID int64, actor Actor, role collab.Role,
		version int, body json.RawMessage) (SceneState, error)
}

// TextRequest 是一次同步文本调用。
type TextRequest struct {
	ModelCode string
	System    string
	User      string
	// Images 是内联好的参考图（`data:` 地址），不发外链（DR-N-11）。
	Images []string
	// Operation 记进请求日志（provider_request_log 记 scene_build，DRD-24）。
	Operation string
}

// TextModel 由 Provider 域实现（provider/app.HTTPProvider.CompleteText）。
type TextModel interface {
	Complete(ctx context.Context, in TextRequest) (string, error)
}

// Choice 是这次搭景要用的模型。
type Choice struct {
	ModelCode string
	// MaxImages 是参考图上限：模型在 scene_build 上 image 角色的上限与 DR-PARAM-14 的 4 取小。
	MaxImages int
}

// Unavailable 说明 AI 搭景现在为什么不可用。
type Unavailable struct {
	Reason string
	// UserFixable 是用户自己能解决的（例如升级会员），其余要运营处理。前端按它分语气（同图片工具栏）。
	UserFixable bool
}

// Catalog 由目录域实现：scene_build 的第一个可用模型（按排序逐个报价，取第一个通过的）。
//
// ⚠️ build 与 build-config 共用这一个判断（DR-F-108）：各写一份的话，会出现「面板说能用、点下去 42203」。
type Catalog interface {
	SceneBuildChoice(ctx context.Context, memberLevelID int64, imageCount int) (Choice, *Unavailable, error)
}

// Images 由媒体域实现：按工作区取缩略图，内联成 `data:` 地址（同营销的推理看图）。
// 不属于本工作区、不是图片、太大，一律报错。
type Images interface {
	InlineImage(ctx context.Context, workspaceID, mediaID int64) (string, error)
}

// Prompts 由目录域实现：渲染 `director.scene_build.system`（DB 行优先，代码兜底在目录域）。
type Prompts interface {
	RenderPrompt(ctx context.Context, code, modelCode string, vars map[string]string) (string, error)
}

// Calls 是调用记录仓储（director/repo 实现）。
type Calls interface {
	InsertWithLimits(ctx context.Context, call repo.LLMCall, lim repo.Limits) (repo.LimitDecision, error)
	Finish(ctx context.Context, id int64, in repo.FinishInput) (bool, error)
	Usage(ctx context.Context, userID int64) (running, usedThisHour int, err error)
	RecoverStale(ctx context.Context, olderThan time.Duration, limit int) (int, error)
	DeleteOlderThan(ctx context.Context, before time.Time, limit int) (int, error)
}
