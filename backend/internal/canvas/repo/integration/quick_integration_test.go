//go:build integration

// 创造页「直接生成」的真库测试（AGT-42）。
//
// ⚠️ 必须用真库：要守的是 `uk_project_create_idem` 这条部分唯一索引（迁移 00039）
// 与节点结果写回的版本 CAS —— 两件事都由 SQL 决定。受理端口用桩：
// 这里验的是「项目建不建、建几个、节点上写了什么」，受理本身在生成域有自己的测试。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type stubAccepter struct {
	quoteErr  error
	credits   int
	balance   int
	acceptErr error
	accepts   atomic.Int32
	// 同一个受理幂等键只产生一个 generation（模拟生成域的 uk_generation_idem）。
	mu   sync.Mutex
	byID map[string]int64
}

func (s *stubAccepter) Quote(context.Context, canvasapp.QuickQuoteAsk) (canvasapp.QuickQuote, error) {
	if s.quoteErr != nil {
		return canvasapp.QuickQuote{}, s.quoteErr
	}
	return canvasapp.QuickQuote{Credits: s.credits}, nil
}

func (s *stubAccepter) Balance(context.Context, int64) (int, error) { return s.balance, nil }

func (s *stubAccepter) Accept(_ context.Context, ask canvasapp.QuickAcceptAsk) (canvasapp.QuickAccepted, error) {
	s.accepts.Add(1)
	if s.acceptErr != nil {
		return canvasapp.QuickAccepted{}, s.acceptErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[string]int64{}
	}
	id, ok := s.byID[ask.IdempotencyKey]
	if !ok {
		id = int64(770000 + len(s.byID))
		s.byID[ask.IdempotencyKey] = id
	}
	return canvasapp.QuickAccepted{GenerationID: id, Status: "queued", Credits: s.credits}, nil
}

func quickSetup(t *testing.T, acc *stubAccepter) (*canvasapp.Service, *repo.Repo, *pgxpool.Pool) {
	t.Helper()
	r, pool, next := setup(t)
	svc := canvasapp.New(r, next)
	svc.SetQuickAccepter(acc)
	return svc, r, pool
}

func quickIn(key string) canvasapp.QuickInput {
	return canvasapp.QuickInput{
		WorkspaceID: ws, PayerAccountID: 1, UserID: 1, Role: collab.RoleOwner,
		IdempotencyKey: key, Mode: "image", ModelCode: "img_model",
		Text:   "三打白骨精，悟空视角，悲壮一点",
		Params: json.RawMessage(`{"aspect_ratio":"16:9","count":"1"}`),
	}
}

func countProjects(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from project where workspace_id = $1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func nodeOf(t *testing.T, r *repo.Repo, projectID int64) (map[string]any, int) {
	t.Helper()
	p, err := r.GetWithCanvas(context.Background(), ws, projectID)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int              `json:"schemaVersion"`
		Nodes         []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal(p.CanvasData, &doc); err != nil || len(doc.Nodes) != 1 {
		t.Fatalf("画布应恰好一个节点：%s", p.CanvasData)
	}
	if doc.SchemaVersion != 1 {
		t.Fatalf("schemaVersion 应为 1：%s", p.CanvasData)
	}
	return doc.Nodes[0], p.Version
}

func TestQuickGenerate_同键重放只建一个项目(t *testing.T) {
	acc := &stubAccepter{credits: 4, balance: 100}
	svc, r, pool := quickSetup(t, acc)
	ctx := context.Background()

	first, err := svc.QuickGenerate(ctx, quickIn("key-replay-1"))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Accepted || first.GenerationID == 0 || first.Replayed {
		t.Fatalf("首次应受理成功：%+v", first)
	}
	for i := 0; i < 2; i++ {
		again, err := svc.QuickGenerate(ctx, quickIn("key-replay-1"))
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed || again.ProjectID != first.ProjectID || again.NodeID != first.NodeID ||
			again.GenerationID != first.GenerationID || !again.Accepted {
			t.Fatalf("重放应返回同一个项目/节点/任务：%+v vs %+v", again, first)
		}
	}
	if n := countProjects(t, pool); n != 1 {
		t.Fatalf("同键三次应只有一个项目，得到 %d", n)
	}
	if n := acc.accepts.Load(); n != 1 {
		t.Fatalf("重放不应再次受理，Accept 被调 %d 次", n)
	}

	node, version := nodeOf(t, r, first.ProjectID)
	if node["type"] != "image" || node["title"] != "图片节点 1" {
		t.Fatalf("节点类型/标题不对：%v", node)
	}
	params, _ := node["params"].(map[string]any)
	if params["model"] != "img_model" || params["prompt"] != "三打白骨精，悟空视角，悲壮一点" || params["aspect_ratio"] != "16:9" {
		t.Fatalf("节点参数不对：%v", params)
	}
	result, _ := node["result"].(map[string]any)
	if result["status"] != "queued" || result["id"] == "" {
		t.Fatalf("节点应处于排队中并带任务 ID：%v", result)
	}
	if version != 2 {
		t.Fatalf("写回结果后版本应为 2，得到 %d", version)
	}
	p, _ := r.Get(ctx, ws, first.ProjectID)
	if p.Name != "三打白骨精，悟空视角，悲壮一点" {
		t.Fatalf("项目名应取正文前 20 字：%q", p.Name)
	}

	// 另一个键是另一次发送：建第二个项目。
	if _, err := svc.QuickGenerate(ctx, quickIn("key-replay-2")); err != nil {
		t.Fatal(err)
	}
	if n := countProjects(t, pool); n != 2 {
		t.Fatalf("不同键应各建一个，得到 %d", n)
	}
}

func TestQuickGenerate_报价失败不建项目(t *testing.T) {
	ctx := context.Background()

	acc := &stubAccepter{quoteErr: httpx.Errf(errcode.ModelUnavailable, "模型当前不可用")}
	svc, _, pool := quickSetup(t, acc)
	_, err := svc.QuickGenerate(ctx, quickIn("key-quote-fail"))
	var biz *httpx.BizError
	if !errors.As(err, &biz) || biz.Code != errcode.ModelUnavailable {
		t.Fatalf("应原样返回报价错误，得到 %v", err)
	}
	if n := countProjects(t, pool); n != 0 {
		t.Fatalf("报价失败不应建项目，得到 %d", n)
	}

	// 余额不足：同样不建项目，且错误说得出还差多少。
	acc2 := &stubAccepter{credits: 60, balance: 15}
	svc2 := canvasapp.New(repo.New(pool), func() int64 { return 1 })
	svc2.SetQuickAccepter(acc2)
	_, err = svc2.QuickGenerate(ctx, quickIn("key-balance"))
	if !errors.As(err, &biz) || biz.Code != errcode.InsufficientCredits || biz.Details["shortfall"] != 45 {
		t.Fatalf("余额不足应报 InsufficientCredits 且带差额，得到 %v", err)
	}
	if n := countProjects(t, pool); n != 0 {
		t.Fatalf("余额不足不应建项目，得到 %d", n)
	}
	if acc.accepts.Load()+acc2.accepts.Load() != 0 {
		t.Fatal("预检失败不应走到受理")
	}

	// 只读成员：直接拒绝。
	in := quickIn("key-viewer")
	in.Role = collab.RoleViewer
	if _, err := svc.QuickGenerate(ctx, in); !errors.As(err, &biz) || biz.Code != errcode.PermissionDenied {
		t.Fatalf("只读成员应 403，得到 %v", err)
	}
	// 缺幂等键
	if _, err := svc.QuickGenerate(ctx, quickIn("")); !errors.As(err, &biz) || biz.Code != errcode.MissingIdempotencyKey {
		t.Fatalf("缺幂等键应拒绝，得到 %v", err)
	}
}

func TestQuickGenerate_受理失败保留项目并标节点失败(t *testing.T) {
	acc := &stubAccepter{credits: 4, balance: 100,
		acceptErr: httpx.Errf(errcode.InvalidArgument, "参考文件不存在")}
	svc, r, pool := quickSetup(t, acc)
	ctx := context.Background()

	res, err := svc.QuickGenerate(ctx, quickIn("key-accept-fail"))
	if err != nil {
		t.Fatalf("受理失败不应整体报错（项目已建）：%v", err)
	}
	if res.Accepted || res.Error != "参考文件不存在" || res.ErrorCode != errcode.InvalidArgument || res.ProjectID == 0 {
		t.Fatalf("应回 accepted=false 与原话：%+v", res)
	}
	if n := countProjects(t, pool); n != 1 {
		t.Fatalf("项目应保留，得到 %d", n)
	}
	node, _ := nodeOf(t, r, res.ProjectID)
	result, _ := node["result"].(map[string]any)
	if result["status"] != "failed" || result["error"] != "参考文件不存在" {
		t.Fatalf("节点应标失败并带原话：%v", result)
	}

	// 重放：返回同一个项目与失败原因，不再受理。
	again, err := svc.QuickGenerate(ctx, quickIn("key-accept-fail"))
	if err != nil || again.ProjectID != res.ProjectID || again.Accepted || again.Error != "参考文件不存在" {
		t.Fatalf("重放应返回已失败的同一项目：%+v %v", again, err)
	}
	if n := acc.accepts.Load(); n != 1 {
		t.Fatalf("失败后的重放不应再受理，Accept 被调 %d 次", n)
	}
}

// 用户已经打开并保存过画布（版本不再是 1）：结果写回放弃，不覆盖他的画布。
func TestMarkNodeResult_版本已变则放弃(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	id := next()
	canvas := json.RawMessage(`{"schemaVersion":1,"nodes":[{"id":"n_1","type":"image","params":{},"x-extra":1}],"edges":[],"keep":true}`)
	if err := r.InsertWithIdemKey(ctx, repo.Project{ID: id, WorkspaceID: ws, CreatedBy: 1, Name: "p", CanvasData: canvas}, "k1"); err != nil {
		t.Fatal(err)
	}
	ok, err := r.MarkNodeResult(ctx, ws, id, "n_1", json.RawMessage(`{"status":"failed","error":"x"}`), 1, 1)
	if err != nil || !ok {
		t.Fatalf("版本 1 时应写入：%v %v", ok, err)
	}
	p, _ := r.GetWithCanvas(ctx, ws, id)
	var doc map[string]any
	_ = json.Unmarshal(p.CanvasData, &doc)
	n0 := doc["nodes"].([]any)[0].(map[string]any)
	if doc["keep"] != true || n0["x-extra"] != float64(1) {
		t.Fatalf("未知字段应原样保留：%s", p.CanvasData)
	}
	ok, err = r.MarkNodeResult(ctx, ws, id, "n_1", json.RawMessage(`{"status":"queued"}`), 1, 1)
	if err != nil || ok {
		t.Fatalf("版本已不是 1 时应放弃：%v %v", ok, err)
	}
	// 同键第二次插入撞部分唯一索引
	if err := r.InsertWithIdemKey(ctx, repo.Project{ID: next(), WorkspaceID: ws, CreatedBy: 1, Name: "p2"}, "k1"); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("同工作区同键应撞唯一索引：%v", err)
	}
	// 另一个工作区同键不撞
	if err := r.InsertWithIdemKey(ctx, repo.Project{ID: next(), WorkspaceID: ws + 1, CreatedBy: 1, Name: "p3"}, "k1"); err != nil {
		t.Fatalf("不同工作区同键应允许：%v", err)
	}
	// 普通建项目（键为空）互不影响
	mustInsert(t, r, next, ws, "a")
	mustInsert(t, r, next, ws, "b")
}

func TestQuickGenerate_并发同键只有一个成功插入(t *testing.T) {
	acc := &stubAccepter{credits: 4, balance: 100}
	svc, _, pool := quickSetup(t, acc)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	results := make([]canvasapp.QuickResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = svc.QuickGenerate(ctx, quickIn("key-concurrent"))
		}(i)
	}
	close(start)
	wg.Wait()

	if c := countProjects(t, pool); c != 1 {
		t.Fatalf("并发同键应只有一个项目，得到 %d", c)
	}
	var pid int64
	gens := map[int64]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			// 极端时序下允许「并发创建冲突，请重试」，但绝不能多建项目。
			var biz *httpx.BizError
			if errors.As(errs[i], &biz) && biz.Code == errcode.Conflict {
				continue
			}
			t.Fatalf("第 %d 个请求出错：%v", i, errs[i])
		}
		if pid == 0 {
			pid = results[i].ProjectID
		}
		if results[i].ProjectID != pid {
			t.Fatalf("所有请求应指向同一个项目：%d vs %d", results[i].ProjectID, pid)
		}
		if results[i].GenerationID != 0 {
			gens[results[i].GenerationID] = true
		}
	}
	if len(gens) > 1 {
		t.Fatalf("同一次发送意图只应对应一个生成任务，得到 %v", gens)
	}
}
