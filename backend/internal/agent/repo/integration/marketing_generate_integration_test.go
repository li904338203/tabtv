//go:build integration

// 营销会话 generate 的计划项强约束（MKT-T-22）：真库读「最近一张已批准」的规划卡、真受理落快照、
// Runner 真写 agent_step.plan_item_key / error_code。
//
// ⚠️ 发给上游的请求体（provider_request_log）要真执行器 + 假上游，在 MKT-T-30 E2E 断言；
// 这里钉住它的上游 —— 受理收到的输入与 generation.inputs_snapshot 就是确认卡上换后的那几张。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// declaredProbe 是受理侧的输入解析：记下受理收到的声明输入，原样作为解析结果（进 inputs_snapshot）。
type declaredProbe struct {
	mu   sync.Mutex
	seen []json.RawMessage
}

func (p *declaredProbe) ResolveForAccept(_ context.Context, _, _ int64, _, _ string,
	declared json.RawMessage) (genapp.ResolvedInputs, error) {
	p.mu.Lock()
	p.seen = append(p.seen, append(json.RawMessage(nil), declared...))
	p.mu.Unlock()
	var arr []json.RawMessage
	_ = json.Unmarshal(declared, &arr)
	if len(arr) == 0 {
		declared = json.RawMessage(`[]`)
	}
	return genapp.ResolvedInputs{Inputs: declared, RoleCounts: map[string]int{"image": len(arr)}}, nil
}

// newMarketingEnv 在付费环境上接营销计划来源（真 agent_session / agent_card）与记录型输入解析。
func newMarketingEnv(t *testing.T, balance int) (*paidEnv, *declaredProbe) {
	t.Helper()
	e := newPaidEnv(t, map[int64]int{payer: balance})
	probe := &declaredProbe{}
	e.gen.SetInputResolver(probe)
	ac := wiring.AgentCanvas{Svc: e.canvas}
	e.tools[1] = agenttools.Generate{Canvas: ac, Quoter: paidQuoter{paidPrice}, Generator: e.cgen, Budget: e.budget,
		Plans: wiring.AgentPlanSource{Repo: e.r}}
	reg, err := agenttools.NewRegistry(e.tools...)
	if err != nil {
		t.Fatal(err)
	}
	e.reg = reg
	return e, probe
}

// seedMarketing 建营销会话（3 张商品图、1 个媒体角色、建会话时钉住创意 unbox@2）+ 一个用户轮次。
func seedMarketing(t *testing.T, e *paidEnv) (repo.Session, repo.Turn) {
	t.Helper()
	ctx := context.Background()
	si := `{"products":["101","102","103"],"product":{"name":"保温杯","sellingPoints":"24 小时保温"},` +
		`"characters":[{"kind":"media","id":"301"}],"sourceVideo":null,` +
		`"picks":{"creative":{"code":"unbox","version":2,"name":"开箱"},"hook":null,"style":null},` +
		`"pins":[{"kind":"hook","code":"q3","version":1,"source":"plan"}]}`
	s, err := e.r.Create(ctx, e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: domain.StateExecuting, ApprovedBudget: 500,
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"}}`), Scene: domain.SceneMarketing,
		SceneInput: json.RawMessage(si)})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.r.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Append(ctx, tx, repo.Message{ID: e.next(), SessionID: s.ID, Role: repo.RoleUser,
		Content: "给保温杯做条开箱视频"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tr, err := e.r.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: s.ID, Trigger: domain.TriggerUser})
	if err != nil {
		t.Fatal(err)
	}
	return s, tr
}

// planCard 发一张规划卡；decision 非空时按它提交（approve = 已批准）。
func planCard(t *testing.T, e *paidEnv, s repo.Session, items []domain.PlanItem, decision string) repo.Card {
	t.Helper()
	ctx := context.Background()
	payload, _ := json.Marshal(domain.PlanConfirmPayload{Title: "营销规划", Items: items, CanApprove: true})
	rc := e.step(t, s, "submit_plan")
	c, err := e.r.InsertCard(ctx, e.pool, repo.Card{ID: e.next(), SessionID: s.ID, StepID: rc.StepID,
		Kind: domain.CardPlanConfirm, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if decision != "" {
		if ok, err := e.r.SubmitCard(ctx, e.pool, c.ID, json.RawMessage(`{"decision":"`+decision+`"}`), user); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	return c
}

func videoItem(key string, mediaIDs []string, indexes []int) domain.PlanItem {
	limit := 3
	return domain.PlanItem{Kind: "video", Key: key, Label: "开箱 · " + key, ModelCode: "dev_video",
		OperationCode: "img2video", Quantity: 5, Unit: "second", Credits: paidPrice,
		ProductIndexes: indexes, ProductMediaIDs: mediaIDs, ProductTotal: 3, ImageLimit: &limit,
		Inputs: map[string]int{"image": len(mediaIDs) + 1},
		Picks: []domain.PlanPick{{Kind: "creative", Code: "unbox", Version: 2, Name: "开箱"},
			{Kind: "hook", Code: "q3", Version: 1, Name: "三问"}}}
}

func TestMarketingGenerate_请求体图片与卡片一致(t *testing.T) {
	e, probe := newMarketingEnv(t, 100)
	ctx := context.Background()
	s, _ := seedMarketing(t, e)
	// 旧的已批准卡（#1）、换图后批准的新卡（#3、#1）、之后发出但还没确认的卡（#2）：以「最近一张已批准」为准。
	planCard(t, e, s, []domain.PlanItem{videoItem("v1", []string{"101"}, []int{1})}, "approve")
	planCard(t, e, s, []domain.PlanItem{videoItem("v1", []string{"103", "101"}, []int{3, 1})}, "approve")
	planCard(t, e, s, []domain.PlanItem{videoItem("v1", []string{"102"}, []int{2})}, "")
	planCard(t, e, s, []domain.PlanItem{videoItem("v1", []string{"102", "101"}, []int{2, 1})}, "adjust")

	rc := e.step(t, s, "generate")
	rc.Scene = domain.SceneMarketing
	res, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(
		`{"planItemKey":"v1","prompt":"桌面开箱","wait":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.seen) != 1 {
		t.Fatalf("应受理一次：%d", len(probe.seen))
	}
	var got []map[string]any
	_ = json.Unmarshal(probe.seen[0], &got)
	ids := []string{}
	for _, x := range got {
		ids = append(ids, x["kind"].(string)+":"+x["referenceId"].(string))
	}
	want := []string{"reference:103", "reference:101", "reference:301"}
	if !equalStrings(ids, want) {
		t.Fatalf("受理收到的输入应是确认卡上换后的图（按条目顺序）+ 角色：%v", ids)
	}
	var snap []map[string]any
	if err := e.pool.QueryRow(ctx, `select inputs_snapshot from generation where id = $1`, res.GenerationID).
		Scan(jsonScan(&snap)); err != nil {
		t.Fatal(err)
	}
	if len(snap) != 3 || snap[0]["referenceId"] != "103" || snap[1]["referenceId"] != "101" {
		t.Fatalf("快照里的输入张数与顺序 = 确认卡：%v", snap)
	}
	var node string
	_ = e.pool.QueryRow(ctx, `select node_id from generation where id = $1`, res.GenerationID).Scan(&node)
	var title string
	if err := e.pool.QueryRow(ctx, `select n->>'title' from project, jsonb_array_elements(canvas_data->'nodes') n
		where id = $1 and n->>'id' = $2`, project, node).Scan(&title); err != nil || title != "开箱 · v1" {
		t.Errorf("节点标题 = 条目 label：%q %v", title, err)
	}
}

func TestMarketingGenerate_快照可追溯picks(t *testing.T) {
	e, _ := newMarketingEnv(t, 100)
	ctx := context.Background()
	s, _ := seedMarketing(t, e)
	planCard(t, e, s, []domain.PlanItem{videoItem("v1", []string{"103", "101"}, []int{3, 1})}, "approve")
	rc := e.step(t, s, "generate")
	rc.Scene = domain.SceneMarketing
	// 模型在参数里塞了一个别的选项：服务端覆盖成卡片上的 picks。
	res, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(`{"planItemKey":"v1","wait":false,`+
		`"params":{"marketing":{"picks":[{"kind":"creative","code":"evil","version":9}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	// MKT-D-44：后台按 params_snapshot->'marketing' 回答「这条片子用的是哪个创意的哪一版」。
	var creative string
	var version int
	if err := e.pool.QueryRow(ctx, `select p->>'code', (p->>'version')::int
		from generation, jsonb_array_elements(params_snapshot->'marketing'->'picks') p
		where id = $1 and p->>'kind' = 'creative'`, res.GenerationID).Scan(&creative, &version); err != nil {
		t.Fatal(err)
	}
	if creative != "unbox" || version != 2 {
		t.Fatalf("快照应可追溯到 unbox@2：%s@%d", creative, version)
	}
	if n := e.count(t, `select count(*) from generation where id = $1
		and params_snapshot->'marketing' = '{"picks":[{"kind":"creative","code":"unbox","version":2},{"kind":"hook","code":"q3","version":1}]}'::jsonb`,
		res.GenerationID); n != 1 {
		t.Errorf("picks 只有 (kind, code, version)、模型塞的被覆盖")
	}
}

func TestMarketingGenerate_失败记error_code(t *testing.T) {
	e, probe := newMarketingEnv(t, 4) // 余额只够不了一次（paidPrice=10）
	ctx := context.Background()
	s, tr := seedMarketing(t, e)
	planCard(t, e, s, []domain.PlanItem{videoItem("v1", []string{"103", "101"}, []int{3, 1}),
		videoItem("v2", []string{"102"}, []int{2})}, "approve")
	chat := &scriptedChat{outs: []agentapp.ChatOutput{
		call("c1", "generate", `{"modelCode":"dev_video","operationCode":"text2video"}`),                             // 无 key
		call("c2", "generate", `{"planItemKey":"v9"}`),                                                               // 不在计划里
		call("c3", "generate", `{"planItemKey":"v1","modelCode":"kling"}`),                                           // 冲突
		call("c4", "generate", `{"planItemKey":"v2","inputs":[{"kind":"reference","referenceId":"101","order":1}]}`), // 换图
		call("c5", "generate", `{"planItemKey":"v1","wait":false}`),                                                  // 余额不足
	}}
	if _, err := marketingRunner(e, chat).RunTurn(ctx, s.ID, tr.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := e.pool.Query(ctx, `select coalesce(plan_item_key, ''), coalesce(error_code, ''), status
		from agent_step where session_id = $1 and turn_id = $2 order by started_at, id`, s.ID, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var k, c, st string
		if err := rows.Scan(&k, &c, &st); err != nil {
			t.Fatal(err)
		}
		got = append(got, k+"|"+c+"|"+st)
	}
	want := []string{
		"|plan_item_not_approved|failed",
		"v9|plan_item_not_approved|failed",
		"v1|plan_item_mismatch|failed",
		"v2|plan_item_mismatch|failed",
		"v1|INSUFFICIENT_CREDITS|failed",
	}
	if !equalStrings(got, want) {
		t.Fatalf("步骤的计划项键与错误码：\n%v\n期望\n%v", got, want)
	}
	// 拒绝的四次没有任何受理；余额不足那次受理一次、不扣费、不留任务。
	if len(probe.seen) != 1 || e.cgen.accepts.Load() != 1 || len(e.generations(t)) != 0 {
		t.Errorf("拒绝不受理：resolve=%d accepts=%d gens=%d", len(probe.seen), e.cgen.accepts.Load(), len(e.generations(t)))
	}
	if bal, _ := e.bill.Balance(ctx, payer); bal != 4 {
		t.Errorf("余额不变：%d", bal)
	}
	// 拒绝回给模型的是结构化结果（码 + 条目），不是一句话。
	msgs, _ := e.r.ListRecent(ctx, s.ID, 50)
	var structured int
	for _, m := range msgs {
		if m.Role != repo.RoleTool {
			continue
		}
		var c map[string]any
		if json.Unmarshal([]byte(m.Content), &c) == nil && (c["error"] == domain.ToolErrPlanItemMismatch ||
			c["error"] == domain.ToolErrPlanItemNotApproved) {
			structured++
		}
	}
	if structured != 4 {
		t.Errorf("四次拒绝都应回结构化结果：%d", structured)
	}

	// 成功的那一步记计划项键（充值后重新执行 v1）。
	if _, err := e.bill.Grant(ctx, billingrepo.GrantInput{PayerID: payer, Source: "purchase", Amount: 100,
		SourceRefID: "mkt-" + strconv.FormatInt(e.next(), 10)}); err != nil {
		t.Fatal(err)
	}
	// 上一轮结束后会话回到空闲；这里直接放回执行态（批准流程不是本用例要测的）。
	if _, err := e.pool.Exec(ctx, `update agent_session set state = 'executing' where id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	tr2, err := e.r.InsertTurn(ctx, e.pool, repo.Turn{ID: e.next(), SessionID: s.ID, Trigger: domain.TriggerUser})
	if err != nil {
		t.Fatal(err)
	}
	chat2 := &scriptedChat{outs: []agentapp.ChatOutput{call("d1", "generate", `{"planItemKey":"v1","wait":false}`)}}
	if _, err := marketingRunner(e, chat2).RunTurn(ctx, s.ID, tr2.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `select count(*) from agent_step where turn_id = $1 and plan_item_key = 'v1'
		and error_code is null and status = 'waiting' and generation_id is not null`, tr2.ID); n != 1 {
		var dump string
		_ = e.pool.QueryRow(ctx, `select coalesce(json_agg(json_build_object('k', plan_item_key, 'c', error_code,
			's', status, 'g', generation_id, 'e', error_message))::text, '') from agent_step where turn_id = $1`, tr2.ID).Scan(&dump)
		t.Errorf("成功受理的步骤记计划项键、不记错误码：%s", dump)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// jsonScan 把 jsonb 列扫进任意值。
func jsonScan(dst any) any { return &jsonDst{dst} }

type jsonDst struct{ v any }

func (j *jsonDst) Scan(src any) error {
	switch b := src.(type) {
	case []byte:
		return json.Unmarshal(b, j.v)
	case string:
		return json.Unmarshal([]byte(b), j.v)
	}
	return nil
}

// marketingRunner 同 paidEnv.runner，调用上限放宽到 10（一轮里连发五次 generate）。
func marketingRunner(e *paidEnv, chat agentapp.ChatModel) *agentapp.Runner {
	return agentapp.NewRunner(agentapp.RunnerDeps{
		Store: agentapp.NewRepoStore(e.r), Chat: chat, Quoter: paidQuoter{paidPrice},
		Ledger:  wiring.AgentLedger{Repo: e.bill, Compensate: 30 * 24 * time.Hour},
		Members: paidMembers{collab.RoleEditor, true}, Canvas: wiring.AgentCanvas{Svc: e.canvas}, Tools: e.reg,
		NextID: e.next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, agentapp.RunnerConfig{MaxCalls: 10, ContextChars: 60000})
}
