//go:build integration

// 对外 MCP 多段工具集（MCPT-16）的真库测试：脚本 / 整图 / 时间线走真实的装配（wiring.BuildCore 的脚本、
// 整图、生成服务，wiring.NewTimelineService，wiring.NewMCPToolDeps 的委托适配器与确认服务）。
//
// ⚠️ 不触发任何上游：分镜的文本模型换成进程内假模型（SetTextModel），生成任务只受理不跑 Worker。
// ⚠️ 「只扣一次」「解析失败退款」「取消只退一次」都要真库才证明得了：用假端口的测试把账本整条删掉照样绿。
package integration

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	contentapp "github.com/li904338203/tabtv/backend/internal/content/app"
	contentrepo "github.com/li904338203/tabtv/backend/internal/content/repo"
	identityapp "github.com/li904338203/tabtv/backend/internal/identity/app"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	mcpapp "github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	mcprepo "github.com/li904338203/tabtv/backend/internal/mcp/repo"
	"github.com/li904338203/tabtv/backend/internal/mcp/tools"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/seed"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// fakeText 是进程内文本模型：想让它回什么就回什么（不发网络）。
type fakeText struct {
	mu    sync.Mutex
	reply string
	calls int
}

func (f *fakeText) Complete(context.Context, scriptapp.TextRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.reply, nil
}

func (f *fakeText) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

const goodStoryboard = `{"globalStyle":"水墨","shots":[
 {"description":"@唐僧 走在山路上","durationSec":5,"entities":[{"name":"唐僧","type":"character"}]},
 {"description":"山门前","durationSec":4}],
 "assets":[{"name":"唐僧","type":"character","appearance":"僧袍"}]}`

type multiEnv struct {
	*env
	text *fakeText
}

// newMultiEnv：真实装配 + 主体带全部 scope 与全部工具集（多段工具集默认关闭）。
func newMultiEnv(t *testing.T, withSeed bool) *multiEnv {
	t.Helper()
	e := newEnv(t)
	if withSeed {
		sqlDB := stdlib.OpenDBFromPool(e.pool)
		if err := seed.Apply(context.Background(), sqlDB, e.next); err != nil {
			t.Fatal(err)
		}
		_ = sqlDB.Close()
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := wiring.BuildCore(wiring.CoreInput{Cfg: config.Config{},
		Deps: &bootstrap.Deps{DB: &db.Pool{Pool: e.pool}}, NextID: e.next, Logger: logger,
		Audit: audit.New(e.pool, e.next, logger), Media: e.media, Catalog: e.catalog})
	if err != nil {
		t.Fatal(err)
	}
	text := &fakeText{reply: goodStoryboard}
	core.Script.SetTextModel(text)
	tl := wiring.NewTimelineService(wiring.TimelineInput{Pool: e.pool, NextID: e.next, Media: e.media,
		Script: core.Script, Canvas: core.Canvas, Catalog: e.catalog, Logger: logger})
	cr := collabrepo.New(e.pool)
	agentSvc := agentapp.NewService(agentrepo.New(e.pool), e.next, agentapp.ServicePorts{}, logger)
	deps := wiring.NewMCPToolDeps(wiring.MCPReadInput{
		Identity: identityapp.New(identityrepo.New(e.pool), nil, nil, nil, e.next), Collab: collabapp.New(cr, e.next),
		Billing: billingapp.New(billingrepo.New(e.pool, e.next)), Canvas: core.Canvas, GenRepo: core.GenRepo,
		Gen: core.Gen, Catalog: e.catalog, Content: contentapp.New(contentrepo.New(e.pool, e.next)),
		Workflow: core.Workflow, Timeline: tl, Media: e.media, MediaURLTTL: time.Hour,
		Store: mcprepo.New(e.pool), NextID: e.next, QuoteTTL: 10 * time.Minute,
		Ledger: core.Billing, StateKey: mcpapp.DeriveStateKey("integration"),
		Script: core.Script, ScriptRepo: scriptrepo.New(e.pool), Skills: agentSvc,
	})
	if deps.Scripts == nil || deps.Workflows == nil || deps.Timelines == nil || deps.Skills == nil {
		t.Fatal("多段端口没有接上")
	}
	e.reg = tools.NewRegistry(deps)
	k, err := e.keys.CreateKey(context.Background(), e.owner, mcpapp.CreateKeyInput{WorkspaceID: e.ws,
		ClientName: "Claude Code", Scopes: domain.Scopes(), Toolsets: domain.Toolsets()})
	if err != nil {
		t.Fatal(err)
	}
	if e.p, err = e.resolver.Resolve(context.Background(), k.Plaintext); err != nil {
		t.Fatal(err)
	}
	return &multiEnv{env: e, text: text}
}

// seedTextModel 配一个文本模型（openai_chat 协议）+ 操作 + 定价。
func (e *multiEnv) seedTextModel(model, op string, credits int) {
	e.t.Helper()
	exec(e.t, e.pool, `insert into api_protocol (code, name, is_async) values ('openai_chat','OpenAI Chat',false)
		on conflict (code) do nothing`)
	pid := e.next()
	exec(e.t, e.pool, `insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,$2,$2,'http','http://127.0.0.1:1/v1','none','enabled')`, pid, "p-"+model+"-"+op)
	mid := e.next()
	exec(e.t, e.pool, `insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
		submit_path, est_seconds, status, sort) values ($1,$2,$2,$3,'text','up','openai_chat','/chat',20,'enabled',0)
		on conflict (code) do nothing`, mid, model, pid)
	exec(e.t, e.pool, `insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,$2,$2,'text','text','text','enabled') on conflict (code) do nothing`, e.next(), op)
	exec(e.t, e.pool, `insert into model_operation (id, model_id, operation_id, is_enabled)
		select $1, m.id, o.id, true from model m, operation o where m.code = $2 and o.code = $3`, e.next(), model, op)
	exec(e.t, e.pool, `insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
		select $1, m.id, o.id, 'text', 0, 4 from model m, operation o where m.code = $2 and o.code = $3`, e.next(), model, op)
	exec(e.t, e.pool, `insert into pricing (id, operation_id, model_id, credits, unit, status)
		select $1, o.id, m.id, $2, 'per_time', 'enabled' from model m, operation o where m.code = $3 and o.code = $4`,
		e.next(), credits, model, op)
}

func (e *multiEnv) callP(args, name string) (map[string]any, error) {
	e.t.Helper()
	m, _, err := e.call(e.p, name, args)
	return m, err
}

func (e *multiEnv) need(name, args string) map[string]any {
	e.t.Helper()
	m, err := e.callP(args, name)
	if err != nil {
		e.t.Fatalf("%s %s：%v", name, args, err)
	}
	return m
}

func withConfirmOf(args string, m map[string]any) string {
	nc, _ := m["needsConfirmation"].(map[string]any)
	if nc == nil {
		panic("没有 needsConfirmation")
	}
	return strings.TrimSuffix(args, "}") + confirmArgs(nc) + "}"
}

func (e *multiEnv) ledger(refType string) (consumes, refunds, consumed int) {
	e.t.Helper()
	if err := e.pool.QueryRow(context.Background(), `
		select count(*) filter (where type='consume'), count(*) filter (where type='refund'),
		       coalesce(sum(-change) filter (where type='consume'), 0)
		from credit_transaction where payer_account_id=$1 and ref_type=$2`, e.payer, refType).
		Scan(&consumes, &refunds, &consumed); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *multiEnv) createScript(pid int64, key string) (scriptID, nodeID string) {
	e.t.Helper()
	m := e.need("script", `{"method":"create","projectId":"`+itoa(pid)+`","text":"三打白骨精","idempotencyKey":"`+key+`"}`)
	return m["scriptId"].(string), m["nodeId"].(string)
}

// script.create：建的是 MCP 来源的脚本节点（origin 归本授权），scriptId 写回节点；同一幂等键重试不多建。
func TestMcpScriptNode_OwnedByGrant(t *testing.T) {
	e := newMultiEnv(t, false)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid, nodeID := e.createScript(pid, "k1")
	n := canvasNode(t, e.env, pid, nodeID)
	origin, _ := n["origin"].(map[string]any)
	if n["type"] != "script" || origin["kind"] != "mcp" || origin["grantId"] != itoa(e.p.GrantID) || n["agent"] != nil {
		t.Fatalf("脚本节点应归本授权：%v", n)
	}
	params, _ := n["params"].(map[string]any)
	if params["scriptId"] != sid {
		t.Fatalf("scriptId 应写回节点（不写回的话节点在画布上一直是空的）：%v", params)
	}
	sid2, node2 := e.createScript(pid, "k1")
	if sid2 != sid || node2 != nodeID {
		t.Fatalf("同一幂等键应返回同一个脚本与节点：%s/%s vs %s/%s", sid2, node2, sid, nodeID)
	}
	if c := e.count(`select count(*) from script_instance where project_id=$1`, pid); c != 1 {
		t.Fatalf("应只建 1 个脚本：%d", c)
	}
	// 写回那一批与建节点那一批分开记（批次长度 ≤64，写回没有因为批次超长失败）。
	if c := e.count(`select count(*) from canvas_op_log where project_id=$1 and actor_kind='mcp' and mcp_grant_id=$2`,
		pid, e.p.GrantID); c != 2 {
		t.Fatalf("应有建节点 + 写回两批 mcp 操作日志：%d", c)
	}
}

// 分镜：确认前不扣费；同一确认重试 3 次（含并发）只受理一次、账本一笔；与报价同价。
func TestMcpScript_StoryboardConfirmChargesOnce(t *testing.T) {
	e := newMultiEnv(t, false)
	e.seedTextModel("txt-m", "storyboard_gen", 30)
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid, _ := e.createScript(pid, "k1")
	args := `{"method":"storyboard","scriptId":"` + sid + `","modelCode":"txt-m"}`
	m := e.need("script", args)
	if m["status"] != "needs_confirmation" || m["needsConfirmation"].(map[string]any)["credits"].(float64) != 30 {
		t.Fatalf("首次应只报价：%v", m)
	}
	if c, _, _ := e.ledger("script_llm"); c != 0 || e.text.count() != 0 {
		t.Fatal("确认之前不得扣费、不得调模型")
	}
	ca := withConfirmOf(args, m)
	var wg sync.WaitGroup
	results := make([]map[string]any, 3)
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = e.callP(ca, "script")
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("第 %d 次：%v", i+1, errs[i])
		}
	}
	// 再顺序重放一次：拿回原结果。
	m = e.need("script", ca)
	if m["replayed"] != true || m["status"] != "completed" {
		t.Fatalf("重放：%v", m)
	}
	consumes, refunds, consumed := e.ledger("script_llm")
	if consumes != 1 || refunds != 0 || consumed != 30 || e.text.count() != 1 {
		t.Fatalf("应恰好一笔 30 积分、模型调用一次：consumes=%d refunds=%d consumed=%d calls=%d",
			consumes, refunds, consumed, e.text.count())
	}
	if c := e.count(`select count(*) from shot where script_id=$1`, mustID(t, sid)); c != 2 {
		t.Fatalf("应落 2 个镜头：%d", c)
	}
}

// 分镜解析失败：结算晚于校验由脚本域保证（全额退款、不落镜头）；MCP 不绕开它，同一确认重放说清失败、不再调模型。
func TestMcpScript_StoryboardParseFail_Refund(t *testing.T) {
	e := newMultiEnv(t, false)
	e.seedTextModel("txt-m", "storyboard_gen", 30)
	e.credit(100)
	e.text.reply = "模型今天不想干活。"
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid, _ := e.createScript(pid, "k1")
	args := `{"method":"storyboard","scriptId":"` + sid + `","modelCode":"txt-m"}`
	ca := withConfirmOf(args, e.need("script", args))
	if _, err := e.callP(ca, "script"); err == nil {
		t.Fatal("解析失败应报错")
	}
	consumes, refunds, _ := e.ledger("script_llm")
	if consumes != 1 || refunds != 1 {
		t.Fatalf("扣一次、退一次：consumes=%d refunds=%d", consumes, refunds)
	}
	var bal int
	_ = e.pool.QueryRow(context.Background(), `select coalesce(sum(remaining),0) from credit_grant
		where payer_account_id=$1 and status='active'`, e.payer).Scan(&bal)
	if bal != 100 {
		t.Fatalf("应全额退回：余额 %d", bal)
	}
	if c := e.count(`select count(*) from shot where script_id=$1`, mustID(t, sid)); c != 0 {
		t.Fatalf("解析失败不落镜头：%d", c)
	}
	// 同一确认重放：说清那次失败了，不再调模型、不再扣费。
	_, err := e.callP(ca, "script")
	if bizCode(err) != errcode.Conflict || e.text.count() != 1 {
		t.Fatalf("重放应回失败且不再调模型：%v（调用 %d 次）", err, e.text.count())
	}
	if c, _, _ := e.ledger("script_llm"); c != 1 {
		t.Fatalf("重放不得再扣：%d", c)
	}
}

const wfCanvas = `{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},"nodes":[` +
	`{"id":"n_img","type":"image","title":"图","position":{"x":0,"y":0},"size":{"w":330,"h":180},` +
	`"params":{"operation":"text2img","modelCode":"img-m","prompt":"一只猫"}}],"edges":[]}`

// 整图运行：同一确认重试 3 次只建一个运行、子任务只扣一次；取消后子任务退款恰好一次（由整图 / 生成域完成）。
func TestMcpWorkflow_RunRetry3_OneRun(t *testing.T) {
	e := newMultiEnv(t, false)
	e.seedPriced("img-m", "text2img", "image", "per_image", 7, "text")
	e.credit(100)
	pid := e.project(e.ws, "片子", wfCanvas, time.Now())
	plan := e.need("workflow", `{"method":"plan","projectId":"`+itoa(pid)+`"}`)
	if plan["status"] != "executable" || e.count(`select count(*) from workflow_run`) != 0 {
		t.Fatalf("预览只读：%v", plan)
	}
	args := `{"method":"run","projectId":"` + itoa(pid) + `"}`
	m := e.need("workflow", args)
	if m["needsConfirmation"].(map[string]any)["credits"].(float64) != 7 {
		t.Fatalf("按计划报价：%v", m)
	}
	ca := withConfirmOf(args, m)
	var runID string
	for i := 0; i < 3; i++ {
		m = e.need("workflow", ca)
		if i == 0 {
			runID = m["runId"].(string)
		}
		if m["runId"] != runID || (m["replayed"] == true) != (i > 0) {
			t.Fatalf("第 %d 次：%v", i+1, m)
		}
	}
	if c := e.count(`select count(*) from workflow_run where workspace_id=$1`, e.ws); c != 1 {
		t.Fatalf("应只有 1 个运行：%d", c)
	}
	if c, _, sum := e.ledger("generation"); c != 1 || sum != 7 {
		t.Fatalf("子任务应只扣一次 7 积分：%d %d", c, sum)
	}

	// 取消：退款由原域完成、恰好一次；再取消 cancelled=false。
	m = e.need("workflow", `{"method":"cancel","runId":"`+runID+`"}`)
	if m["cancelled"] != true {
		t.Fatalf("取消：%v", m)
	}
	m = e.need("workflow", `{"method":"cancel","runId":"`+runID+`"}`)
	if m["cancelled"] != false {
		t.Fatalf("再取消应 cancelled=false：%v", m)
	}
	if _, r, _ := e.ledger("generation"); r != 1 {
		t.Fatalf("子任务退款应恰好 1 笔：%d", r)
	}
}

// 计划不可执行（模型没配价 → 节点 blocked）时 run 不签发 quote、不建运行。
func TestMcpWorkflow_计划不可执行不报价(t *testing.T) {
	e := newMultiEnv(t, false)
	e.credit(100)
	pid := e.project(e.ws, "片子", wfCanvas, time.Now())
	m := e.need("workflow", `{"method":"run","projectId":"`+itoa(pid)+`"}`)
	if m["status"] == "needs_confirmation" || e.count(`select count(*) from mcp_quote`) != 0 ||
		e.count(`select count(*) from workflow_run`) != 0 {
		t.Fatalf("不可执行的计划不进确认：%v", m)
	}
}

// 渲染预检有问题（空时间线）：直接返回问题清单，不签发 quote、不建渲染任务、不扣费。
func TestMcpTimeline_RenderPrecheckIssues_NoQuote(t *testing.T) {
	e := newMultiEnv(t, true) // 真 seed：timeline_render 的兜底价
	e.credit(100)
	pid := e.project(e.ws, "片子", genCanvas, time.Now())
	sid, _ := e.createScript(pid, "k1")
	// 空跑装配：没有时间线也不建。
	m := e.need("timeline", `{"method":"assemble","scriptId":"`+sid+`"}`)
	if m["status"] != "planned" || e.count(`select count(*) from timeline`) != 0 {
		t.Fatalf("空跑不写库、不建时间线：%v", m)
	}
	m = e.need("timeline", `{"method":"assemble","scriptId":"`+sid+`","dryRun":false}`)
	tid, _ := m["timelineId"].(string)
	if m["status"] != "assembled" || tid == "" || e.count(`select count(*) from timeline`) != 1 {
		t.Fatalf("显式 dryRun=false 才建时间线并装配：%v", m)
	}
	m = e.need("timeline", `{"method":"render","timelineId":"`+tid+`"}`)
	if m["status"] != "not_renderable" || m["issues"] == nil {
		t.Fatalf("预检应不通过：%v", m)
	}
	if e.count(`select count(*) from mcp_quote`) != 0 || e.count(`select count(*) from render_job`) != 0 {
		t.Fatal("预检有问题：不签发 quote、不建渲染任务")
	}
	if c, _, _ := e.ledger("render"); c != 0 {
		t.Fatalf("不得扣费：%d", c)
	}
}

// 提示词：来自真 seed 的启用技能、正文已渲染；停用的不出现。
func TestMcpPrompts_来自启用技能且停用不出现(t *testing.T) {
	e := newMultiEnv(t, true)
	ps := e.reg.Prompts()
	list, err := ps.List(context.Background())
	if err != nil || len(list) == 0 {
		t.Fatalf("%v %v", list, err)
	}
	codes := map[string]bool{}
	for _, p := range list {
		codes[p.Name] = true
		b, err := ps.Get(context.Background(), p.Name)
		if err != nil || strings.Contains(b.Text, "{{") || !strings.Contains(b.Text, "canvas_read") {
			t.Fatalf("%s 应已渲染且带工具名对照：%v", p.Name, err)
		}
	}
	if !codes["video_single"] {
		t.Fatalf("应有 video_single：%v", codes)
	}
	exec(t, e.pool, `update agent_skill set is_enabled=false where code='video_single'`)
	list, _ = ps.List(context.Background())
	for _, p := range list {
		if p.Name == "video_single" {
			t.Fatal("停用的技能不应出现")
		}
	}
	if _, err := ps.Get(context.Background(), "video_single"); err != mcpapp.ErrPromptNotFound {
		t.Fatalf("停用的技能取不到：%v", err)
	}
}

func mustID(t *testing.T, s string) int64 {
	t.Helper()
	var v int64
	for _, c := range s {
		v = v*10 + int64(c-'0')
	}
	return v
}
