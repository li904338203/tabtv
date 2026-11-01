//go:build integration

// 一键优化提示词（AGT-43）的真库 + httptest 测试：走真实路由与权限闸，账本用真库逐笔核对。
package integration

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

// ---------------------------------------------------------------- 假端口

type fakeChat struct {
	mu    sync.Mutex
	out   string
	err   error
	calls int
	last  agentapp.ChatInput
}

func (f *fakeChat) Chat(_ context.Context, in agentapp.ChatInput, _ func(string)) (agentapp.ChatOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = in
	return agentapp.ChatOutput{Text: f.out}, f.err
}

func (f *fakeChat) set(out string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out, f.err = out, err
}

type fakeTurnQuoter struct{ credits int }

func (q fakeTurnQuoter) QuoteTurn(context.Context, string, int64) (agentapp.Quote, error) {
	return agentapp.Quote{Credits: q.credits}, nil
}

func (q fakeTurnQuoter) QuoteGeneration(context.Context, agentapp.QuoteInput) (agentapp.Quote, error) {
	return agentapp.Quote{Credits: q.credits}, nil
}

type fakeReasoning struct{ code string }

func (f fakeReasoning) DefaultReasoningModel(context.Context, int64) (string, error) {
	return f.code, nil
}

// ---------------------------------------------------------------- 工具

func (e *env) optimize(user int64, key, text string) resp {
	e.t.Helper()
	return e.do("POST", "/agent/prompt-optimize", user, key, map[string]any{"text": text})
}

// ledgerCount 某次优化（refID）在账本里的某类流水条数。
func (e *env) ledgerCount(refID, typ string) int {
	e.t.Helper()
	return e.count(`select count(*) from credit_transaction where payer_account_id = $1
		and ref_type = 'agent_turn' and ref_id = $2 and type = $3`, e.payer, refID, typ)
}

func (e *env) balance() int {
	e.t.Helper()
	n, err := e.ledger.Balance(context.Background(), e.payer)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------- 用例

func TestOptimizePrompt_成功扣一次且结果不落到任何会话(t *testing.T) {
	e := newEnv(t, true)
	e.grant(100)
	e.chat.set("“雨夜霓虹街头，悟空视角，镜头缓慢推进”", nil)
	r := e.optimize(e.owner, "opt-ok-1", "三打白骨精，悟空视角")
	if r.status != http.StatusOK {
		t.Fatalf("优化失败：%d %v", r.status, r.body)
	}
	if r.body["text"] != "雨夜霓虹街头，悟空视角，镜头缓慢推进" {
		t.Errorf("整段包裹的引号应去掉：%v", r.body["text"])
	}
	if r.body["credits"].(float64) != 36 || r.body["modelCode"] != "m-reason" || r.body["replayed"] != false {
		t.Errorf("应按轮扣 36、用默认推理模型：%v", r.body)
	}
	id := r.body["id"].(string)
	if e.ledgerCount(id, "consume") != 1 || e.ledgerCount(id, "refund") != 0 {
		t.Errorf("成功应恰好一条 consume、没有 refund")
	}
	if got := e.balance(); got != 64 {
		t.Errorf("余额应为 64：%d", got)
	}
	if e.chat.last.System == "" || len(e.chat.last.Tools) != 0 || e.chat.last.Messages[0].Content != "三打白骨精，悟空视角" {
		t.Errorf("应带系统提示词、不带工具、原文作为 user 消息：%+v", e.chat.last)
	}
	// 不建会话、不建轮次：优化只回结果，不写任何用户数据。
	if n := e.count(`select count(*) from agent_session`); n != 0 {
		t.Errorf("优化不应建会话：%d", n)
	}
	// 同键重放：原样返回，不再调模型、不再扣费。
	calls := e.chat.calls
	r2 := e.optimize(e.owner, "opt-ok-1", "三打白骨精，悟空视角")
	if r2.status != http.StatusOK || r2.body["replayed"] != true || r2.body["id"] != id {
		t.Fatalf("同键重放应返回同一次结果：%d %v", r2.status, r2.body)
	}
	if e.chat.calls != calls || e.ledgerCount(id, "consume") != 1 {
		t.Errorf("同键重放不应再调模型或扣费")
	}
}

func TestOptimizePrompt_空输出全额退款(t *testing.T) {
	e := newEnv(t, true)
	e.grant(100)
	e.chat.set("  \n ``` ```  ", nil)
	r := e.optimize(e.owner, "opt-empty-1", "三打白骨精")
	if r.status != http.StatusBadGateway {
		t.Fatalf("空输出应 502：%d %v", r.status, r.body)
	}
	id := e.count(`select count(*) from agent_prompt_optimize where status = 'failed' and error_code = 'empty_reply'`)
	if id != 1 {
		t.Fatalf("应留一条 failed(empty_reply) 记录：%d", id)
	}
	var recID int64
	if err := e.pool.QueryRow(context.Background(), `select id from agent_prompt_optimize`).Scan(&recID); err != nil {
		t.Fatal(err)
	}
	ref := strconv.FormatInt(recID, 10)
	if e.ledgerCount(ref, "consume") != 1 || e.ledgerCount(ref, "refund") != 1 {
		t.Errorf("应 consume + refund 各一条：consume=%d refund=%d", e.ledgerCount(ref, "consume"), e.ledgerCount(ref, "refund"))
	}
	if got := e.balance(); got != 100 {
		t.Errorf("全额退款后余额应回到 100：%d", got)
	}
}

func TestOptimizePrompt_模型出错全额退款(t *testing.T) {
	e := newEnv(t, true)
	e.grant(100)
	e.chat.set("", errors.New("上游 500"))
	r := e.optimize(e.owner, "opt-err-1", "三打白骨精")
	if r.status != http.StatusBadGateway {
		t.Fatalf("模型出错应 502：%d %v", r.status, r.body)
	}
	if got := e.balance(); got != 100 {
		t.Errorf("全额退款后余额应回到 100：%d", got)
	}
	// 失败的那一键重放：不重跑（重跑会撞「已退款」的账本引用），说清要重新点。
	if r2 := e.optimize(e.owner, "opt-err-1", "三打白骨精"); r2.status != http.StatusConflict {
		t.Errorf("失败的键重放应 409：%d %v", r2.status, r2.body)
	}
}

func TestOptimizePrompt_每次调用新建轮次(t *testing.T) {
	e := newEnv(t, true)
	e.grant(200)
	// 第一次失败、第二次换键成功：⚠️ 轮次按调用计（失败也计），重试不会命中上一次的 failed。
	e.chat.set("", nil)
	if r := e.optimize(e.owner, "opt-round-1", "三打白骨精"); r.status != http.StatusBadGateway {
		t.Fatalf("第一次应失败：%d %v", r.status, r.body)
	}
	e.chat.set("改写一", nil)
	r2 := e.optimize(e.owner, "opt-round-2", "三打白骨精")
	e.chat.set("改写二", nil)
	r3 := e.optimize(e.owner, "opt-round-3", "三打白骨精")
	if r2.status != http.StatusOK || r3.status != http.StatusOK {
		t.Fatalf("换键重试应成功：%d %v / %d %v", r2.status, r2.body, r3.status, r3.body)
	}
	if r2.body["id"] == r3.body["id"] {
		t.Errorf("每次调用应是新的记录与账本引用")
	}
	if n := e.count(`select count(*) from agent_prompt_optimize`); n != 3 {
		t.Errorf("三次调用应三条记录（失败也留着）：%d", n)
	}
	if n := e.count(`select count(distinct ref_id) from credit_transaction where ref_type = 'agent_turn' and type = 'consume'`); n != 3 {
		t.Errorf("三次调用应三个不同的账本引用：%d", n)
	}
	if got := e.balance(); got != 200-72 {
		t.Errorf("两次成功扣 72、失败那次退回：%d", got)
	}
}

func TestOptimizePrompt_只读成员403(t *testing.T) {
	e := newEnv(t, true)
	e.grant(100)
	r := e.optimize(e.viewer, "opt-viewer-1", "三打白骨精")
	if r.status != http.StatusForbidden {
		t.Fatalf("只读成员应 403：%d %v", r.status, r.body)
	}
	if e.chat.calls != 0 || e.count(`select count(*) from agent_prompt_optimize`) != 0 {
		t.Errorf("被拒的请求不应调模型或留记录")
	}
}

func TestOptimizePrompt_余额不足不扣费(t *testing.T) {
	e := newEnv(t, true)
	e.grant(10)
	r := e.optimize(e.owner, "opt-poor-1", "三打白骨精")
	if r.status != http.StatusUnprocessableEntity || r.code() != 42201 {
		t.Fatalf("余额不足应 422/42201：%d %v", r.status, r.body)
	}
	if d, _ := r.body["details"].(map[string]any); d == nil || d["shortfall"].(float64) != 26 {
		t.Errorf("应给出差额 26：%v", r.body)
	}
	if e.chat.calls != 0 || e.count(`select count(*) from credit_transaction where ref_type = 'agent_turn'`) != 0 {
		t.Errorf("余额不足不应调模型、不应有任何流水")
	}
}

func TestOptimizePrompt_参数校验(t *testing.T) {
	e := newEnv(t, true)
	e.grant(100)
	if r := e.optimize(e.owner, "", "三打白骨精"); r.status != http.StatusBadRequest || r.code() != 40002 {
		t.Errorf("缺幂等键应 400/40002：%d %v", r.status, r.body)
	}
	if r := e.optimize(e.owner, "opt-blank", "   "); r.status != http.StatusBadRequest {
		t.Errorf("空正文应 400：%d %v", r.status, r.body)
	}
	if r := e.do("POST", "/agent/prompt-optimize", e.owner, "opt-unknown", map[string]any{"text": "x", "prompt": "y"}); r.status != http.StatusBadRequest {
		t.Errorf("未知字段应 400：%d %v", r.status, r.body)
	}
}

func TestOptimizePrompt_恢复扫描退款(t *testing.T) {
	e := newEnv(t, true)
	e.grant(100)
	ctx := context.Background()
	// 模拟「扣费之后进程崩了」：一条 21 分钟前的 running 记录 + 一条 consume。
	id := e.next()
	if _, err := e.pool.Exec(ctx, `insert into agent_prompt_optimize
		(id, workspace_id, payer_account_id, user_id, model_code, idempotency_key, status, credits_cost, input_text, created_at)
		values ($1,$2,$3,$4,'m-reason','opt-stale','running',36,'x', now() - interval '21 minutes')`,
		id, e.ws, e.payer, e.owner); err != nil {
		t.Fatal(err)
	}
	ref := strconv.FormatInt(id, 10)
	if _, err := e.ledger.Consume(ctx, billingrepo.ConsumeInput{PayerID: e.payer, Amount: 36,
		RefType: "agent_turn", RefID: ref, UserID: e.owner, Reason: "测试"}); err != nil {
		t.Fatal(err)
	}
	// 另一条 running 但没扣成（崩在扣费之前）：判失败、不报错。
	id2 := e.next()
	if _, err := e.pool.Exec(ctx, `insert into agent_prompt_optimize
		(id, workspace_id, payer_account_id, user_id, model_code, idempotency_key, status, credits_cost, input_text, created_at)
		values ($1,$2,$3,$4,'m-reason','opt-stale-2','running',36,'x', now() - interval '21 minutes')`,
		id2, e.ws, e.payer, e.owner); err != nil {
		t.Fatal(err)
	}
	svc := agentapp.NewService(e.repo, e.next, agentapp.ServicePorts{
		Optimize: &agentapp.OptimizePorts{Ledger: wiring.AgentLedger{Repo: e.ledger}},
	}, nil)
	n, err := svc.RecoverStaleOptimizes(ctx)
	if err != nil || n != 2 {
		t.Fatalf("应判失败 2 条：n=%d err=%v", n, err)
	}
	if e.ledgerCount(ref, "refund") != 1 || e.balance() != 100 {
		t.Errorf("扣过费的应退回：refund=%d 余额=%d", e.ledgerCount(ref, "refund"), e.balance())
	}
	// 再扫一次：没有可处理的，不重复退款。
	if n, _ := svc.RecoverStaleOptimizes(ctx); n != 0 || e.ledgerCount(ref, "refund") != 1 {
		t.Errorf("第二次扫描不应重复处理")
	}
}

// 「AI 帮写」角色外观（MKT-T-35、MKT-AC-40）：单价与 general 逐字相等、流水 refType 相同、只换改写指令；
// 失败退款恰好一次；缺省 purpose 发给模型的请求与 general 逐字一致；未知 purpose 40001 且不扣费。
func TestOptimize_角色外观同价同链路(t *testing.T) {
	e := newEnv(t, true)
	e.grant(200)
	e.chat.set("圆脸柚子拟人，绿色叶子发型", nil)
	general := e.optimize(e.owner, "opt-p-general", "柚子吉祥物")
	generalIn := e.chat.last
	explicit := e.do("POST", "/agent/prompt-optimize", e.owner, "opt-p-general2",
		map[string]any{"text": "柚子吉祥物", "purpose": "general"})
	explicitIn := e.chat.last
	char := e.do("POST", "/agent/prompt-optimize", e.owner, "opt-p-char",
		map[string]any{"text": "柚子吉祥物", "purpose": "character_appearance"})
	charIn := e.chat.last
	for _, r := range []resp{general, explicit, char} {
		if r.status != http.StatusOK {
			t.Fatalf("应成功：%d %v", r.status, r.body)
		}
	}
	if general.body["credits"] != char.body["credits"] || general.body["credits"] != explicit.body["credits"] {
		t.Errorf("同价：general=%v char=%v", general.body["credits"], char.body["credits"])
	}
	if generalIn.System != explicitIn.System || generalIn.ModelCode != explicitIn.ModelCode ||
		generalIn.Messages[0].Content != explicitIn.Messages[0].Content {
		t.Errorf("缺省 purpose 与 general 发给模型的请求应逐字一致")
	}
	if charIn.System == generalIn.System || !strings.Contains(charIn.System, "非真人角色") ||
		charIn.Messages[0].Content != "柚子吉祥物" || len(charIn.Tools) != 0 {
		t.Errorf("角色外观应只换改写指令：%+v", charIn)
	}
	id := char.body["id"].(string)
	if e.ledgerCount(id, "consume") != 1 {
		t.Errorf("角色外观应同样记 agent_turn 流水")
	}

	// 失败：全额退款恰好一次。
	e.chat.set("", nil)
	fail := e.do("POST", "/agent/prompt-optimize", e.owner, "opt-p-char-fail",
		map[string]any{"text": "柚子吉祥物", "purpose": "character_appearance"})
	if fail.status != http.StatusBadGateway {
		t.Fatalf("空输出应 502：%d %v", fail.status, fail.body)
	}
	var recID int64
	if err := e.pool.QueryRow(context.Background(), `select id from agent_prompt_optimize
		where idempotency_key = 'opt-p-char-fail'`).Scan(&recID); err != nil {
		t.Fatal(err)
	}
	ref := strconv.FormatInt(recID, 10)
	if e.ledgerCount(ref, "consume") != 1 || e.ledgerCount(ref, "refund") != 1 {
		t.Errorf("失败应 consume + refund 各一条")
	}
	if got := e.balance(); got != 200-3*36 {
		t.Errorf("三次成功扣费、失败那次退回：%d", got)
	}

	// 未知 purpose：40001，不调模型、不留记录。
	calls := e.chat.calls
	bad := e.do("POST", "/agent/prompt-optimize", e.owner, "opt-p-bad",
		map[string]any{"text": "柚子吉祥物", "purpose": "script"})
	if bad.status != http.StatusBadRequest || bad.code() != 40001 {
		t.Fatalf("未知 purpose 应 400/40001：%d %v", bad.status, bad.body)
	}
	if e.chat.calls != calls || e.count(`select count(*) from agent_prompt_optimize where idempotency_key = 'opt-p-bad'`) != 0 {
		t.Errorf("未知 purpose 不应调模型或留记录")
	}
	// 只读成员：带 purpose 也是 403（MKT-AC-41）。
	if r := e.do("POST", "/agent/prompt-optimize", e.viewer, "opt-p-viewer",
		map[string]any{"text": "柚子吉祥物", "purpose": "character_appearance"}); r.status != http.StatusForbidden {
		t.Errorf("只读成员应 403：%d %v", r.status, r.body)
	}
}
