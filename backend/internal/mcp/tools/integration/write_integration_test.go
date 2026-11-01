//go:build integration

// 对外 MCP 写工具（MCPT-11）的真库测试：任务取消、参考文件上传、写画布、写文档、建项目。
//
// ⚠️ 走真实的装配（wiring.BuildCore 的画布 / 生成 / 整图服务、wiring.NewTimelineService、wiring.NewMCPToolDeps）：
// 「退款只一次」靠生成域的 CAS 与账本唯一索引、「不能改用户节点」靠画布域的 OwnedByActor、「同键只建一个」靠
// uk_project_create_idem —— 假端口的测试把这些整条删掉照样绿。
// 只受理不跑 Worker；对象存储是本进程里的假 S3（只收 PUT），**不触发任何上游调用**。
package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/tools"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	providertransport "github.com/li904338203/tabtv/backend/internal/provider/transport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	contentrepo "github.com/li904338203/tabtv/backend/internal/content/repo"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
)

// fakeS3 只收 PutObject（200 + ETag），记下写过的 key。上传链路之后不再读对象（引用解析只查库）。
type fakeS3 struct {
	mu   sync.Mutex
	puts []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	if r.Method != http.MethodPut {
		http.Error(w, "only PUT", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	f.puts = append(f.puts, r.URL.Path)
	f.mu.Unlock()
	w.Header().Set("ETag", `"x"`)
	w.WriteHeader(http.StatusOK)
}

// newWriteEnv 在只读环境上换成真实的画布 / 生成 / 整图 / 时间线服务与可写的媒体服务，主体带全部 scope。
// allowPrivate 是 media.upload 的出网策略（测试里的文件服务器在 127.0.0.1）。
func newWriteEnv(t *testing.T, allowPrivate bool) (*env, *fakeS3) {
	t.Helper()
	e := newEnv(t)
	s3 := &fakeS3{}
	srv := httptest.NewServer(s3)
	t.Cleanup(srv.Close)
	store, err := objstore.New(context.Background(), config.Storage{Bucket: "seeaitv", Region: "us-east-1",
		Endpoint: srv.URL, AccessKey: "ak", SecretKey: "sk", SignTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	e.media = mediaapp.New(mediarepo.New(e.pool), store, e.next)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := wiring.BuildCore(wiring.CoreInput{Cfg: config.Config{},
		Deps: &bootstrap.Deps{DB: &db.Pool{Pool: e.pool}}, NextID: e.next, Logger: logger,
		Audit: audit.New(e.pool, e.next, logger), Media: e.media, Catalog: e.catalog})
	if err != nil {
		t.Fatal(err)
	}
	tl := wiring.NewTimelineService(wiring.TimelineInput{Pool: e.pool, NextID: e.next, Media: e.media,
		Canvas: core.Canvas, Catalog: e.catalog, Logger: logger})
	deps := wiring.NewMCPToolDeps(wiring.MCPReadInput{
		Identity: identityapp.New(identityrepo.New(e.pool), nil, nil, nil, e.next),
		Collab:   collabapp.New(collabrepo.New(e.pool), e.next),
		Billing:  billingapp.New(billingrepo.New(e.pool, e.next)), Canvas: core.Canvas, GenRepo: core.GenRepo,
		Gen: core.Gen, Catalog: e.catalog, Content: contentapp.New(contentrepo.New(e.pool, e.next)),
		Workflow: core.Workflow, Timeline: tl, Media: e.media, MediaURLTTL: time.Hour,
		Store: mcprepo.New(e.pool), NextID: e.next, QuoteTTL: 10 * time.Minute,
		Ledger: core.Billing, StateKey: mcpapp.DeriveStateKey("integration"),
		Download: providertransport.NewClient(providertransport.URLPolicy{AllowPrivate: allowPrivate}),
		WaitMax:  50 * time.Second,
	})
	e.reg = tools.NewRegistry(deps)
	e.canvas = core.Canvas
	k, err := e.keys.CreateKey(context.Background(), e.owner, mcpapp.CreateKeyInput{WorkspaceID: e.ws,
		ClientName: "Claude Code",
		Scopes:     []string{domain.ScopeView, domain.ScopeEdit, domain.ScopeGenerate}})
	if err != nil {
		t.Fatal(err)
	}
	if e.p, err = e.resolver.Resolve(context.Background(), k.Plaintext); err != nil {
		t.Fatal(err)
	}
	return e, s3
}

// mcpUserSave 是用户本人在网页上保存文档（canvas 域 UpdateDocument，actor=user）。
func (e *env) mcpUserSave(pid int64, version int) (any, error) {
	var docID int64
	_ = e.pool.QueryRow(context.Background(), `select id from project_document where project_id=$1`, pid).Scan(&docID)
	return e.canvas.UpdateDocument(context.Background(), e.ws, pid, docID, canvasapp.DocumentActor{UserID: e.owner},
		collab.RoleOwner, version, "网页上的旧稿")
}

func (e *env) refunds(refType, refID string) int {
	return e.count(`select count(*) from credit_transaction where ref_type=$1 and ref_id=$2 and type='refund'`,
		refType, refID)
}

// accept 走 generate 的基线确认受理一个任务，返回 generationId 与 nodeId。
func (e *env) accept(pid int64) (string, string) {
	e.t.Helper()
	nc := e.begin(genArgsFor(pid, ""))
	m, err := e.genCall(genArgsFor(pid, confirmArgs(nc)))
	if err != nil {
		e.t.Fatal(err)
	}
	return m["generationId"].(string), m["nodeId"].(string)
}

func TestTasksCancel_只退一次(t *testing.T) {
	e, _ := newWriteEnv(t, false)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	gid, _ := e.accept(pid)

	// AC-10 的前半：受理后任务 working，wait 到点（1 秒）返回当前状态，文本里没有「失败」。
	m, out, err := e.call(e.p, "tasks", `{"method":"wait","kind":"generation","id":"`+gid+`","timeoutSec":1}`)
	if err != nil || m["stillRunning"] != true || m["task"].(map[string]any)["status"] != "working" ||
		strings.Contains(out.Summary, "失败") || strings.Contains(m["message"].(string), "失败") {
		t.Fatalf("wait 到点应返回 working 且不含「失败」：%v %v", m, err)
	}

	m1 := e.mustCall("tasks", `{"method":"cancel","kind":"generation","id":"`+gid+`","reason":"不要了"}`)
	m2 := e.mustCall("tasks", `{"method":"cancel","kind":"generation","id":"`+gid+`"}`)
	if m1["cancelled"] != true || m2["cancelled"] != false {
		t.Fatalf("第一次 cancelled=true、第二次 false：%v / %v", m1, m2)
	}
	if st := m2["task"].(map[string]any)["status"]; st != "cancelled" {
		t.Fatalf("第二次应如实给出当前状态：%v", st)
	}
	if n := e.refunds("generation", gid); n != 1 {
		t.Fatalf("退款应恰好 1 笔，实际 %d", n)
	}
	// 终态之后 wait 立即返回。
	m = e.mustCall("tasks", `{"method":"wait","kind":"generation","id":"`+gid+`"}`)
	if m["stillRunning"] != false || m["task"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("终态应立即返回：%v", m)
	}
	// 导出没有取消入口：明确拒绝。
	if _, _, err := e.call(e.p, "tasks", `{"method":"cancel","kind":"export","id":"1"}`); bizCode(err) !=
		errcode.InvalidArgument || !strings.Contains(err.Error(), "不支持取消") {
		t.Fatalf("导出取消应明确拒绝：%v", err)
	}
	// 跨工作区的任务：与不存在一样 NOT_FOUND，不会去取消别人的任务。
	// 主体直接换成别的工作区（绕过 Resolver：只验适配层的租户条件取自 Principal）。
	other := e.p
	other.WorkspaceID = e.otherWS
	if _, _, err := e.call(other, "tasks", `{"method":"cancel","kind":"generation","id":"`+gid+`"}`); bizCode(err) !=
		errcode.NotFound {
		t.Fatalf("别的工作区应取不到：%v", err)
	}
}

func TestTasksCancel_整图与渲染走各自入口(t *testing.T) {
	e, _ := newWriteEnv(t, false)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	gid, nodeID := e.accept(pid)

	// 整图运行：一个运行中的节点挂着那个生成任务。取消运行 → 运行与子任务都 cancelled，子任务退款一次。
	run := e.next()
	exec(t, e.pool, `insert into workflow_run (id, workspace_id, project_id, payer_account_id, created_by_user_id,
		graph_snapshot, status, started_at) values ($1,$2,$3,$4,$5,'{}'::jsonb,'running',now())`,
		run, e.ws, pid, e.payer, e.owner)
	exec(t, e.pool, `insert into workflow_node_run (id, run_id, node_id, status, generation_id)
		values ($1,$2,$3,'running',$4)`, e.next(), run, nodeID, gid)
	m := e.mustCall("tasks", `{"method":"cancel","kind":"workflow","id":"`+itoa(run)+`"}`)
	if m["cancelled"] != true || m["task"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("整图取消：%v", m)
	}
	var gst string
	_ = e.pool.QueryRow(context.Background(), `select status from generation where id=$1`, gid).Scan(&gst)
	if gst != "failed" && gst != "cancelled" {
		t.Fatalf("子任务应随运行取消，实际 %s", gst)
	}
	if n := e.refunds("generation", gid); n != 1 {
		t.Fatalf("子任务退款应恰好 1 笔：%d", n)
	}
	if m := e.mustCall("tasks", `{"method":"cancel","kind":"workflow","id":"`+itoa(run)+`"}`); m["cancelled"] != false {
		t.Fatalf("已取消的运行再取消应 cancelled=false：%v", m)
	}

	// 成片渲染：timeline/app.CancelRender（CAS），第二次 cancelled=false 而不是 409。
	rj := e.next()
	exec(t, e.pool, `insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot,
		payer_account_id, created_by_user_id, status) values ($1,$2,$3,1,'{}'::jsonb,$4,$5,'running')`,
		rj, e.ws, e.next(), e.payer, e.owner)
	m = e.mustCall("tasks", `{"method":"cancel","kind":"render","id":"`+itoa(rj)+`"}`)
	if m["cancelled"] != true || m["task"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("渲染取消：%v", m)
	}
	if m := e.mustCall("tasks", `{"method":"cancel","kind":"render","id":"`+itoa(rj)+`"}`); m["cancelled"] != false {
		t.Fatalf("已取消的渲染再取消应 cancelled=false：%v", m)
	}
}

func canvasNode(t *testing.T, e *env, pid int64, id string) map[string]any {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `select n from project p, jsonb_array_elements(p.canvas_data->'nodes') n
		where p.id=$1 and n->>'id'=$2`, pid, id).Scan(&raw); err != nil {
		t.Fatalf("节点 %s：%v", id, err)
	}
	var n map[string]any
	_ = json.Unmarshal(raw, &n)
	return n
}

func TestCanvasApply_MCP不能改用户节点(t *testing.T) {
	e, _ := newWriteEnv(t, false)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	args := `{"projectId":"` + itoa(pid) + `","idempotencyKey":"k1","ops":[` +
		`{"kind":"add_node","ref":"ref:a","node":{"type":"image","title":"猫","params":{"prompt":"一只猫"},` +
		`"agent":{"sessionId":"1"},"origin":{"kind":"user"}},"position":{"x":400,"y":0}},` +
		`{"kind":"add_edge","edge":{"source":"n_user1","target":"ref:a"}}]}`
	m := e.mustCall("canvas_apply", args)
	ids := m["nodeIds"].([]any)
	if len(ids) != 1 || m["version"].(float64) != m["fromVersion"].(float64)+1 || m["replayed"] != false {
		t.Fatalf("应新建 1 个节点、版本 +1：%v", m)
	}
	own := ids[0].(string)
	n := canvasNode(t, e, pid, own)
	origin, _ := n["origin"].(map[string]any)
	if origin["kind"] != "mcp" || origin["grantId"] != itoa(e.p.GrantID) || origin["clientName"] != "Claude Code" ||
		n["agent"] != nil {
		t.Fatalf("来源应由画布域强写为 mcp（不能伪造）：%v", n)
	}
	// 操作日志：actor=mcp、带授权、无会话（浏览器靠它补取重放）。
	if c := e.count(`select count(*) from canvas_op_log where project_id=$1 and actor_kind='mcp' and mcp_grant_id=$2
		and agent_session_id is null and user_id=$3`, pid, e.p.GrantID, e.owner); c != 1 {
		t.Fatalf("应有 1 条 mcp 操作日志：%d", c)
	}
	// 同一幂等键重试：重放，不写第二份。
	m2 := e.mustCall("canvas_apply", args)
	if m2["replayed"] != true || m2["nodeIds"].([]any)[0] != own {
		t.Fatalf("同键应重放：%v", m2)
	}
	var nodes int
	_ = e.pool.QueryRow(context.Background(), `select jsonb_array_length(canvas_data->'nodes') from project where id=$1`,
		pid).Scan(&nodes)
	if nodes != 2 {
		t.Fatalf("重放不应多建节点：%d", nodes)
	}

	// 改用户的节点、往用户节点上接线、移动用户节点：一律 MCP_NODE_NOT_OWNED，画布不变。
	for _, op := range []string{
		`{"kind":"update_node","nodeId":"n_user1","patch":{"title":"改"}}`,
		`{"kind":"move_node","nodeId":"n_user1","position":{"x":9,"y":9}}`,
		`{"kind":"add_edge","edge":{"source":"` + own + `","target":"n_user1"}}`,
	} {
		_, _, err := e.call(e.p, "canvas_apply", `{"projectId":"`+itoa(pid)+`","ops":[`+op+`]}`)
		if bizCode(err) != errcode.McpNodeNotOwned || !strings.Contains(err.Error(), "请在旁边新建") {
			t.Fatalf("%s 应被拒：%v", op, err)
		}
	}
	// 改本授权建的节点：成功。
	m3 := e.mustCall("canvas_apply", `{"projectId":"`+itoa(pid)+`","ops":[`+
		`{"kind":"update_node","nodeId":"`+own+`","patch":{"title":"狗","params":{"prompt":"一只狗"}}}]}`)
	if n := canvasNode(t, e, pid, own); n["title"] != "狗" || m3["version"].(float64) <= m2["version"].(float64) {
		t.Fatalf("改自己的节点应成功：%v %v", n, m3)
	}
	// link_generation 不在白名单。
	if _, _, err := e.call(e.p, "canvas_apply", `{"projectId":"`+itoa(pid)+`","ops":[`+
		`{"kind":"link_generation","nodeId":"`+own+`","patch":{"generationId":"1"}}]}`); bizCode(err) !=
		errcode.InvalidArgument {
		t.Fatalf("link_generation 应被拒：%v", err)
	}
	// 只读成员（scope 只有 view）：闸门在 transport 的 Access.Check，这里验工具声明的动作。
	tool, _ := e.reg.Lookup("canvas_apply")
	if m, _ := tool.Method(""); m.Action != "edit" || !m.Write {
		t.Fatalf("canvas_apply 应要 edit 并声明 Write：%+v", m)
	}
}

func TestCanvasApply_用户改过后MCP被拒(t *testing.T) {
	e, _ := newWriteEnv(t, false)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	m := e.mustCall("canvas_apply", `{"projectId":"`+itoa(pid)+`","ops":[{"kind":"add_node","node":{"type":"text"}}]}`)
	own := m["nodeIds"].([]any)[0].(string)
	// 用户在浏览器里改过（前端 markTouched 写 origin.userTouched）。
	exec(t, e.pool, `update project set canvas_data = jsonb_set(canvas_data, '{nodes}',
		(select jsonb_agg(case when n->>'id'=$2 then jsonb_set(n, '{origin,userTouched}', 'true') else n end)
		 from jsonb_array_elements(canvas_data->'nodes') n)) where id=$1`, pid, own)
	_, _, err := e.call(e.p, "canvas_apply", `{"projectId":"`+itoa(pid)+`","ops":[`+
		`{"kind":"update_node","nodeId":"`+own+`","patch":{"title":"再改"}}]}`)
	if bizCode(err) != errcode.McpNodeNotOwned {
		t.Fatalf("用户改过的节点应拒绝：%v", err)
	}
	// 另一个授权（同一用户的另一个客户端）也不能改这个授权建的节点。
	k, _ := e.keys.CreateKey(context.Background(), e.owner, mcpapp.CreateKeyInput{WorkspaceID: e.ws,
		ClientName: "Cursor", Scopes: []string{domain.ScopeView, domain.ScopeEdit}})
	p2, _ := e.resolver.Resolve(context.Background(), k.Plaintext)
	m = e.mustCall("canvas_apply", `{"projectId":"`+itoa(pid)+`","ops":[{"kind":"add_node","node":{"type":"text"}}]}`)
	mine := m["nodeIds"].([]any)[0].(string)
	if _, _, err := e.call(p2, "canvas_apply", `{"projectId":"`+itoa(pid)+`","ops":[`+
		`{"kind":"move_node","nodeId":"`+mine+`","position":{"x":1,"y":1}}]}`); bizCode(err) != errcode.McpNodeNotOwned {
		t.Fatalf("别的授权不能改：%v", err)
	}
}

func TestMediaUpload_内网URL被出网策略拒绝(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	pngBytes := buf.Bytes()
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngBytes)
	}))
	t.Cleanup(files.Close)

	// 默认（生产）策略：127.0.0.1 一律拒绝，不落任何文件。
	e, s3 := newWriteEnv(t, false)
	_, _, err := e.call(e.p, "media", `{"method":"upload","url":"`+files.URL+`/a.png"}`)
	if bizCode(err) != errcode.InvalidArgument || !strings.Contains(err.Error(), "内网") {
		t.Fatalf("内网地址应被出网策略拒绝：%v", err)
	}
	if n := e.count(`select count(*) from media_object`); n != 0 || len(s3.puts) != 0 {
		t.Fatalf("被拒时不应落文件：%d %v", n, s3.puts)
	}

	// 开发策略（允许内网）：URL 下载 → media 域按文件头判出 PNG（不信响应头的 octet-stream）。
	e2, s32 := newWriteEnv(t, true)
	m := e2.mustCall("media", `{"method":"upload","url":"`+files.URL+`/cat.png"}`)
	ref := m["reference"].(map[string]any)
	if ref["kind"] != "image" || ref["contentType"] != "image/png" || ref["width"].(float64) != 2 || len(s32.puts) != 1 {
		t.Fatalf("URL 上传：%v %v", m, s32.puts)
	}
	var purpose string
	_ = e2.pool.QueryRow(context.Background(), `select purpose from media_object where id=$1 and workspace_id=$2`,
		ref["referenceId"], e2.ws).Scan(&purpose)
	if purpose != "upload_reference" {
		t.Fatalf("应记为参考文件：%q", purpose)
	}
	// base64：只收图片，类型照样按文件头判（声明成 jpeg 被拒）。
	b64 := base64.StdEncoding.EncodeToString(pngBytes)
	m = e2.mustCall("media", `{"method":"upload","base64":"`+b64+`","fileName":"b.png"}`)
	refB := m["reference"].(map[string]any)["referenceId"].(string)
	if _, _, err := e2.call(e2.p, "media", `{"method":"upload","base64":"`+b64+`","fileName":"b.png","mimeType":"image/jpeg"}`); bizCode(err) !=
		errcode.FileContentMismatch {
		t.Fatalf("声明与实际不符应拒绝：%v", err)
	}
	// 上传得到的 referenceId 能作为 generate 的输入受理。
	e2.seedPriced("edit-m", "img2img", "image", "per_image", 5, "text", "image")
	e2.credit(100)
	pid := e2.project(e2.ws, "片子", genCanvas, time.Now())
	args := `{"projectId":"` + itoa(pid) + `","nodeType":"image","operationCode":"img2img","modelCode":"edit-m",` +
		`"prompt":"换成夜景","count":1,"inputs":[{"kind":"reference","order":1,"referenceId":"` + refB + `"}]`
	nc := e2.begin(args + `}`)
	g, err := e2.genCall(args + confirmArgs(nc) + `}`)
	if err != nil || g["status"] != "queued" {
		t.Fatalf("referenceId 应能被 generate 受理：%v %v", g, err)
	}
}

func TestProjectsCreate_同键只建一个(t *testing.T) {
	e, _ := newWriteEnv(t, false)
	args := `{"method":"create","name":"新片","idempotencyKey":"retry-1"}`
	var id string
	for i := 0; i < 3; i++ {
		m := e.mustCall("projects", args)
		p := m["project"].(map[string]any)
		if i == 0 {
			id = p["id"].(string)
		}
		if p["id"] != id || (m["replayed"] == true) != (i > 0) || p["status"] != "active" {
			t.Fatalf("第 %d 次：%v", i+1, m)
		}
	}
	if n := e.count(`select count(*) from project where workspace_id=$1 and name='新片'`, e.ws); n != 1 {
		t.Fatalf("同键三次应只有一个项目：%d", n)
	}
	var key string
	_ = e.pool.QueryRow(context.Background(), `select create_idempotency_key from project where id=$1`, id).Scan(&key)
	if key != domain.ProjectIdemKey(e.p.GrantID, "retry-1") {
		t.Fatalf("幂等键应按授权加前缀：%s", key)
	}
	// 并发同键：仍只有一个。
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = e.call(e.p, "projects", `{"method":"create","name":"并发","idempotencyKey":"race"}`)
		}()
	}
	wg.Wait()
	if n := e.count(`select count(*) from project where workspace_id=$1 and name='并发'`, e.ws); n != 1 {
		t.Fatalf("并发同键应只有一个项目：%d", n)
	}
	// 另一个授权用同一个键：是另一个项目（两个客户端的键互不命中）。
	k, _ := e.keys.CreateKey(context.Background(), e.owner, mcpapp.CreateKeyInput{WorkspaceID: e.ws,
		ClientName: "Cursor", Scopes: []string{domain.ScopeView, domain.ScopeEdit}})
	p2, _ := e.resolver.Resolve(context.Background(), k.Plaintext)
	m, _, err := e.call(p2, "projects", args)
	if err != nil || m["project"].(map[string]any)["id"] == id {
		t.Fatalf("别的授权同键应建新项目：%v %v", m, err)
	}
}

func TestDocumentsPut_来源记为mcp且版本冲突(t *testing.T) {
	e, _ := newWriteEnv(t, false)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	m := e.mustCall("documents", `{"method":"put","projectId":"`+itoa(pid)+`","path":"/notes/idea.md","content":"v1","version":0}`)
	doc := m["document"].(map[string]any)
	if m["created"] != true || doc["source"] != "mcp" || doc["version"].(float64) != 1 {
		t.Fatalf("新建：%v", m)
	}
	m = e.mustCall("documents", `{"method":"put","projectId":"`+itoa(pid)+`","path":"/notes/idea.md","content":"v2","version":1}`)
	if m["created"] != false || m["document"].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("更新：%v", m)
	}
	var src, content string
	_ = e.pool.QueryRow(context.Background(), `select source, content from project_document where project_id=$1`, pid).
		Scan(&src, &content)
	if src != "mcp" || content != "v2" {
		t.Fatalf("库里应记 mcp：%s %s", src, content)
	}
	// 拿旧版本再写：40902，说得出是外部客户端改的，且不是「本人另一个窗口」。
	_, _, err := e.call(e.p, "documents", `{"method":"put","projectId":"`+itoa(pid)+`","path":"/notes/idea.md","content":"旧稿","version":1}`)
	var be *httpx.BizError
	if !asBiz(err, &be) || be.Code != errcode.VersionConflict || be.Details["currentVersion"] != 2 ||
		be.Details["currentSource"] != "mcp" || be.Details["bySelf"] != false {
		t.Fatalf("应 40902 且带当前版本与来源：%v %+v", err, be)
	}
	// 用户本人在网页上拿旧版本保存：冲突说的是外部客户端，不是「你在另一个窗口」（否则「以我为准」会覆盖它）。
	_, err = e.mcpUserSave(pid, 1)
	if !asBiz(err, &be) || be.Details["bySelf"] != false || !strings.Contains(be.Error(), "外部 AI 客户端") {
		t.Fatalf("用户保存冲突应说是外部客户端：%v", err)
	}
	// 同路径再新建：重名。
	if _, _, err := e.call(e.p, "documents", `{"method":"put","projectId":"`+itoa(pid)+`","path":"/notes/idea.md","content":"x","version":0}`); bizCode(err) !=
		errcode.DuplicateName {
		t.Fatalf("同路径新建应重名：%v", err)
	}
}
