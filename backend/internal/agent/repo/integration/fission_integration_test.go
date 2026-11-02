//go:build integration

// 素材裂变（MKT-T-28、MKT-F-53）：规划时按目标模型能力走「视频生视频」或「重新生成」并写明各花多少，
// 执行时发给上游的输入与条目一致 —— 真库 + 真目录报价（QuoteWithFacts）+ 真受理 + 真账本。
//
// ⚠️ 走不走 video2video 只看报价那一份判断（绑定 ∧ 启用 ∧ 能力矩阵 ∧ 有价），不在 agent 里另写模型名单
// （GAP-22）：这里改一行 model_operation.is_enabled，路线就跟着变。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenttools "github.com/li904338203/tabtv/backend/internal/agent/tools"
	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

const fissionSource = "880101"

// seedFissionCatalog：m-v2v 绑了 video2video（恰好 1 个视频，10/秒）与 ref2video（图 ≤ 4，8/秒）；
// m-ref 只绑 ref2video（图 ≤ 4，6/秒）。
func seedFissionCatalog(t *testing.T, e *paidEnv) {
	t.Helper()
	ctx := context.Background()
	x := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	x(`insert into api_protocol (code, name, is_async) values ('openai_images','OpenAI 图片',false) on conflict (code) do nothing`)
	x(`insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,'video2video','视频生视频','video','video,effect','video','enabled') on conflict (code) do nothing`, e.next())
	x(`insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
		values ($1,'ref2video','参考生视频','video','text,image,audio','video','enabled') on conflict (code) do nothing`, e.next())
	model := func(code string, binds map[string]struct {
		role     string
		min, max int
		price    int
	}) {
		pid, mid := e.next(), e.next()
		x(`insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
			values ($1,$2,$2,'http','http://127.0.0.1:1/v1','none','enabled')`, pid, "p-"+code)
		x(`insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
			submit_path, est_seconds, status, sort) values ($1,$2,$2,$3,'video','up','openai_images','/x',20,'enabled',0)`,
			mid, code, pid)
		for op, b := range binds {
			x(`insert into model_operation (id, model_id, operation_id, is_enabled)
				select $1, $2, o.id, true from operation o where o.code = $3`, e.next(), mid, op)
			x(`insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
				select $1, $2, o.id, $3, $4, $5 from operation o where o.code = $6`, e.next(), mid, b.role, b.min, b.max, op)
			x(`insert into pricing (id, operation_id, model_id, credits, unit, status)
				select $1, o.id, $2, $3, 'per_second', 'enabled' from operation o where o.code = $4`, e.next(), mid, b.price, op)
		}
	}
	type bind = struct {
		role     string
		min, max int
		price    int
	}
	model("m-v2v", map[string]bind{"video2video": {"video", 1, 1, 10}, "ref2video": {"image", 0, 4, 8}})
	model("m-ref", map[string]bind{"ref2video": {"image", 0, 4, 6}})
}

// seedFission 建一个带源视频的营销会话（3 张商品图、无角色、源视频 fissionSource）。
func seedFission(t *testing.T, e *paidEnv, state domain.State) repo.Session {
	t.Helper()
	si := `{"products":["101","102","103"],"product":{"name":"保温杯","sellingPoints":"24 小时保温"},` +
		`"characters":[],"sourceVideo":{"kind":"media","id":"` + fissionSource + `"},` +
		`"picks":{"creative":null,"hook":null,"style":null},"pins":[]}`
	s, err := e.r.Create(context.Background(), e.pool, repo.Session{ID: e.next(), WorkspaceID: ws, ProjectID: project,
		PayerAccountID: payer, CreatedByUserID: user, State: state, ApprovedBudget: 500,
		Prefs: json.RawMessage(`{"reasoning":{"model":"m-agent"}}`), Scene: domain.SceneMarketing,
		SceneInput: json.RawMessage(si)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fissionSubmitPlan 是接真目录的 submit_plan（报价与能力判定 = wiring.AgentPlanQuoter / AgentPlanScene）。
func fissionSubmitPlan(e *paidEnv) agentapp.Tool {
	cat := catalogapp.New(catalogrepo.New(e.pool), e.next)
	for _, tl := range agenttools.PlanningTools(agenttools.PlanningDeps{
		Store: e.r, Canvas: wiring.AgentCanvas{Svc: e.canvas},
		Pricer:   wiring.AgentPlanQuoter{Catalog: cat},
		Scene:    wiring.AgentPlanScene{Catalog: cat},
		Balances: wiring.AgentAccounts{Billing: billingapp.New(e.bill)},
		NextID:   e.next, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}) {
		if tl.Name() == domain.ToolSubmitPlan {
			return tl
		}
	}
	return nil
}

func latestPlan(t *testing.T, e *paidEnv, sessionID int64) domain.PlanConfirmPayload {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `select payload from agent_card
		where session_id = $1 and kind = 'plan_confirm' order by created_at desc, id desc limit 1`, sessionID).
		Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var p domain.PlanConfirmPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFission_video2video与重新生成两条路报价(t *testing.T) {
	e := newPaidEnv(t, map[int64]int{payer: 1000})
	seedFissionCatalog(t, e)
	s := seedFission(t, e, domain.StatePlanning)
	tool := fissionSubmitPlan(e)
	items := `[{"type":"video","modelCode":"m-v2v","operationCode":"ref2video","durationSec":5,"key":"v1","variant":1,"variantLabel":"Hook：反问开场"},` +
		`{"type":"video","modelCode":"m-ref","operationCode":"ref2video","durationSec":5,"key":"v2","variant":2,"variantLabel":"风格：极简白"}]`
	run := func() domain.PlanConfirmPayload {
		t.Helper()
		rc := e.step(t, s, "submit_plan")
		rc.State = domain.StatePlanning
		if _, err := tool.Run(context.Background(), rc, json.RawMessage(`{"title":"裂变","items":`+items+`}`)); err != nil {
			t.Fatal(err)
		}
		return latestPlan(t, e, s.ID)
	}

	p := run()
	v1, v2 := p.Items[0], p.Items[1]
	// m-v2v 绑了 video2video 且报得出价 → 走视频生视频：输入恰好 1 个视频、按 video2video 的价（10 × 5）。
	if v1.FissionRoute != domain.FissionVideo2Video || v1.OperationCode != "video2video" || v1.SourceVideoID != fissionSource ||
		v1.Inputs["video"] != 1 || v1.Inputs["image"] != 0 || v1.Credits != 50 {
		t.Fatalf("v1 应走 video2video、50 积分：%+v", v1)
	}
	if !strings.Contains(v1.FissionNote, "视频生视频") || strings.Contains(v1.FissionNote, "✦") {
		t.Fatalf("v1 说明应写明路径且不出现金额：%q", v1.FissionNote)
	}
	// m-ref 没绑 video2video → 重新生成：按 ref2video + 3 张商品图报价（6 × 5），不发源视频。
	if v2.FissionRoute != domain.FissionRegenerate || v2.OperationCode != "ref2video" || v2.Inputs["video"] != 0 ||
		v2.Inputs["image"] != 3 || v2.Credits != 30 {
		t.Fatalf("v2 应走重新生成、30 积分：%+v", v2)
	}
	if !strings.Contains(v2.FissionNote, "不支持以视频为输入") || strings.Contains(v2.FissionNote, "✦") {
		t.Fatalf("v2 说明应写明原因且不出现金额：%q", v2.FissionNote)
	}
	if p.Total != 80 || !p.CanApprove {
		t.Fatalf("合计 = 各行之和：%d %v", p.Total, p.CanApprove)
	}

	// 运营停用 m-v2v 的 video2video 绑定 → 同一份判断把它改判成重新生成（按 ref2video 8 × 5）。
	if _, err := e.pool.Exec(context.Background(), `update model_operation set is_enabled = false
		where model_id = (select id from model where code = 'm-v2v')
		  and operation_id = (select id from operation where code = 'video2video')`); err != nil {
		t.Fatal(err)
	}
	p = run()
	if v1 := p.Items[0]; v1.FissionRoute != domain.FissionRegenerate || v1.OperationCode != "ref2video" ||
		v1.Credits != 40 || v1.Inputs["video"] != 0 || strings.Contains(v1.FissionNote, "✦") {
		t.Fatalf("绑定停用后应改走重新生成：%+v", v1)
	}
	// 规划不扣费。
	if n := e.count(t, `select count(*) from credit_transaction where change < 0`); n != 0 {
		t.Fatalf("规划不应扣费：%d", n)
	}
}

func TestFission_执行时输入与条目一致(t *testing.T) {
	e, probe := newMarketingEnv(t, 100)
	ctx := context.Background()
	s := seedFission(t, e, domain.StateExecuting)
	v2v := domain.Video2VideoItem(domain.PlanItem{Kind: "video", Key: "f1", Label: "裂变 · 反问开场", ModelCode: "dev_video",
		Quantity: 5, Unit: "second", Credits: paidPrice}, fissionSource)
	regen := domain.RegenerateItem(videoItem("f2", []string{"102", "101"}, []int{2, 1}), fissionSource, "所选模型不支持以视频为输入")
	regen.Inputs = map[string]int{"image": 2}
	planCard(t, e, s, []domain.PlanItem{v2v, regen}, "approve")

	gen := func(key string) int64 {
		t.Helper()
		rc := e.step(t, s, "generate")
		rc.Scene = domain.SceneMarketing
		if _, err := e.tool(domain.ToolGenerate).Run(ctx, rc, json.RawMessage(`{"planItemKey":"`+key+`","wait":false}`)); err != nil {
			t.Fatalf("%s：%v", key, err)
		}
		var id int64
		if err := e.pool.QueryRow(ctx, `select id from generation order by created_at desc, id desc limit 1`).Scan(&id); err != nil {
			t.Fatalf("%s：没有落任务 %v", key, err)
		}
		return id
	}
	refs := func(raw json.RawMessage) []string {
		var arr []map[string]any
		_ = json.Unmarshal(raw, &arr)
		out := []string{}
		for _, x := range arr {
			out = append(out, x["kind"].(string)+":"+x["referenceId"].(string))
		}
		return out
	}

	// video2video：受理只收到源视频一个输入，快照里同样只有它（操作改写由单测钉住）。
	id1 := gen("f1")
	if got := refs(probe.seen[0]); !equalStrings(got, []string{"reference:" + fissionSource}) {
		t.Fatalf("视频生视频只发源视频：%v", got)
	}
	var snap json.RawMessage
	if err := e.pool.QueryRow(ctx, `select inputs_snapshot from generation where id = $1`, id1).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	if got := refs(snap); !equalStrings(got, []string{"reference:" + fissionSource}) {
		t.Fatalf("快照里的输入 = 条目：%v", got)
	}

	// 重新生成：只有条目上的商品图（按条目顺序），没有源视频。
	gen("f2")
	if got := refs(probe.seen[1]); !equalStrings(got, []string{"reference:102", "reference:101"}) {
		t.Fatalf("重新生成只发商品图：%v", got)
	}
	// 两次受理各扣一次。
	if n := e.count(t, `select count(*) from credit_transaction where type = 'consume' and ref_type = 'generation'`); n != 2 {
		t.Fatalf("两次受理各扣一次：%d", n)
	}
}
