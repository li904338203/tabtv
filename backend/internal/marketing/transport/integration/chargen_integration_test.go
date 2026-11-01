//go:build integration

// 生成角色（MKT-T-35）的真库 + httptest 测试。
//
// ⚠️ 走真实装配：wiring.BuildCore 的画布 / 生成服务（真实输入解析、真实受理与账本）、真实目录报价、
// 真实工作区闸（只装上下文，dryRun / 真生成的权限在处理器里判）。只受理、不跑 Worker；
// 失败退款用生成域自己的 Executor + 假 Provider（不触发任何上游调用）。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	catalogtransport "github.com/li904338203/tabtv/backend/internal/catalog/transport"
	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	collabhttp "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	contentrepo "github.com/li904338203/tabtv/backend/internal/content/repo"
	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	marketinghttp "github.com/li904338203/tabtv/backend/internal/marketing/transport"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/audit"
	"github.com/li904338203/tabtv/backend/internal/platform/bootstrap"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type genEnv struct {
	*env
	payer   int64
	catalog *catalogapp.Service
	gen     *genapp.Service
	genRepo *genrepo.Repo
	ledger  *billingrepo.Repo
	canvas  *canvasapp.Service
}

func newGenEnv(t *testing.T) *genEnv {
	t.Helper()
	pool := testsupport.Fresh(t)
	g, _ := id.New(0)
	next := g.Next
	ctx := context.Background()
	e := &genEnv{env: &env{t: t, pool: pool, next: next, held: map[int64]bool{}}}
	e.owner = mkUser(t, pool, next, "发起人", "13900000011")
	e.viewer = mkUser(t, pool, next, "只读", "13900000012")
	e.outsdr = mkUser(t, pool, next, "外人", "13900000013")
	cr := collabrepo.New(pool)
	w, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "营销组", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	e.ws, e.payer = w.ID, w.PayerAccountID
	if _, err := pool.Exec(ctx, `insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'viewer')`, next(), e.ws, e.viewer); err != nil {
		t.Fatal(err)
	}
	w2, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "别的组", e.outsdr)
	if err != nil {
		t.Fatal(err)
	}
	e.otherWS = w2.ID

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.media = mediaapp.New(mediarepo.New(pool), nil, next)
	e.catalog = catalogapp.New(catalogrepo.New(pool), next)
	core, err := wiring.BuildCore(wiring.CoreInput{Cfg: config.Config{},
		Deps: &bootstrap.Deps{DB: &db.Pool{Pool: pool}}, NextID: next, Logger: logger,
		Audit: audit.New(pool, next, logger), Media: e.media, Catalog: e.catalog})
	if err != nil {
		t.Fatal(err)
	}
	e.gen, e.genRepo, e.ledger, e.canvas = core.Gen, core.GenRepo, billingrepo.New(pool, next), core.Canvas

	userIDs := func(ctx context.Context) (int64, bool) {
		v, ok := ctx.Value(userKey{}).(int64)
		return v, ok
	}
	collabH := collabhttp.NewHandler(collabapp.New(cr, next), userIDs)
	requireUser := func(n http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, err := strconv.ParseInt(r.Header.Get("X-Test-User"), 10, 64)
			if err != nil {
				httpx.Fail(w, r, httpx.Err(errcode.Unauthenticated))
				return
			}
			n.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, uid)))
		})
	}
	mrepo := marketingrepo.New(pool)
	h := marketinghttp.NewHandler(wiring.NewMarketingService(wiring.MarketingServiceInput{Repo: mrepo,
		Settings: marketingapp.Settings{MaxProductImages: 10, MaxVariants: 4}}), true)
	h.SetLibrary(wiring.NewMarketingLibrary(wiring.MarketingLibraryInput{Repo: mrepo, Media: e.media, NextID: next,
		Settings: marketingapp.LibrarySettings{MaxProductImages: 10, MaxProducts: 3, MaxCharacters: 3}}),
		func(r *http.Request) int64 { v, _ := userIDs(r.Context()); return v })
	h.SetCharacterGen(wiring.NewMarketingCharacterGen(e.gen, e.catalog, wiring.MarketingWorkspaceMedia{Media: e.media}),
		func(*http.Request) int64 { return 0 })
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	h.MountPublic(r)
	h.MountLibrary(r, requireUser, func(n http.Handler) http.Handler {
		return collabH.RequireWorkspace(collabhttp.GuardByMethod(n))
	})
	h.MountCharacterGen(r, requireUser, collabH.RequireWorkspace)
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *genEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

// seedImageModel 配一个图片模型：text2img（收 text，0~4）与 img2img（收 image，0~maxRefs），per_image 计价。
func (e *genEnv) seedImageModel(model string, credits, maxRefs int) {
	e.t.Helper()
	e.exec(`insert into api_protocol (code, name, is_async) values ('openai_images','OpenAI 图片',false)
		on conflict (code) do nothing`)
	pid := e.next()
	e.exec(`insert into provider_config (id, code, name, adapter_type, base_url, auth_type, status)
		values ($1,$2,$2,'http','http://127.0.0.1:1/v1','none','enabled')`, pid, "p-"+model)
	mid := e.next()
	e.exec(`insert into model (id, code, name, provider_id, domain, upstream_model_code, api_protocol,
		submit_path, est_seconds, status, sort) values ($1,$2,$2,$3,'image','up','openai_images','/x',20,'enabled',0)`,
		mid, model, pid)
	for _, op := range []struct {
		code, in, role string
		max            int
	}{{"text2img", "text,style", "text", 4}, {"img2img", "image,style", "image", maxRefs}} {
		e.exec(`insert into operation (id, code, name, domain, in_node_types, out_node_type, status)
			values ($1,$2,$2,'image',$3,'image','enabled') on conflict (code) do nothing`, e.next(), op.code, op.in)
		e.exec(`insert into model_operation (id, model_id, operation_id, is_enabled)
			select $1, $2, o.id, true from operation o where o.code = $3`, e.next(), mid, op.code)
		e.exec(`insert into model_input_capability (id, model_id, operation_id, input_role, min_count, max_count)
			select $1, $2, o.id, $3, 0, $4 from operation o where o.code = $5`, e.next(), mid, op.role, op.max, op.code)
		e.exec(`insert into pricing (id, operation_id, model_id, credits, unit, status)
			select $1, o.id, $2, $3, 'per_image', 'enabled' from operation o where o.code = $4`,
			e.next(), mid, credits, op.code)
	}
	e.exec(`insert into style (id, code, name, prompt) values ($1,'ink','水墨','水墨画风') on conflict (code) do nothing`,
		e.next())
}

func (e *genEnv) fund(n int) {
	e.exec(`insert into credit_grant (id, payer_account_id, source, amount, remaining, status)
		values ($1,$2,'admin_adjust',$3,$3,'active')`, e.next(), e.payer, n)
}

func (e *genEnv) balance() int {
	n, err := e.ledger.Balance(context.Background(), e.payer)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *genEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// generate 调生成角色接口；key 为空表示不带 Idempotency-Key。
func (e *genEnv) generate(user int64, key string, body map[string]any) resp {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+"/marketing/characters/generate", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", strconv.FormatInt(user, 10))
	req.Header.Set("X-Workspace-Id", strconv.FormatInt(e.ws, 10))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, body: map[string]any{}}
	_ = json.NewDecoder(res.Body).Decode(&out.body)
	return out
}

// estimate 走真实的 /generations/estimate handler（与用户端同一个函数），返回原始响应。
func (e *genEnv) estimate(body string) map[string]any {
	e.t.Helper()
	h := catalogtransport.NewHandler(e.catalog, func(*http.Request) int64 { return 0 })
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	h.MountPricing(r, pass, pass, func(string) func(http.Handler) http.Handler { return pass })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/generations/estimate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		e.t.Fatalf("estimate %s → %d %s", body, rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

func genBody(refs []int64, count int, style string, dry bool) map[string]any {
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = sid(r)
	}
	b := map[string]any{"modelCode": "img-m", "description": "圆脸柚子拟人，绿色叶子发型", "count": count,
		"referenceMediaIds": ids, "dryRun": dry}
	if style != "" {
		b["styleCode"] = style
	}
	return b
}

// ---------------------------------------------------------------- 用例

// dryRun 与同参 /generations/estimate 逐字相等：有 / 无参考图、count 1 与 4、带画风（MKT-AC-39）。
func TestGenerateCharacter_dryRun与estimate相等(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	img1, img2 := e.mkMedia(e.ws, "image/png", "upload_reference"), e.mkMedia(e.ws, "image/jpeg", "upload_reference")
	cases := []struct {
		name  string
		refs  []int64
		count int
		style string
		op    string
	}{
		{"无参考图 1 张", nil, 1, "", "text2img"},
		{"无参考图 4 张带画风", nil, 4, "ink", "text2img"},
		{"1 张参考图 1 张", []int64{img1}, 1, "", "img2img"},
		{"2 张参考图 4 张带画风", []int64{img1, img2}, 4, "ink", "img2img"},
	}
	for _, c := range cases {
		r := e.generate(e.owner, "", genBody(c.refs, c.count, c.style, true))
		if r.status != http.StatusOK || r.body["blocked"] != false || r.body["operation"] != c.op {
			t.Fatalf("%s：dryRun 失败 %d %v", c.name, r.status, r.body)
		}
		est := e.estimate(`{"modelCode":"img-m","operation":"` + c.op + `","count":` + strconv.Itoa(c.count) +
			`,"text":"圆脸柚子拟人，绿色叶子发型","inputs":{"image":` + strconv.Itoa(len(c.refs)) + `}}`)
		if r.body["credits"] != est["credits"] || r.body["unit"] != est["unit"] {
			t.Errorf("%s：dryRun %v/%v 与 estimate %v/%v 不一致", c.name, r.body["credits"], r.body["unit"],
				est["credits"], est["unit"])
		}
		if want := float64(7 * c.count); r.body["credits"] != want {
			t.Errorf("%s：应 per_image × count = %v，得到 %v", c.name, want, r.body["credits"])
		}
	}
	// 参考图超过模型能收的张数：照报价的原因回 blocked，用户能自己解决。
	img3 := e.mkMedia(e.ws, "image/webp", "upload_reference")
	r := e.generate(e.owner, "", genBody([]int64{img1, img2, img3}, 1, "", true))
	if r.status != http.StatusOK || r.body["blocked"] != true || r.body["userFixable"] != true ||
		r.body["blockedReason"] == "" || r.body["credits"].(float64) != 0 {
		t.Fatalf("超过能力矩阵应 blocked + userFixable：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Fatalf("dryRun 不应写任务：%d", n)
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("dryRun 不应有流水：%d", n)
	}
}

// 真生成：project_id = 0、扣费 == 报价、提示词进 params.prompt、来源冻结、任务中心 / 运维台 / 详情 / 产物都能取到。
func TestGenerateCharacter_无项目受理(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	e.fund(100)
	img := e.mkMedia(e.ws, "image/png", "upload_reference")
	dry := e.generate(e.owner, "", genBody([]int64{img}, 2, "ink", true))
	r := e.generate(e.owner, "char-ok-1", genBody([]int64{img}, 2, "ink", false))
	if r.status != http.StatusCreated || r.body["operation"] != "img2img" || r.body["replayed"] != false {
		t.Fatalf("真生成失败：%d %v", r.status, r.body)
	}
	if r.body["credits"] != dry.body["credits"] || e.balance() != 100-14 {
		t.Fatalf("扣费应 == 报价 14：%v 余额 %d", r.body["credits"], e.balance())
	}
	gid, _ := strconv.ParseInt(r.body["generationId"].(string), 10, 64)
	var projectID int64
	var nodeID *string
	var key string
	var params map[string]any
	var raw []byte
	if err := e.pool.QueryRow(context.Background(), `select project_id, node_id, idempotency_key, params_snapshot
		from generation where id = $1`, gid).Scan(&projectID, &nodeID, &key, &raw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &params)
	snap, _ := params["_snapshot"].(map[string]any)
	if projectID != 0 || (nodeID != nil && *nodeID != "") || key != "mkchar:char-ok-1" {
		t.Errorf("应无项目、无节点、键带前缀：project=%d node=%v key=%s", projectID, nodeID, key)
	}
	if params["prompt"] != "圆脸柚子拟人，绿色叶子发型" || params["count"] != "2" {
		t.Errorf("描述要进 params.prompt、张数进 params.count（CD-81）：%v", params)
	}
	if snap["origin"] != gendomain.OriginMarketingCharacter || !strings.Contains(snap["promptSuffix"].(string), "水墨画风") {
		t.Errorf("快照应带来源与画风词：%v", snap)
	}
	if n := e.count(`select count(*) from project`); n != 0 {
		t.Errorf("不应建项目：%d", n)
	}
	// 同键重放：200、同一个任务、不再扣费。
	r2 := e.generate(e.owner, "char-ok-1", genBody([]int64{img}, 2, "ink", false))
	if r2.status != http.StatusOK || r2.body["replayed"] != true || r2.body["generationId"] != r.body["generationId"] {
		t.Fatalf("同键重放：%d %v", r2.status, r2.body)
	}
	if e.count(`select count(*) from credit_transaction where type='consume'`) != 1 {
		t.Error("重放不应再扣费")
	}

	// 详情（GET /generations/{id} 的服务方法）按工作区取得到；他人工作区取不到。
	ctx := context.Background()
	if g, err := e.gen.Get(ctx, e.ws, gid); err != nil || g.ProjectID != 0 {
		t.Fatalf("详情应可取：%v %v", g, err)
	}
	if _, err := e.gen.Get(ctx, e.otherWS, gid); err == nil {
		t.Fatal("他人工作区不应取到")
	}
	// 任务中心与运维台（同一个 union，无 project join）能看到它。
	cr := contentrepo.New(e.pool, e.next)
	tasks, err := cr.ListTasks(ctx, e.ws, contentrepo.TaskFilter{Limit: 10})
	if err != nil || len(tasks) != 1 || tasks[0].ID != gid {
		t.Fatalf("任务中心应列出无项目任务：%v %v", tasks, err)
	}
	ops, err := cr.ListOpsTasks(ctx, contentrepo.OpsTaskFilter{Keyword: sid(gid), Limit: 10}, time.Now())
	if err != nil || len(ops) != 1 {
		t.Fatalf("运维台应查得到：%v %v", ops, err)
	}

	// 模拟成功出图：产物可取、进「我的创作」（left join project），但**不登记待审作品**（MKR-17）。
	mid := e.mkMedia(e.ws, "image/png", "generation_output")
	e.exec(`update generation set status = 'success', finished_at = now() where id = $1`, gid)
	e.exec(`insert into generation_output (id, generation_id, idx, media_object_id, output_type)
		values ($1,$2,0,$3,'image')`, e.next(), gid, mid)
	if o, err := e.gen.Output(ctx, e.ws, gid, 0); err != nil || o.MediaObjectID != mid {
		t.Fatalf("产物应可取：%v %v", o, err)
	}
	cs, err := cr.ListCreations(ctx, contentrepo.CreationFilter{WorkspaceID: e.ws, Limit: 10})
	if err != nil || len(cs) != 1 || cs[0].ProjectID != 0 || cs[0].ProjectName != "" {
		t.Fatalf("我的创作应列出无项目产物：%v %v", cs, err)
	}
	if ok, err := e.genRepo.RegisterWork(ctx, e.next(), gid); err != nil || ok {
		t.Fatalf("生成角色的候选图不应登记作品：%v %v", ok, err)
	}
	if n := e.count(`select count(*) from work`); n != 0 {
		t.Fatalf("work 表应为空：%d", n)
	}
}

// 余额不足：不受理、无扣费流水、不留任务（MKT-AC-39）。
func TestGenerateCharacter_余额不足不扣费(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	e.fund(10)
	r := e.generate(e.owner, "char-poor-1", genBody(nil, 2, "", false))
	if r.status != http.StatusUnprocessableEntity || r.code() != int(errcode.InsufficientCredits) {
		t.Fatalf("应 422 余额不足：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Errorf("不应有任何流水：%d", n)
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Errorf("不应留任务：%d", n)
	}
	if e.balance() != 10 {
		t.Errorf("余额不应变：%d", e.balance())
	}
}

// recordingProvider 记下发给上游的 Job（断言提示词真的在 params 里），按设定的结局返回。
type recordingProvider struct {
	mu   sync.Mutex
	jobs []genapp.Job
}

func (p *recordingProvider) Submit(_ context.Context, j genapp.Job) (genapp.SubmitResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jobs = append(p.jobs, j)
	return genapp.SubmitResult{Outcome: gendomain.SubmitRejected, HTTPStatus: 400, Message: "上游拒绝"}, nil
}

func (p *recordingProvider) Query(context.Context, genapp.Job, string) (genapp.QueryResult, error) {
	return genapp.QueryResult{}, nil
}

func (p *recordingProvider) CanQueryByIdempotencyKey() bool { return false }

type ledgerRefunder struct{ r *billingrepo.Repo }

func (a ledgerRefunder) Refund(ctx context.Context, payerID int64, refType, refID, reason string) error {
	_, err := a.r.Refund(ctx, billingrepo.RefundInput{PayerID: payerID, RefType: refType, RefID: refID,
		Reason: reason, CompensationValidFor: 30 * 24 * time.Hour})
	return err
}

// 上游失败（假 Provider 明确拒绝）：退款恰好一次；发出去的请求里提示词就是形象描述（MKT-AC-39、CD-81）。
func TestGenerateCharacter_失败退款一次(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	e.fund(100)
	r := e.generate(e.owner, "char-fail-1", genBody(nil, 1, "", false))
	if r.status != http.StatusCreated {
		t.Fatalf("受理失败：%d %v", r.status, r.body)
	}
	gid, _ := strconv.ParseInt(r.body["generationId"].(string), 10, 64)
	p := &recordingProvider{}
	ex := genapp.NewExecutor(e.genRepo, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ex.SetRefunder(ledgerRefunder{e.ledger})
	ctx := context.Background()
	for i := 0; i < 2; i++ { // 第二次是重复消息
		if _, err := ex.Execute(ctx, gid); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.jobs) != 1 {
		t.Fatalf("上游只应被提交一次：%d", len(p.jobs))
	}
	var sent map[string]any
	_ = json.Unmarshal(p.jobs[0].Params, &sent)
	if sent["prompt"] != "圆脸柚子拟人，绿色叶子发型" {
		t.Errorf("发给上游的 params.prompt 应是形象描述：%v", sent["prompt"])
	}
	ref := sid(gid)
	if c := e.count(`select count(*) from credit_transaction where ref_type='generation' and ref_id=$1 and type='consume'`, ref); c != 1 {
		t.Errorf("应 1 条 consume：%d", c)
	}
	if c := e.count(`select count(*) from credit_transaction where ref_type='generation' and ref_id=$1 and type='refund'`, ref); c != 1 {
		t.Errorf("应恰好 1 条 refund：%d", c)
	}
	if e.balance() != 100 {
		t.Errorf("退款后余额应回到 100：%d", e.balance())
	}
	if n := e.count(`select count(*) from work`); n != 0 {
		t.Errorf("失败任务不登记作品：%d", n)
	}
}

// 参考图用他人工作区 / 平台素材 / 不支持的格式：拒绝、点名第几张、不扣费；画风不存在 40001 点名 styleCode。
func TestGenerateCharacter_越权参考图(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	e.fund(100)
	other := e.mkMedia(e.otherWS, "image/png", "upload_reference")
	platform := e.mkMedia(0, "image/png", "material_cover")
	gif := e.mkMedia(e.ws, "image/gif", "upload_reference")
	for _, c := range []struct {
		name string
		body map[string]any
		dry  bool
	}{
		{"他人工作区", genBody([]int64{other}, 1, "", false), false},
		{"他人工作区 dryRun", genBody([]int64{other}, 1, "", true), true},
		{"平台素材", genBody([]int64{platform}, 1, "", false), false},
	} {
		r := e.generate(e.owner, "char-x-"+strconv.Itoa(len(c.name)), c.body)
		if r.status != http.StatusBadRequest || r.code() != int(errcode.InvalidArgument) ||
			!strings.Contains(r.body["message"].(string), "第 1 张参考图不存在或不属于当前工作区") {
			t.Errorf("%s：应 40001 点名第 1 张：%d %v", c.name, r.status, r.body)
		}
	}
	r := e.generate(e.owner, "char-gif", genBody([]int64{gif}, 1, "", false))
	if r.status != http.StatusBadRequest || !strings.Contains(r.body["message"].(string), "格式不支持") {
		t.Errorf("GIF 参考图应拒：%d %v", r.status, r.body)
	}
	r = e.generate(e.owner, "char-style", genBody(nil, 1, "no-such", false))
	det, _ := r.body["details"].(map[string]any)
	issues, _ := det["issues"].([]any)
	if r.status != http.StatusBadRequest || len(issues) != 1 ||
		issues[0].(map[string]any)["field"] != "styleCode" {
		t.Errorf("画风不存在应 40001 点名 styleCode：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Errorf("被拒的请求不应扣费：%d", n)
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Errorf("被拒的请求不应留任务：%d", n)
	}
}

// 只读成员：dryRun 200、真生成 403 且说清缺生成权限（MKT-AC-41）；真生成缺幂等键 40002。
func TestGenerateRoute_只读dryRun可真生成403(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	e.fund(100)
	if r := e.generate(e.viewer, "", genBody(nil, 1, "", true)); r.status != http.StatusOK || r.body["credits"] != float64(7) {
		t.Fatalf("只读成员 dryRun 应 200：%d %v", r.status, r.body)
	}
	r := e.generate(e.viewer, "char-ro-1", genBody(nil, 1, "", false))
	if r.status != http.StatusForbidden || r.code() != int(errcode.InsufficientRole) ||
		!strings.Contains(r.body["message"].(string), "生成") {
		t.Fatalf("只读成员真生成应 403 并说清缺生成权限：%d %v", r.status, r.body)
	}
	if r := e.generate(e.outsdr, "", genBody(nil, 1, "", true)); r.status != http.StatusForbidden {
		t.Fatalf("非成员连 dryRun 也不行：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Errorf("被拒的请求不应留任务：%d", n)
	}
}

func TestGenerateRoute_缺幂等键(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	e.fund(100)
	r := e.generate(e.owner, "", genBody(nil, 1, "", false))
	if r.status != http.StatusBadRequest || r.code() != int(errcode.MissingIdempotencyKey) {
		t.Fatalf("真生成缺幂等键应 400/40002：%d %v", r.status, r.body)
	}
	if n := e.count(`select count(*) from generation`); n != 0 {
		t.Errorf("不应留任务：%d", n)
	}
}

// 「无项目」只对生成角色这一个来源开放（dev/05 R-4）：其余调用方漏传项目照旧 404；
// 无项目任务不能引用生成产出（「同项目」防线在 project_id = 0 时不成立）。
func TestNoProject_只对生成角色开放(t *testing.T) {
	e := newGenEnv(t)
	e.seedImageModel("img-m", 7, 2)
	e.fund(100)
	ctx := context.Background()
	// 常规来源 ProjectID = 0：照旧 404，不扣费。
	_, err := e.gen.Accept(ctx, genapp.AcceptInput{WorkspaceID: e.ws, PayerAccountID: e.payer, UserID: e.owner,
		OperationCode: "text2img", ModelCode: "img-m", IdempotencyKey: "plain-no-project", Count: 1, Text: "x"})
	var be *httpx.BizError
	if !errors.As(err, &be) || be.Code != errcode.NotFound {
		t.Fatalf("常规来源漏传项目应 404：%v", err)
	}
	if n := e.count(`select count(*) from credit_transaction`); n != 0 {
		t.Fatalf("不应扣费：%d", n)
	}
	// 生成角色来源却带了项目：拒绝（来源与项目自相矛盾）。
	if _, err := e.gen.Preview(ctx, genapp.AcceptInput{WorkspaceID: e.ws, PayerAccountID: e.payer,
		ProjectID: 42, Origin: gendomain.OriginMarketingCharacter, OperationCode: "text2img", ModelCode: "img-m",
		Count: 1, Text: "x"}); err == nil {
		t.Fatal("生成角色来源不应带项目")
	}
	// 无项目解析：引用生成产出一律拒绝；参考图按工作区解析。
	img := e.mkMedia(e.ws, "image/png", "upload_reference")
	out, err := e.canvas.ResolveForAcceptNoProject(ctx, e.ws, "img2img",
		json.RawMessage(`[{"kind":"generation","order":1,"generationId":"123"}]`))
	if err != nil || len(out.Issues) == 0 || !strings.Contains(out.Issues[0], "不属于任何项目") {
		t.Fatalf("无项目任务不能引用生成产出：%v %v", out.Issues, err)
	}
	out, err = e.canvas.ResolveForAcceptNoProject(ctx, e.ws, "img2img",
		json.RawMessage(`[{"kind":"reference","order":1,"referenceId":"`+sid(img)+`"}]`))
	if err != nil || len(out.Issues) != 0 || out.RoleCounts["image"] != 1 {
		t.Fatalf("本工作区参考图应解析成功：%v %v %v", out.Issues, out.RoleCounts, err)
	}
	// 画布入口传 projectId = 0 仍是 404（不借无项目解析的口子）。
	if _, err := e.canvas.ResolveForAccept(ctx, e.ws, 0, "", "img2img",
		json.RawMessage(`[{"kind":"reference","order":1,"referenceId":"`+sid(img)+`"}]`)); err == nil {
		t.Fatal("常规解析 projectId=0 应 404")
	}
}
