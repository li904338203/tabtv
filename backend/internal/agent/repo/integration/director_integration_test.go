//go:build integration

// 3D 导演台的 Agent 工具（DRT-29，DR-AC-46）的真库测试。
//
// 画布、场景、搭景调用记录与限流全部走真库与真服务（canvas/app、director/app、director/repo、wiring 适配器）；
// 只有看图的文本模型、目录选模型与提示词是假实现 —— 要证明的是「节点 → 场景 → 写回」「写入来源为 agent、
// 版本递增、发出 director.scene_updated」，这些都由画布域与 SQL 决定。
package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	directorapp "github.com/li904338203/tabtv/backend/internal/director/app"
	directorrepo "github.com/li904338203/tabtv/backend/internal/director/repo"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// directorDraft 是模型给的搭景方案：两个人对峙、低机位（与 director/app 单测同一种形状）。
const directorDraft = `{"objects":[
	{"op":"add","ref":"阿强","kind":"character","posePreset":"fight_stance","facing":{"target":"老刀"}},
	{"op":"add","ref":"老刀","kind":"character","posePreset":"fight_stance","relation":{"type":"facing","target":"阿强","distance":2.0}}],
	"camera":{"angle":"low","side":"front"},"notes":["街道环境不在导演台里搭，只摆人物"]}`

type dirText struct{}

func (dirText) Complete(context.Context, directorapp.TextRequest) (string, error) {
	return directorDraft, nil
}

type dirCatalog struct{}

func (dirCatalog) SceneBuildChoice(context.Context, int64, int) (directorapp.Choice, *directorapp.Unavailable, error) {
	return directorapp.Choice{ModelCode: "m-scene", MaxImages: 4}, nil, nil
}

type dirPrompts struct{}

func (dirPrompts) RenderPrompt(context.Context, string, string, map[string]string) (string, error) {
	return "按描述摆放人物与机位", nil
}

// sceneEvents 记下画布域发出的事件（只关心 director.scene_updated）。
type sceneEvents struct {
	mu     sync.Mutex
	scenes []canvasapp.DirectorSceneUpdated
}

func (e *sceneEvents) Publish(_ context.Context, _ string, payload []byte) error {
	var ev canvasapp.DirectorSceneUpdated
	if json.Unmarshal(payload, &ev) == nil && ev.Type == canvasapp.DirectorSceneUpdatedType {
		e.mu.Lock()
		e.scenes = append(e.scenes, ev)
		e.mu.Unlock()
	}
	return nil
}

func TestAgentDirector_建节点并搭景写入场景(t *testing.T) {
	ctx := context.Background()
	r, pool, next := setup(t)
	crepo := canvasrepo.New(pool)
	if err := crepo.Insert(ctx, canvasrepo.Project{ID: project, WorkspaceID: ws, CreatedBy: user, Name: "导演台项目",
		CanvasData: json.RawMessage(`{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},"nodes":[],"edges":[]}`)}); err != nil {
		t.Fatal(err)
	}
	canvas := canvasapp.New(crepo, next)
	events := &sceneEvents{}
	canvas.SetEvents(events)
	director := directorapp.New(directorapp.Ports{Scenes: wiring.DirectorSceneStore(canvas), Text: dirText{},
		Catalog: dirCatalog{}, Prompts: dirPrompts{}, Calls: directorrepo.New(pool)}, directorapp.Config{}, next, nil)
	tools := map[string]agentapp.Tool{}
	for _, tl := range wiring.NewAgentMultiTools(wiring.AgentMultiToolsInput{
		Paid: wiring.AgentPaidToolsInput{AgentRepo: r, Canvas: canvas, NextID: next}, Director: director}) {
		tools[string(tl.Name())] = tl
	}
	s := mustSession(t, r, pool, next)
	rc := agentapp.RunContext{WorkspaceID: ws, ProjectID: project, SessionID: s.ID, StepID: next(), Attempt: 1,
		UserID: user, Role: collab.RoleEditor, PayerAccountID: payer, State: domain.StateExecuting}

	// ① director_create：画布上出现导演台节点，params.sceneId 指向按这个节点建的场景（来源 agent）。
	res, err := tools["director_create"].Run(ctx, rc, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ NodeID, SceneID, Next string }
	if err := json.Unmarshal(res.Content, &created); err != nil || created.NodeID == "" || created.SceneID == "" ||
		!strings.Contains(created.Next, "截图") {
		t.Fatalf("director_create 结果：%s（%v）", res.Content, err)
	}
	p, err := canvas.Get(ctx, ws, project, collab.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []struct {
			ID     string         `json:"id"`
			Type   string         `json:"type"`
			Params map[string]any `json:"params"`
			Agent  map[string]any `json:"agent"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(p.CanvasData, &doc); err != nil || len(doc.Nodes) != 1 || doc.Nodes[0].ID != created.NodeID ||
		doc.Nodes[0].Type != "director" || doc.Nodes[0].Params["sceneId"] != created.SceneID {
		t.Fatalf("画布应有一个导演台节点且 params.sceneId 已写回：%s", p.CanvasData)
	}
	var nodeID, source string
	var version int
	var sessionID *int64
	if err := pool.QueryRow(ctx, `select canvas_node_id, source, version, agent_session_id from director_scene
		where id = $1::bigint and project_id = $2`, created.SceneID, project).Scan(&nodeID, &source, &version, &sessionID); err != nil {
		t.Fatal(err)
	}
	if nodeID != created.NodeID || source != "agent" || version != 1 || sessionID == nil || *sessionID != s.ID {
		t.Fatalf("场景按节点建、来源 agent：node=%s source=%s v%d session=%v", nodeID, source, version, sessionID)
	}

	// ② director_build：场景有了内容，版本 +1，最后编辑来源 agent；结果带截图提醒；发出 director.scene_updated。
	rc.StepID = next()
	res, err = tools["director_build"].Run(ctx, rc,
		json.RawMessage(`{"nodeId":"`+created.NodeID+`","prompt":"阿强和老刀在街头对峙，低机位"}`))
	if err != nil {
		t.Fatal(err)
	}
	var built struct {
		SceneID string   `json:"sceneId"`
		Version int      `json:"version"`
		Added   []string `json:"added"`
		Notice  string   `json:"notice"`
	}
	if err := json.Unmarshal(res.Content, &built); err != nil || built.SceneID != created.SceneID || built.Version != 2 ||
		len(built.Added) == 0 || built.Notice != directorapp.Notice {
		t.Fatalf("director_build 结果：%s（%v）", res.Content, err)
	}
	var body string
	if err := pool.QueryRow(ctx, `select source, version, agent_session_id, scene::text from director_scene where id = $1::bigint`,
		created.SceneID).Scan(&source, &version, &sessionID, &body); err != nil {
		t.Fatal(err)
	}
	if source != "agent" || version != 2 || sessionID == nil || *sessionID != s.ID ||
		!strings.Contains(body, "阿强") || !strings.Contains(body, "老刀") {
		t.Fatalf("场景应写入搭景结果、来源 agent：source=%s v%d session=%v", source, version, sessionID)
	}
	var calls int
	if err := pool.QueryRow(ctx, `select count(*)::int from director_llm_call where scene_id = $1::bigint
		and source = 'agent' and agent_session_id = $2 and status = 'succeeded'`, created.SceneID, s.ID).Scan(&calls); err != nil ||
		calls != 1 {
		t.Fatalf("搭景调用记录应记来源 agent 并成功：%d（%v）", calls, err)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.scenes) != 1 || events.scenes[0].EditorKind != "agent" || events.scenes[0].Version != 2 ||
		events.scenes[0].SceneID != created.SceneID || events.scenes[0].CanvasNodeID != created.NodeID {
		t.Fatalf("编辑器应收到一条 editorKind=agent 的 director.scene_updated：%+v", events.scenes)
	}
}
