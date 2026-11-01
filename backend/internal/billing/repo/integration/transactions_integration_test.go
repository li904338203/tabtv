//go:build integration

package integration

import (
	"context"
	"strconv"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/billing/repo"
)

// ListTransactions 的展示关联（2026-11-01：积分中心的流水要说清「哪个项目、什么生成、哪个模型」）。
//
// ⚠️ 钉住三条，都由 SQL 决定，所以用真库：
//  1. 四类消耗（生成 / 提示词合成 / Agent 对话 / 成片渲染）都关得回项目；生成带操作名与模型名，
//     Agent 对话带会话标题与那一轮的模型，成片渲染带时间线名；已删除的项目照常给名字并标出来；
//  2. 关联只认本付费主体：账本上的 ref_id 指向别人的生成时什么都不带 —— 不能借一个 ID 读到别人的项目名；
//  3. 关不上的（购买、非数字 ref_id）照常列出、字段为空：流水本身不能因为关联少一行或整页报错。
func TestListTransactions_带出项目与生成类型且只认本付费主体(t *testing.T) {
	r, pool, next := usageSetup(t)
	ctx := context.Background()
	x := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	mine := mkPayer(t, pool, next)
	other := mkPayer(t, pool, next)
	ws := next()

	project, gone := next(), next()
	x(`insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status)
		values ($1, $2, 1, '演唱会舞台', '{}', 'active')`, project, ws)
	x(`insert into project (id, workspace_id, created_by_user_id, name, canvas_data, status, deleted_at)
		values ($1, $2, 1, '旧项目', '{}', 'deleted', now())`, gone, ws)

	x(`insert into operation (id, code, name, domain, out_node_type, status)
		values ($1, 'text2img', '文生图', 'image', 'image', 'enabled') on conflict (code) do nothing`, next())
	var opID int64
	if err := pool.QueryRow(ctx, `select id from operation where code = 'text2img'`).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	x(`insert into api_protocol (code, name, is_async) values ('openai_images', 'OpenAI 图片', false)
		on conflict (code) do nothing`)
	provider, model := next(), next()
	x(`insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1, 'p-it-ledger', 'p-it-ledger', 'http', 'http://127.0.0.1:1/v1', 'none', 'enabled')`, provider)
	x(`insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
		submit_path, est_seconds, status, sort)
		values ($1, 'it-seedream', 'Seedream 测试', $2, 'image', 'up', 'openai_images', '/x', 20, 'enabled', 0)`,
		model, provider)

	gen := func(payer int64) int64 {
		id := next()
		x(`insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id,
			operation_id, model_id, result_type, credits_cost, idempotency_key, status)
			values ($1, $2, $3, 1, $4, $5, $6, 'image', 48, $7, 'success')`,
			id, ws, payer, project, opID, model, "it-"+strconv.FormatInt(id, 10))
		return id
	}
	myGen, othersGen := gen(mine), gen(other)

	script, call := next(), next()
	x(`insert into script_instance (id, workspace_id, project_id, canvas_node_id, source_type)
		values ($1, $2, $3, 'n1', 'manual')`, script, ws, project)
	x(`insert into script_llm_call (id, workspace_id, payer_account_id, script_id, purpose, model_id, idempotency_key)
		values ($1, $2, $3, $4, 'prompt_compose', $5, 'it-call')`, call, ws, mine, script, model)

	session, turn := next(), next()
	x(`insert into agent_session (id, workspace_id, project_id, payer_account_id, created_by_user_id, title, state, last_active_at)
		values ($1, $2, $3, $4, 1, '做一条宣传片', 'idle', now())`, session, ws, project, mine)
	x(`insert into agent_turn (id, session_id, trigger, model_code) values ($1, $2, 'user', 'it-seedream')`, turn, session)

	timeline, render := next(), next()
	x(`insert into timeline (id, workspace_id, project_id, name) values ($1, $2, $3, '成片 A')`, timeline, ws, gone)
	x(`insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot, payer_account_id,
		created_by_user_id, status) values ($1, $2, $3, 1, '{}'::jsonb, $4, 1, 'success')`, render, ws, timeline, mine)

	if _, err := r.Grant(ctx, repo.GrantInput{PayerID: mine, Source: "purchase", Amount: 1000, Reason: "订单 1"}); err != nil {
		t.Fatal(err)
	}
	itoa := func(v int64) string { return strconv.FormatInt(v, 10) }
	consume := func(refType, refID string) {
		t.Helper()
		if _, err := r.Consume(ctx, repo.ConsumeInput{PayerID: mine, Amount: 10, RefType: refType, RefID: refID, Reason: "测试"}); err != nil {
			t.Fatal(err)
		}
	}
	consume("generation", itoa(myGen))
	consume("generation", itoa(othersGen)) // 账本上指向别人的生成（不该发生，但不能因此串账）
	consume("generation", "not-a-number")
	consume("script_llm", itoa(call))
	consume("agent_turn", itoa(turn))
	consume("render", itoa(render))

	rows, err := r.ListTransactions(ctx, mine, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 7 {
		t.Fatalf("应列出全部 7 条流水（关不上的也在），得到 %d 条", len(rows))
	}
	byRef := map[string]repo.Transaction{}
	for _, row := range rows {
		byRef[row.RefType+":"+row.RefID] = row
	}

	g := byRef["generation:"+itoa(myGen)]
	if g.ProjectID != project || g.ProjectName != "演唱会舞台" || g.ProjectDeleted ||
		g.OperationCode != "text2img" || g.OperationName != "文生图" || g.ModelName != "Seedream 测试" {
		t.Errorf("生成：应带出项目、操作与模型，得到 %+v", g)
	}
	if o := byRef["generation:"+itoa(othersGen)]; o.ProjectID != 0 || o.ProjectName != "" || o.OperationName != "" || o.ModelName != "" {
		t.Errorf("别人的生成：什么都不该带出来，得到 %+v", o)
	}
	if n := byRef["generation:not-a-number"]; n.ProjectID != 0 || n.OperationName != "" {
		t.Errorf("非数字 ref_id：照常列出、字段为空，得到 %+v", n)
	}
	if s := byRef["script_llm:"+itoa(call)]; s.ProjectName != "演唱会舞台" || s.ModelName != "Seedream 测试" || s.OperationName != "" {
		t.Errorf("提示词合成：应带出项目与模型、没有操作名，得到 %+v", s)
	}
	if a := byRef["agent_turn:"+itoa(turn)]; a.ProjectName != "演唱会舞台" || a.RefTitle != "做一条宣传片" || a.ModelName != "Seedream 测试" {
		t.Errorf("Agent 对话：应带出项目、会话标题与那一轮的模型，得到 %+v", a)
	}
	if rr := byRef["render:"+itoa(render)]; rr.ProjectID != gone || rr.ProjectName != "旧项目" || !rr.ProjectDeleted || rr.RefTitle != "成片 A" {
		t.Errorf("成片渲染：应带出（已删除的）项目与时间线名，得到 %+v", rr)
	}
	for _, row := range rows {
		if row.Type == "grant" && (row.ProjectID != 0 || row.ProjectName != "" || row.ModelName != "") {
			t.Errorf("购买到账没有业务表可关，字段应为空，得到 %+v", row)
		}
	}
}
