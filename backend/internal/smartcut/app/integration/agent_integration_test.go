//go:build integration

// Agent / MCP 的智能剪辑（docs/agent/smartcut SCT-41）：真库、真画布与时间线写入路径，工具走与 Worker / API 同一份
// wiring 适配器（AgentCanvas / AgentSmartcuts / MCPSmartcuts）；只把文本模型与目录换成测试替身。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	"github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// kindLog 收 timeline.updated 的「原因/编辑者来源」。
type kindLog struct {
	mu  sync.Mutex
	got []string
}

func (l *kindLog) Publish(_ context.Context, _ string, payload []byte) error {
	var ev timelineapp.TimelineUpdated
	if err := json.Unmarshal(payload, &ev); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, ev.Reason+"/"+ev.EditorKind)
	return nil
}

func (l *kindLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.got
	l.got = nil
	return out
}

type agentEnv struct {
	planEnv
	proj  int64
	log   *kindLog
	tools map[string]agenttools.Tool
	rc    agentapp.RunContext
}

const agentSession = int64(7001)

// setupAgent 在 setupPlan 的服务之上另建一个项目：画布上四个素材节点（三段视频 + 一条配乐），还没有智能剪辑节点。
func setupAgent(t *testing.T) agentEnv {
	t.Helper()
	e := setupPlan(t, config.Smartcut{})
	log := &kindLog{}
	e.tl.SetEvents(log)
	pid := e.env.project(t, ws)
	e.setCanvas(t, pid, []map[string]any{
		nodeWith("v1", "video", mediaResult(e.media[0])), nodeWith("v2", "video", mediaResult(e.media[1])),
		nodeWith("v3", "video", mediaResult(e.media[2])), nodeWith("a1", "audio", mediaResult(e.music)),
	}, []map[string]any{})
	deps := agenttools.MultiDeps{Canvas: wiring.AgentCanvas{Svc: e.canvas}, Timelines: wiring.AgentTimelines{Svc: e.tl},
		Smartcuts: wiring.AgentSmartcuts{Svc: e.svc, Timeline: e.tl}}
	tools := map[string]agenttools.Tool{}
	for _, tl := range agenttools.MultiTools(deps) {
		tools[string(tl.Name())] = tl
	}
	rc := agentapp.RunContext{WorkspaceID: ws, ProjectID: pid, SessionID: agentSession, TurnID: 1, StepID: 1, Attempt: 1,
		UserID: user, Role: collab.RoleEditor, State: agentdomain.StateExecuting}
	return agentEnv{planEnv: e, proj: pid, log: log, tools: tools, rc: rc}
}

func (e agentEnv) run(t *testing.T, name string, step int64, args string) map[string]any {
	t.Helper()
	rc := e.rc
	rc.StepID = step
	res, err := e.tools[name].Run(e.ctx, rc, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s %s：%v", name, args, err)
	}
	var m map[string]any
	if err := json.Unmarshal(res.Content, &m); err != nil {
		t.Fatalf("%s 结果不是 JSON：%s", name, res.Content)
	}
	return m
}

// canvasOf 读项目画布里的节点与连线。
func (e agentEnv) canvasOf(t *testing.T) (map[string]map[string]any, []map[string]any) {
	t.Helper()
	var raw string
	if err := e.pool.QueryRow(e.ctx, `select canvas_data::text from project where id = $1`, e.proj).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	_ = json.Unmarshal([]byte(raw), &doc)
	nodes := map[string]map[string]any{}
	for _, n := range doc.Nodes {
		nodes[n["id"].(string)] = n
	}
	return nodes, doc.Edges
}

func (e agentEnv) mediaOn(t *testing.T, tid int64, kind string) []int64 {
	t.Helper()
	v, err := e.tl.Get(e.ctx, ws, tid)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, c := range clipsOf(t, v, kind) {
		out = append(out, c.MediaID)
	}
	return out
}

func parseID(t *testing.T, v any) int64 {
	t.Helper()
	var id int64
	if _, err := fmt.Sscan(fmt.Sprint(v), &id); err != nil || id <= 0 {
		t.Fatalf("不是 ID：%v", v)
	}
	return id
}

// SC-AC-67：Agent 建智能剪辑节点并连上素材 → 连线即入轨铺好 → 发起 AI 剪辑：时间线有内容、最后编辑来源为 agent、
// 剪辑台开着的人收到 editorKind=agent 的 timeline.updated；调用记录的来源是 agent；写入前存了快照；只读详情看得到结果。
func TestAgentSmartcut_端到端写入与事件(t *testing.T) {
	e := setupAgent(t)
	created := e.run(t, "smartcut_create", 11, `{"title":"新品剪辑","inputNodeIds":["v1","v2","v3","a1"]}`)
	nodeID := created["nodeId"].(string)
	tid := parseID(t, created["timelineId"])
	if created["appended"] != float64(4) {
		t.Fatalf("四个素材按连线即入轨铺上：%v", created)
	}
	nodes, edges := e.canvasOf(t)
	if n := nodes[nodeID]; n == nil || n["type"] != "smartcut" ||
		n["params"].(map[string]any)["timelineId"] != fmt.Sprint(tid) {
		t.Fatalf("画布上有智能剪辑节点、params.timelineId 写回：%v", n)
	}
	var order []string
	for _, eg := range edges {
		if eg["target"] == nodeID {
			order = append(order, fmt.Sprintf("%s:%v", eg["source"], eg["order"]))
		}
	}
	if strings.Join(order, ",") != "v1:1,v2:2,v3:3,a1:4" {
		t.Fatalf("按给的顺序连进来：%v", order)
	}
	if got := e.mediaOn(t, tid, domain.TrackVideo); fmt.Sprint(got) != fmt.Sprint(e.media) {
		t.Fatalf("画面轨按编号铺好：%v", got)
	}
	if got := e.mediaOn(t, tid, domain.TrackAudio); len(got) != 1 || got[0] != e.music {
		t.Fatalf("配乐进音频轨：%v", got)
	}
	if got := e.log.take(); !slices.Contains(got, "sync/sync") {
		t.Fatalf("铺轨发 timeline.updated（sync）：%v", got)
	}

	planned := e.run(t, "smartcut_plan", 12, `{"nodeId":"`+nodeID+`","mode":"mix","instruction":"突出第二段","targetSec":10}`)
	if planned["appliedVariant"] != float64(0) || planned["snapshotId"] == nil {
		t.Fatalf("剪辑结果：%v", planned)
	}
	if got := e.mediaOn(t, tid, domain.TrackVideo); fmt.Sprint(got) != fmt.Sprint([]int64{e.media[1], e.media[0], e.media[2]}) {
		t.Fatalf("钩子在前、结尾在后：%v", got)
	}
	var kind string
	var editor int64
	if err := e.pool.QueryRow(e.ctx, `select last_editor_kind, last_editor_user_id from timeline where id = $1`, tid).
		Scan(&kind, &editor); err != nil || kind != "agent" || editor != user {
		t.Fatalf("最后编辑来源 agent、发起人记用户：%s %d %v", kind, editor, err)
	}
	if got := e.log.take(); !slices.Contains(got, "plan/agent") {
		t.Fatalf("剪辑台开着的人收到「时间线被 Agent 更新」：%v", got)
	}
	var source string
	callID := parseID(t, planned["callId"])
	if err := e.pool.QueryRow(e.ctx, `select source from smartcut_llm_call where id = $1`, callID).Scan(&source); err != nil ||
		source != "agent" {
		t.Fatalf("调用记录的来源：%s %v", source, err)
	}
	if n := e.count(t, `select count(*) from timeline_snapshot where timeline_id = $1 and reason = 'ai_plan'`, tid); n != 1 {
		t.Fatalf("写入前存一份快照：%d", n)
	}
	if n := e.count(t, `select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("AI 剪辑免费：%d 条积分流水", n)
	}

	got := e.run(t, "smartcut_get", 13, `{"nodeId":"`+nodeID+`"}`)
	clips, _ := got["clips"].([]any)
	inputs, _ := got["inputs"].([]any)
	if got["hasTimeline"] != true || got["clipCount"] != float64(3) || len(inputs) != 4 || len(clips) < 3 {
		t.Fatalf("只读详情：%v", got)
	}
}

// 剪辑期间用户在剪辑台里改过（写入撞上乐观锁）：按刚出的同一套方案重读重写一次，不再调模型；结果照样写进去。
func TestAgentSmartcut_写入冲突重放一次(t *testing.T) {
	e := setupAgent(t)
	created := e.run(t, "smartcut_create", 21, `{"inputNodeIds":["v1","v2","v3","a1"]}`)
	nodeID := created["nodeId"].(string)
	tid := parseID(t, created["timelineId"])
	e.text.during = func() {
		v, err := e.tl.Get(e.ctx, ws, tid)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := e.tl.UpdateMeta(e.ctx, ws, tid, v.Timeline.Version, timelinerepo.Editor{UserID: user, Kind: timelinerepo.EditorUser}, "用户改了名", "", 0, 0, 0); err != nil {
			t.Error(err)
		}
	}
	before := len(e.text.requests())
	planned := e.run(t, "smartcut_plan", 22, `{"nodeId":"`+nodeID+`","mode":"mix","instruction":"突出第二段"}`)
	if planned["appliedVariant"] != float64(0) {
		t.Fatalf("冲突之后按同一套方案写入：%v", planned)
	}
	if n := len(e.text.requests()) - before; n != 1 {
		t.Fatalf("重放不再调模型：调了 %d 次", n)
	}
	if got := e.mediaOn(t, tid, domain.TrackVideo); fmt.Sprint(got) != fmt.Sprint([]int64{e.media[1], e.media[0], e.media[2]}) {
		t.Fatalf("结果写进去了：%v", got)
	}
	var name string
	_ = e.pool.QueryRow(e.ctx, `select name from timeline where id = $1`, tid).Scan(&name)
	if name != "用户改了名" {
		t.Fatalf("用户期间的改动不丢：%s", name)
	}
}

// 不走脚本：按节点拼 = 新建智能剪辑节点 → 按顺序连进来 → 连线即入轨铺好（SCD-39）；给了画幅就定成那个画幅。
func TestAgentTimelineAssemble_按节点拼建智能剪辑节点(t *testing.T) {
	e := setupAgent(t)
	op := e.next()
	e.exec(t, `insert into operation (id, code, name, domain, out_node_type) values ($1,'text2video','文生视频','video','video')`, op)
	for i, node := range []string{"v1", "v2"} {
		gid := e.next()
		e.exec(t, `insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id, node_id,
			operation_id, model_id, result_type, idempotency_key, status)
			values ($1,$2,1,$3,$4,$5,$6,1,'video',$7,'success')`, gid, ws, user, e.proj, node, op, fmt.Sprintf("g-%d", gid))
		e.exec(t, `insert into generation_output (id, generation_id, idx, media_object_id, output_type)
			values ($1,$2,0,$3,'video')`, e.next(), gid, e.media[i])
	}
	deps := agenttools.MultiDeps{Canvas: wiring.AgentCanvas{Svc: e.canvas}, Timelines: wiring.AgentTimelines{Svc: e.tl},
		Smartcuts: wiring.AgentSmartcuts{Svc: e.svc, Timeline: e.tl}, Nodes: wiring.AgentNodeGenerations{Repo: genrepo.New(e.pool)}}
	var tool agenttools.Tool
	for _, tl := range agenttools.MultiTools(deps) {
		if tl.Name() == agentdomain.ToolTimelineAssemble {
			tool = tl
		}
	}
	rc := e.rc
	rc.StepID = 31
	res, err := tool.Run(e.ctx, rc, json.RawMessage(`{"nodeIds":["v2","v1"],"aspectRatio":"16:9"}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(res.Content, &m)
	nodeID, _ := m["nodeId"].(string)
	tid := parseID(t, m["timelineId"])
	if m["assembled"] != true || nodeID == "" || m["appended"] != float64(2) {
		t.Fatalf("结果：%v", m)
	}
	if got := e.mediaOn(t, tid, domain.TrackVideo); fmt.Sprint(got) != fmt.Sprint([]int64{e.media[1], e.media[0]}) {
		t.Fatalf("按给的顺序铺：%v", got)
	}
	var aspect string
	_ = e.pool.QueryRow(e.ctx, `select aspect_ratio from timeline where id = $1`, tid).Scan(&aspect)
	if aspect != "16:9" {
		t.Fatalf("画幅：%s", aspect)
	}
	if node, ok, err := e.svc.TimelineNode(e.ctx, ws, e.proj, tid); err != nil || !ok || node != nodeID {
		t.Fatalf("时间线绑在新节点上：%s %v %v", node, ok, err)
	}
}

// 把脚本时间线（没绑节点）挂到节点上：只绑没绑过的；重试算成功；已经绑在别的节点上、或节点已经有时间线都回 40902。
func TestBindTimeline_脚本时间线挂到节点上(t *testing.T) {
	e := setupAgent(t)
	v, err := e.tl.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: e.proj, Name: "脚本成片"})
	if err != nil {
		t.Fatal(err)
	}
	tid := v.Timeline.ID
	if _, ok, _ := e.svc.TimelineNode(e.ctx, ws, e.proj, tid); ok {
		t.Fatal("新建的脚本时间线还没绑节点")
	}
	if err := e.svc.BindTimeline(e.ctx, ws, e.proj, tid, "sx"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.BindTimeline(e.ctx, ws, e.proj, tid, "sx"); err != nil {
		t.Fatalf("重试算成功：%v", err)
	}
	if node, ok, _ := e.svc.TimelineNode(e.ctx, ws, e.proj, tid); !ok || node != "sx" {
		t.Fatalf("绑上了：%s %v", node, ok)
	}
	if err := e.svc.BindTimeline(e.ctx, ws, e.proj, tid, "sy"); code(t, err) != errcode.VersionConflict {
		t.Fatalf("绑在别的节点上：%v", err)
	}
	second, _ := e.tl.Create(e.ctx, timelineapp.CreateInput{WorkspaceID: ws, ProjectID: e.proj, Name: "另一条"})
	if err := e.svc.BindTimeline(e.ctx, ws, e.proj, second.Timeline.ID, "sx"); code(t, err) != errcode.VersionConflict {
		t.Fatalf("节点已经有时间线：%v", err)
	}
	if err := e.svc.BindTimeline(e.ctx, other, e.proj, tid, "sz"); err == nil {
		t.Fatal("跨工作区不能绑")
	}
}
