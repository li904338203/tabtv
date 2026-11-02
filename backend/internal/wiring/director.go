package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	directorapp "github.com/li904338203/tabtv/backend/internal/director/app"
	directorrepo "github.com/li904338203/tabtv/backend/internal/director/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	providerapp "github.com/li904338203/tabtv/backend/internal/provider/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 3D 导演台 AI 搭景的端口适配（docs/agent/director，DRT-19）。
//
// director/app 不 import 画布、目录、Provider、媒体任何一个域（谁定义端口谁拥有契约）：适配器写在组装层。
// API 与 Worker 都经 BuildCore 装出这一份（Agent / MCP 的搭景工具在 Worker 里执行，DRT-29）。

// DirectorInput 是 BuildDirector 的入参。
type DirectorInput struct {
	Pool    *pgxpool.Pool
	Canvas  *canvasapp.Service
	Catalog *catalogapp.Service
	Media   *mediaapp.Service
	Text    *providerapp.HTTPProvider
	Cfg     config.Director
	NextID  func() int64
	Logger  *slog.Logger
}

// BuildDirector 装出 AI 搭景服务。
func BuildDirector(in DirectorInput) *directorapp.Service {
	return directorapp.New(directorapp.Ports{
		Scenes:  directorScenes{in.Canvas},
		Text:    directorText{in.Text},
		Catalog: directorCatalog{in.Catalog},
		// 参考图内联与营销的推理看图同一个实现：按工作区取缩略图、只收四种图片、1 MiB 上限（DR-N-11）。
		Images:  AgentSceneImages{Catalog: in.Catalog, Media: in.Media, Width: in.Cfg.BuildImageWidth},
		Prompts: in.Catalog,
		Calls:   directorrepo.New(in.Pool),
	}, directorapp.Config{
		Concurrency: in.Cfg.BuildConcurrency, PerHour: in.Cfg.BuildPerHour,
		CallTimeout: in.Cfg.BuildTimeout, StaleAfter: in.Cfg.BuildStaleAfter,
	}, in.NextID, in.Logger)
}

// DirectorSceneStore 是搭景的场景端口（画布域实现）。BuildDirector 用它；集成测试换掉模型与目录、
// 但场景仍走这一条保存路径（校验、乐观锁、事件都在画布域）。
func DirectorSceneStore(svc *canvasapp.Service) directorapp.SceneStore { return directorScenes{svc} }

type directorScenes struct{ svc *canvasapp.Service }

func sceneState(sc repo.DirectorScene) directorapp.SceneState {
	return directorapp.SceneState{ID: sc.ID, ProjectID: sc.ProjectID, CanvasNodeID: sc.CanvasNodeID,
		Version: sc.Version, Body: sc.Scene}
}

func (a directorScenes) SceneForBuild(ctx context.Context, ws, projectID, sceneID int64, role collab.Role) (
	directorapp.SceneState, error) {
	sc, err := a.svc.DirectorSceneForBuild(ctx, ws, projectID, sceneID, role)
	if err != nil {
		return directorapp.SceneState{}, err
	}
	return sceneState(sc), nil
}

// SaveScene 走画布域的同一个保存入口：校验、媒体归属、指纹、乐观锁与 `director.scene_updated` 事件都在那里，
// Agent / MCP 的写入与网页保存是同一条路（DRD-31）。
func (a directorScenes) SaveScene(ctx context.Context, ws, projectID, sceneID int64, actor directorapp.Actor,
	role collab.Role, version int, body json.RawMessage) (directorapp.SceneState, error) {
	sc, err := a.svc.UpdateDirectorScene(ctx, ws, projectID, sceneID, canvasapp.DirectorActor{
		UserID: actor.UserID, AgentSessionID: actor.AgentSessionID, McpGrantID: actor.McpGrantID,
	}, role, version, body)
	if err != nil {
		return directorapp.SceneState{}, err
	}
	return sceneState(sc), nil
}

type directorText struct{ p *providerapp.HTTPProvider }

func (a directorText) Complete(ctx context.Context, in directorapp.TextRequest) (string, error) {
	return a.p.CompleteText(ctx, providerapp.TextInput{ModelCode: in.ModelCode, System: in.System, User: in.User,
		Images: in.Images, Operation: in.Operation})
}

type directorCatalog struct{ svc *catalogapp.Service }

func (a directorCatalog) SceneBuildChoice(ctx context.Context, memberLevelID int64, imageCount int) (
	directorapp.Choice, *directorapp.Unavailable, error) {
	c, u, err := a.svc.SceneBuildChoice(ctx, memberLevelID, imageCount)
	if err != nil || u != nil {
		if u != nil {
			return directorapp.Choice{}, &directorapp.Unavailable{Reason: u.Reason, UserFixable: u.UserFixable}, err
		}
		return directorapp.Choice{}, nil, err
	}
	return directorapp.Choice{ModelCode: c.ModelCode, MaxImages: c.MaxImages}, nil, nil
}

// ---------------------------------------------------------------- Agent / MCP 的导演台工具（DRT-29）
//
// 「建节点 → 建场景 → 写回」与「按节点取场景（自愈）」在 internal/canvasgen；这里只把画布域的场景服务与
// director/app 的搭景接成 Agent 与 MCP 两侧的端口。建场景、写场景一律走画布域的同一个入口（来源记 agent / mcp，
// 保存成功发 director.scene_updated）。

var (
	_ agentapp.Directors       = AgentDirectors{}
	_ canvasgen.DirectorScenes = canvasGenDirectorScenes{}
)

// createDirectorScene 为画布节点建场景（按（项目，节点）幂等；source 非 0 时分叉那一份），返回场景 ID。
func createDirectorScene(ctx context.Context, svc *canvasapp.Service, ws, projectID int64,
	actor canvasapp.DirectorActor, role collab.Role, nodeID string, source int64) (int64, error) {
	v, _, err := svc.CreateDirectorScene(ctx, ws, projectID, actor, role,
		canvasapp.CreateDirectorSceneInput{CanvasNodeID: nodeID, SourceSceneID: source})
	if err != nil {
		return 0, err
	}
	return v.Scene.ID, nil
}

// directorSceneNode 取本项目里一份场景绑定的画布节点；found=false 表示场景不在本项目（含不存在）。
func directorSceneNode(ctx context.Context, svc *canvasapp.Service, ws, projectID, sceneID int64,
	role collab.Role) (nodeID string, found bool, err error) {
	v, err := svc.GetDirectorScene(ctx, ws, projectID, sceneID, role)
	var be *httpx.BizError
	if errors.As(err, &be) && be.Code == errcode.NotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v.Scene.CanvasNodeID, true, nil
}

// errDirectorUnwired 是搭景服务没装上（装配错误，不是用户问题）。
func errDirectorUnwired() error { return httpx.Errf(errcode.Internal, "AI 搭景链路未接入") }

// AgentDirectors 实现 agentapp.Directors（Worker 进程：Agent 回合在那里跑）。
type AgentDirectors struct {
	Canvas   *canvasapp.Service
	Director *directorapp.Service
}

func (a AgentDirectors) CreateScene(ctx context.Context, in agentapp.DirectorSceneRequest) (int64, error) {
	return createDirectorScene(ctx, a.Canvas, in.WorkspaceID, in.ProjectID,
		canvasapp.DirectorActor{UserID: in.UserID, AgentSessionID: in.SessionID}, in.Role, in.CanvasNodeID,
		in.SourceSceneID)
}

func (a AgentDirectors) SceneNode(ctx context.Context, ws, projectID, sceneID int64, role collab.Role) (string, error) {
	nodeID, found, err := directorSceneNode(ctx, a.Canvas, ws, projectID, sceneID, role)
	if err == nil && !found {
		return "", agentapp.ErrDirectorSceneNotFound
	}
	return nodeID, err
}

func (a AgentDirectors) BuildAndApply(ctx context.Context, in agentapp.DirectorBuildRequest) (
	agentapp.DirectorBuildOutcome, error) {
	if a.Director == nil {
		return agentapp.DirectorBuildOutcome{}, errDirectorUnwired()
	}
	r, err := a.Director.BuildAndApply(ctx, directorapp.BuildInput{
		WorkspaceID: in.WorkspaceID, ProjectID: in.ProjectID, SceneID: in.SceneID, Role: in.Role,
		Actor:         directorapp.Actor{UserID: in.UserID, AgentSessionID: in.SessionID},
		MemberLevelID: in.MemberLevelID, Prompt: in.Prompt, ReferenceMediaIDs: in.ReferenceMediaIDs,
	})
	if err != nil {
		return agentapp.DirectorBuildOutcome{}, err
	}
	return agentapp.DirectorBuildOutcome{SceneID: r.SceneID, Version: r.Version, Added: r.Changes.Added,
		Updated: r.Changes.Updated, Removed: r.Changes.Removed, Defaults: r.Defaults, Notice: r.Notice}, nil
}

// canvasGenDirectorScenes 是 canvasgen.DirectorScenes 的画布域实现（MCP 用；来源按 Caller.Origin 记）。
type canvasGenDirectorScenes struct{ Svc *canvasapp.Service }

func (a canvasGenDirectorScenes) Create(ctx context.Context, c canvasgen.Caller, nodeID string, source int64) (
	int64, error) {
	actor := canvasapp.DirectorActor{UserID: c.UserID}
	switch c.Origin.Kind {
	case canvasgen.OriginAgent:
		actor.AgentSessionID = c.Origin.SessionID
	case canvasgen.OriginMCP:
		actor.McpGrantID = c.Origin.GrantID
	}
	return createDirectorScene(ctx, a.Svc, c.WorkspaceID, c.ProjectID, actor, c.Role, nodeID, source)
}

func (a canvasGenDirectorScenes) SceneNode(ctx context.Context, c canvasgen.Caller, sceneID int64) (string, error) {
	nodeID, found, err := directorSceneNode(ctx, a.Svc, c.WorkspaceID, c.ProjectID, sceneID, c.Role)
	if err == nil && !found {
		return "", canvasgen.ErrDirectorSceneNotFound
	}
	return nodeID, err
}
