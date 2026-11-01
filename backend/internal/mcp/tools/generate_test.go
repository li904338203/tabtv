package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/li904338203/tabtv/backend/internal/canvasgen"
	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// fakeCanvasGen 是假的画布生成端口：报价 = price（缺省 count×10，与 fakeQuoter 的单项同价）；
// 受理按幂等键去重（同受理侧 uk_generation_idem）。nodeId "n_user" 不归本授权。
type fakeCanvasGen struct {
	mu      sync.Mutex
	price   int // 0 表示按 count×10
	nodes   []string
	batches []string
	accepts map[string]int64
	calls   int
	links   []string
	next    int64
}

func newFakeCanvasGen() *fakeCanvasGen {
	return &fakeCanvasGen{accepts: map[string]int64{}, next: 7000}
}

// fakeOutputs 是目录里操作的产出类型（operation.out_node_type 的假表）。
type fakeOutputs map[string]string

func (o fakeOutputs) OperationOutNodeType(_ context.Context, op string) (string, error) {
	return o[op], nil
}

// NewNodeType 走真的推导（canvasgen.ResolveNewNodeType），只把目录换成假表。
func (f *fakeCanvasGen) NewNodeType(ctx context.Context, op, declared string) (string, error) {
	return canvasgen.ResolveNewNodeType(ctx, fakeOutputs{"text2img": "image", "text2video": "video",
		"storyboard_gen": "script"}, op, declared)
}

func (f *fakeCanvasGen) Prepare(_ context.Context, _ canvasgen.Caller, req canvasgen.Request) (app.GenPrepared, error) {
	if req.NodeID == "n_user" {
		return app.GenPrepared{}, canvasgen.ErrNodeNotOwned
	}
	p := app.GenPrepared{Prepared: canvasgen.Prepared{Request: req, ResolvedNodeType: req.NodeType}}
	if len(req.Inputs) > 0 {
		var arr []any
		_ = json.Unmarshal(req.Inputs, &arr)
		p.RoleCounts = map[string]int{"image": len(arr)}
	}
	return p, nil
}

func (f *fakeCanvasGen) Quote(_ context.Context, _ canvasgen.Caller, p app.GenPrepared) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.price > 0 {
		return f.price, nil
	}
	return p.Count * 10, nil
}

func (f *fakeCanvasGen) EnsureNode(_ context.Context, c canvasgen.Caller, p app.GenPrepared) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, c.BatchID)
	if p.NodeID != "" {
		return p.NodeID, nil
	}
	id := "a_" + c.BatchID // 同批次重放返回同一节点
	for _, n := range f.nodes {
		if n == id {
			return id, nil
		}
	}
	f.nodes = append(f.nodes, id)
	return id, nil
}

func (f *fakeCanvasGen) Accept(_ context.Context, _ canvasgen.Caller, p app.GenPrepared, _, key string) (
	canvasgen.AcceptResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if id, ok := f.accepts[key]; ok {
		return canvasgen.AcceptResult{GenerationID: id, Credits: p.Count * 10, Status: "queued", Replayed: true}, nil
	}
	f.next++
	f.accepts[key] = f.next
	return canvasgen.AcceptResult{GenerationID: f.next, Credits: p.Count * 10, Status: "queued"}, nil
}

func (f *fakeCanvasGen) Link(_ context.Context, c canvasgen.Caller, nodeID string, _ int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links = append(f.links, canvasgen.LinkBatchID(c.BatchID)+"→"+nodeID)
}

func (f *fakeCanvasGen) FindAccepted(_ context.Context, _ int64, key string) (app.AcceptedTask, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.accepts[key]
	if !ok {
		return app.AcceptedTask{}, false, nil
	}
	return app.AcceptedTask{GenerationID: id, ProjectID: 11, NodeID: "a_" + key, Status: "running", Credits: 20}, true, nil
}

func (f *fakeCanvasGen) acceptedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accepts)
}

type fakeBalance int

func (b fakeBalance) Balance(context.Context, app.Principal) (int, error) { return int(b), nil }

// genDeps 装出 generate 的假端口：余额 balance，quote 与 quote 工具共用一张假表。
func genDeps(balance int) (Deps, *fakeCanvasGen, *fakeQuoteStore) {
	d := fakeDeps()
	store := &fakeQuoteStore{}
	cg := newFakeCanvasGen()
	n := int64(9100)
	c := app.NewConfirm(store, fakeBalance(balance), func() int64 { n++; return n }, 0, app.DeriveStateKey("k"))
	c.SetClock(func() time.Time { return t0 })
	d.CanvasGen, d.Confirm = cg, c
	q := app.NewQuotes(&fakeQuoter{}, store, func() int64 { n++; return n }, 0)
	q.SetClock(func() time.Time { return t0 })
	d.Quotes = q
	return d, cg, store
}

const genArgs = `{"projectId":"11","nodeType":"image","operationCode":"text2img","modelCode":"m","prompt":"一只猫","count":2}`

func withConfirm(args, quoteID string, credits int) string {
	return strings.TrimSuffix(args, "}") + `,"confirmQuoteId":"` + quoteID + `","confirmCredits":` +
		strconv.Itoa(credits) + `}`
}

func callErr(t *testing.T, reg *app.Registry, ctx context.Context, name, args string) error {
	t.Helper()
	tool, _ := reg.Lookup(name)
	c, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s 入参：%v", name, err)
	}
	_, err = tool.Handler(ctx, c)
	return err
}

func TestGenerateSchema_无布尔确认参数(t *testing.T) {
	reg := NewRegistry(fakeDeps())
	tool, _ := reg.Lookup("generate")
	var walk func(path string, s *jsonschema.Schema)
	walk = func(path string, s *jsonschema.Schema) {
		if s == nil {
			return
		}
		if s.Type == "boolean" || containsStr(s.Types, "boolean") {
			t.Errorf("%s 是布尔参数：确认不能是一个开关（MCP-F-45）", path)
		}
		for k, p := range s.Properties {
			walk(path+"."+k, p)
		}
		walk(path+"[]", s.Items)
	}
	walk("generate", tool.InputSchema())
	for _, bad := range []string{
		strings.TrimSuffix(genArgs, "}") + `,"confirm":true}`,
		strings.TrimSuffix(genArgs, "}") + `,"confirmed":true}`,
		strings.TrimSuffix(genArgs, "}") + `,"confirmCredits":true,"confirmQuoteId":"1"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("%s 应被 schema 拒绝", bad)
		}
	}
	m, _ := tool.Method("")
	if !m.Paid || !m.Write {
		t.Fatal("generate 必须声明 Paid + Write（只读端点据此挡掉）")
	}
	if !strings.Contains(tool.Description, "直接调用，不要先在聊天里问用户；平台会请用户确认费用") ||
		!strings.Contains(tool.Description, "宿主超时不代表失败，不要重新提交，用 tasks 查询") {
		t.Fatal("描述要写明确认方式与后台任务语义（MCP-F-51）")
	}
}

func TestGenerate_基线确认后受理且重试只有一个任务(t *testing.T) {
	d, cg, store := genDeps(100)
	reg := NewRegistry(d)
	m, _ := call(t, reg, "generate", genArgs, 6000)
	if m["status"] != "needs_confirmation" || cg.calls != 0 || len(cg.nodes) != 0 {
		t.Fatalf("无确认不受理、不建节点：%v calls=%d nodes=%v", m, cg.calls, cg.nodes)
	}
	nc := m["needsConfirmation"].(map[string]any)
	if nc["credits"].(float64) != 20 || nc["balance"].(float64) != 100 || nc["after"].(float64) != 80 ||
		nc["payerAccountId"] != "3" || nc["quoteId"] == "" {
		t.Fatalf("needsConfirmation：%v", nc)
	}
	if !strings.Contains(m["message"].(string), "confirmCredits=20") {
		t.Errorf("要告诉模型怎么重调：%v", m["message"])
	}
	qid := nc["quoteId"].(string)
	for i := 0; i < 3; i++ {
		m, _ = call(t, reg, "generate", withConfirm(genArgs, qid, 20), 6000)
		if m["generationId"] != "7001" || m["nodeId"] != "a_mcp:5:"+qid {
			t.Fatalf("第 %d 次：%v", i+1, m)
		}
		if (i == 0) == (m["replayed"] == true) {
			t.Fatalf("第 %d 次 replayed=%v", i+1, m["replayed"])
		}
	}
	if cg.acceptedCount() != 1 {
		t.Fatalf("重试 3 次只应有一个任务：%d", cg.acceptedCount())
	}
	if want := "mcp:5:" + qid + ":link→a_mcp:5:" + qid; len(cg.links) == 0 || cg.links[0] != want {
		t.Fatalf("受理后要挂到节点，批次 = 幂等键 + :link：%v", cg.links)
	}
	if q, _ := store.GetQuote(context.Background(), 5, mustID(t, qid)); q.UsedAt == nil {
		t.Fatal("quote 应已用")
	}
	// 改了参数（指纹不符）→ MCP_QUOTE_INVALID。
	err := callErr(t, reg, ctxWithPrincipal(), "generate",
		withConfirm(strings.Replace(genArgs, "一只猫", "一只狗", 1), qid, 20))
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.McpQuoteInvalid || be.Details["reason"] != "fingerprint_mismatch" {
		t.Fatalf("改参数应无效：%v", err)
	}
	// 只给一半确认字段 → 参数错误。
	if err := callErr(t, reg, ctxWithPrincipal(), "generate", strings.TrimSuffix(genArgs, "}")+`,"confirmQuoteId":"`+qid+`"}`); err == nil {
		t.Fatal("confirmQuoteId 与 confirmCredits 必须一起给")
	}
}

func TestGenerate_余额不足不签发quote(t *testing.T) {
	d, cg, store := genDeps(5)
	err := callErr(t, NewRegistry(d), ctxWithPrincipal(), "generate", genArgs)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.InsufficientCredits || be.Details["shortfall"] != 15 {
		t.Fatalf("应直接拒绝并给差额：%v", err)
	}
	if len(store.rows) != 0 || cg.calls != 0 {
		t.Fatal("余额不足不签发、不受理")
	}
}

// 不给 nodeType 时按目录的产出类型推导（会话 105692702233853952 第 15 条：只给了 text2video）；
// 给了而与产出类型不一致、或产出类型不是可新建的节点类型时，参数错误、不报价不建节点。
func TestGenerate_新建节点类型按目录推导(t *testing.T) {
	d, cg, _ := genDeps(100)
	reg := NewRegistry(d)
	m, _ := call(t, reg, "generate", `{"projectId":"11","operationCode":"text2video","modelCode":"m","count":1}`, 6000)
	if m["status"] != "needs_confirmation" {
		t.Fatalf("缺 nodeType 应按 text2video 的产出类型推导成 video：%v", m)
	}
	for _, c := range []struct{ args, want string }{
		{`{"projectId":"11","nodeType":"image","operationCode":"text2video","modelCode":"m"}`, "不一致"},
		{`{"projectId":"11","operationCode":"storyboard_gen","modelCode":"m"}`, "不能直接建节点"},
		{`{"projectId":"11","operationCode":"nope","modelCode":"m"}`, "不在目录里"},
	} {
		err := callErr(t, reg, ctxWithPrincipal(), "generate", c.args)
		var be *httpx.BizError
		if !errors.As(err, &be) || be.Code != errcode.InvalidArgument || !strings.Contains(be.Error(), c.want) {
			t.Fatalf("%s：应参数错误并提到 %q：%v", c.args, c.want, err)
		}
	}
	if cg.calls != 0 || len(cg.nodes) != 0 {
		t.Fatal("参数错误不该建节点或受理")
	}
}

func TestGenerate_改用户节点返回NodeNotOwned(t *testing.T) {
	d, cg, store := genDeps(100)
	err := callErr(t, NewRegistry(d), ctxWithPrincipal(), "generate",
		`{"projectId":"11","nodeId":"n_user","operationCode":"text2img","modelCode":"m","count":1}`)
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.McpNodeNotOwned || !strings.Contains(be.Error(), "新建") {
		t.Fatalf("应返回 MCP_NODE_NOT_OWNED 并提示新建：%v", err)
	}
	if len(store.rows) != 0 || cg.calls != 0 {
		t.Fatal("报价之前就该拦下")
	}
}

func TestGenerate_elicitation拒绝不扣费(t *testing.T) {
	d, cg, store := genDeps(100)
	reg := NewRegistry(d)
	tool, _ := reg.Lookup("generate")
	c, _ := tool.Decode(json.RawMessage(genArgs))
	ctx := app.WithElicitation(ctxWithPrincipal(), app.ElicitContext{Supported: true})
	out, err := tool.Handler(ctx, c)
	if err != nil || out.Elicit == nil || out.Elicit.State == "" {
		t.Fatalf("支持 elicitation 时应给表单：%+v %v", out, err)
	}
	for _, s := range []string{"20 积分", "付费账户", "当前余额 100", "确认后剩余 80"} {
		if !strings.Contains(out.Elicit.Message, s) {
			t.Errorf("表单缺「%s」：%s", s, out.Elicit.Message)
		}
	}
	state := out.Elicit.State
	for _, action := range []string{"decline", "cancel"} {
		ctx := app.WithElicitation(ctxWithPrincipal(), app.ElicitContext{Supported: true, State: state,
			Answer: &app.ElicitAnswer{Action: action}})
		out, err := tool.Handler(ctx, c)
		if err != nil || out.Data.(generateOut).Status != "declined" || !strings.Contains(out.Summary, "未扣费") {
			t.Fatalf("%s：%+v %v", action, out, err)
		}
	}
	if cg.calls != 0 || store.rows[0].UsedAt != nil {
		t.Fatal("拒绝后不受理、quote 保持未用")
	}
	// 接受：走同一个 Verify，幂等键与基线相同。
	ctx = app.WithElicitation(ctxWithPrincipal(), app.ElicitContext{Supported: true, State: state,
		Answer: &app.ElicitAnswer{Action: "accept", Content: map[string]any{"confirm": true}}})
	out, err = tool.Handler(ctx, c)
	if err != nil || out.Data.(generateOut).Status != "queued" || cg.acceptedCount() != 1 {
		t.Fatalf("接受后应受理：%+v %v", out, err)
	}
	if _, ok := cg.accepts[domain.IdemKey(5, store.rows[0].ID)]; !ok {
		t.Fatalf("幂等键应为 mcp:{grant}:{quote}：%v", cg.accepts)
	}
	// 篡改的 state → 无效。
	ctx = app.WithElicitation(ctxWithPrincipal(), app.ElicitContext{Supported: true, State: state + "x",
		Answer: &app.ElicitAnswer{Action: "accept", Content: map[string]any{"confirm": true}}})
	if _, err := tool.Handler(ctx, c); err == nil {
		t.Fatal("篡改的 requestState 应被拒")
	}
}

// quote 工具签发的单项 quoteId 能直接给 generate 用（假端口版；真实报价路径见 integration）。
func TestGenerate_quote工具的quoteId可直接确认(t *testing.T) {
	d, cg, _ := genDeps(100)
	reg := NewRegistry(d)
	// 规范化：modelCode 首尾空白、inputs 键序与 0 值、0 值的角色数都不影响指纹。
	qm, _ := call(t, reg, "quote", `{"item":{"modelCode":" m","operation":"text2img","count":2,"text":"一只猫","inputs":{"image":0}}}`, 6000)
	qid := qm["quoteId"].(string)
	m, _ := call(t, reg, "generate", withConfirm(genArgs, qid, 20), 6000)
	if m["status"] != "queued" || cg.acceptedCount() != 1 {
		t.Fatalf("quote 签发的 quoteId 应可直接确认：%v", m)
	}
}

func mustID(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
