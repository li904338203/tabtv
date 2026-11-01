//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	directorapp "github.com/li904338203/tabtv/backend/internal/director/app"
	directorrepo "github.com/li904338203/tabtv/backend/internal/director/repo"
	"github.com/li904338203/tabtv/backend/internal/mcp/tools"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// 3D 导演台的 MCP 工具（DRT-29，DR-AC-47）的真库测试：画布、场景、搭景调用记录走真库与真服务，
// 只有看图的文本模型、目录选模型与提示词是假的。

type dirText struct{}

func (dirText) Complete(context.Context, directorapp.TextRequest) (string, error) {
	return `{"objects":[{"op":"add","ref":"小美","kind":"character","posePreset":"sit_chair"},` +
		`{"op":"add","ref":"阿杰","kind":"character","relation":{"type":"near","target":"小美","distance":1.5}}],` +
		`"camera":{"angle":"eye","side":"front"}}`, nil
}

type dirCatalog struct{}

func (dirCatalog) SceneBuildChoice(context.Context, int64, int) (directorapp.Choice, *directorapp.Unavailable, error) {
	return directorapp.Choice{ModelCode: "m-scene", MaxImages: 4}, nil, nil
}

type dirPrompts struct{}

func (dirPrompts) RenderPrompt(context.Context, string, string, map[string]string) (string, error) {
	return "按描述摆放人物与机位", nil
}

type dirEvents struct {
	mu     sync.Mutex
	scenes []canvasapp.DirectorSceneUpdated
}

func (e *dirEvents) Publish(_ context.Context, _ string, payload []byte) error {
	var ev canvasapp.DirectorSceneUpdated
	if json.Unmarshal(payload, &ev) == nil && ev.Type == canvasapp.DirectorSceneUpdatedType {
		e.mu.Lock()
		e.scenes = append(e.scenes, ev)
		e.mu.Unlock()
	}
	return nil
}

func TestMcpDirector_搭景写入来源为mcp(t *testing.T) {
	e, _ := newWriteEnv(t, false)
	events := &dirEvents{}
	e.canvas.SetEvents(events)
	director := directorapp.New(directorapp.Ports{Scenes: wiring.DirectorSceneStore(e.canvas), Text: dirText{},
		Catalog: dirCatalog{}, Prompts: dirPrompts{}, Calls: directorrepo.New(e.pool)}, directorapp.Config{}, e.next, nil)
	// 只接导演台需要的端口（画布 + 搭景），与 cmd/api 同一个装配函数。
	e.reg = tools.NewRegistry(wiring.NewMCPToolDeps(wiring.MCPReadInput{Canvas: e.canvas, Director: director}))
	pid := e.project(e.ws, "导演台", `{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},"nodes":[],"edges":[]}`,
		time.Now())

	// ① director_create：同一个 idempotencyKey 重试只建一个节点、一份场景。
	args := `{"projectId":"` + itoa(pid) + `","idempotencyKey":"dir-1"}`
	m, _, err := e.call(e.p, "director_create", args)
	if err != nil {
		t.Fatal(err)
	}
	nodeID, sceneID := m["nodeId"].(string), m["sceneId"].(string)
	again, _, err := e.call(e.p, "director_create", args)
	if err != nil || again["nodeId"] != nodeID || again["sceneId"] != sceneID {
		t.Fatalf("重试应拿回同一个节点与场景：%v %v（%v）", m, again, err)
	}
	var n int
	_ = e.pool.QueryRow(context.Background(), `select count(*)::int from director_scene where project_id=$1`, pid).Scan(&n)
	if n != 1 {
		t.Fatalf("应只有一份场景，实际 %d", n)
	}
	var canvasData string
	_ = e.pool.QueryRow(context.Background(), `select canvas_data::text from project where id=$1`, pid).Scan(&canvasData)
	if !strings.Contains(canvasData, `"type": "director"`) || !strings.Contains(canvasData, `"sceneId": "`+sceneID+`"`) {
		t.Fatalf("画布应有导演台节点且 params.sceneId 已写回：%s", canvasData)
	}

	// ② director_build：写入场景，来源 mcp（记授权），版本 +1，结果带截图提醒，发出 editorKind=mcp 的事件。
	m, out, err := e.call(e.p, "director_build",
		`{"projectId":"`+itoa(pid)+`","nodeId":"`+nodeID+`","prompt":"小美坐着，阿杰站在她身边，平视"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m["sceneId"] != sceneID || m["version"] != float64(2) || m["notice"] != directorapp.Notice ||
		!strings.Contains(out.Summary, "截图") {
		t.Fatalf("director_build 结果：%v %s", m, out.Summary)
	}
	var source string
	var version int
	var grant *int64
	if err := e.pool.QueryRow(context.Background(), `select source, version, mcp_grant_id from director_scene
		where id = $1::bigint`, sceneID).Scan(&source, &version, &grant); err != nil {
		t.Fatal(err)
	}
	if source != "mcp" || version != 2 || grant == nil || *grant != e.p.GrantID {
		t.Fatalf("场景最后编辑来源应为 mcp 且记授权：source=%s v%d grant=%v", source, version, grant)
	}
	var calls int
	_ = e.pool.QueryRow(context.Background(), `select count(*)::int from director_llm_call where scene_id = $1::bigint
		and source = 'mcp' and mcp_grant_id = $2 and status = 'succeeded'`, sceneID, e.p.GrantID).Scan(&calls)
	if calls != 1 {
		t.Fatalf("搭景调用记录应记来源 mcp：%d", calls)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.scenes) != 1 || events.scenes[0].EditorKind != "mcp" || events.scenes[0].Version != 2 ||
		events.scenes[0].CanvasNodeID != nodeID {
		t.Fatalf("应发出一条 editorKind=mcp 的 director.scene_updated：%+v", events.scenes)
	}
}
