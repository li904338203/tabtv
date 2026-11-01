//go:build integration

// 商品库 HTTP 接口（MKT-T-33）的真库 + httptest 测试。
//
// ⚠️ 走真实的工作区闸（collaboration 的 RequireWorkspace + GuardByMethod）：「只读成员写接口 403」
// 必须由路由层给出，绕开路由直接调 Service 的测试把闸整条删掉照样绿。
// 媒体用真实的 media/app.Service（删除会跑全部引用检查器），另注册一个假检查器模拟「被会话引用」。
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	collabapp "github.com/li904338203/tabtv/backend/internal/collaboration/app"
	collabrepo "github.com/li904338203/tabtv/backend/internal/collaboration/repo"
	collabhttp "github.com/li904338203/tabtv/backend/internal/collaboration/transport"
	marketingapp "github.com/li904338203/tabtv/backend/internal/marketing/app"
	marketingrepo "github.com/li904338203/tabtv/backend/internal/marketing/repo"
	marketinghttp "github.com/li904338203/tabtv/backend/internal/marketing/transport"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
	"github.com/li904338203/tabtv/backend/internal/wiring"
)

type userKey struct{}

// fakeSessionRefs 模拟「被营销会话引用」的检查器（只认 held 里的媒体）。
type fakeSessionRefs struct{ held map[int64]bool }

func (f fakeSessionRefs) Kind() string { return mediaapp.UsageAgentMarketing }
func (f fakeSessionRefs) References(_ context.Context, id int64) ([]int64, error) {
	if f.held[id] {
		return []int64{1}, nil
	}
	return nil, nil
}

type productRefs struct{ r *marketingrepo.Repo }

func (p productRefs) Kind() string { return mediaapp.UsageMarketingProduct }
func (p productRefs) References(ctx context.Context, id int64) ([]int64, error) {
	return p.r.ProductMediaReferences(ctx, id)
}

type env struct {
	t                     *testing.T
	pool                  *pgxpool.Pool
	next                  func() int64
	srv                   *httptest.Server
	media                 *mediaapp.Service
	held                  map[int64]bool
	owner, viewer, outsdr int64
	ws, otherWS           int64
}

func mkUser(t *testing.T, pool *pgxpool.Pool, next func() int64, nickname, phone string) int64 {
	t.Helper()
	uid := next()
	if _, err := pool.Exec(context.Background(),
		`insert into app_user (id, phone, nickname, password_hash) values ($1,$2,$3,'x')`, uid, phone, nickname); err != nil {
		t.Fatal(err)
	}
	return uid
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	next := gen.Next
	ctx := context.Background()
	e := &env{t: t, pool: pool, next: next, held: map[int64]bool{}}
	e.owner = mkUser(t, pool, next, "发起人", "13900000001")
	e.viewer = mkUser(t, pool, next, "只读", "13900000002")
	e.outsdr = mkUser(t, pool, next, "外人", "13900000003")
	cr := collabrepo.New(pool)
	w, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "营销组", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	e.ws = w.ID
	if _, err := pool.Exec(ctx, `insert into workspace_member (id, workspace_id, user_id, role)
		values ($1,$2,$3,'viewer')`, next(), e.ws, e.viewer); err != nil {
		t.Fatal(err)
	}
	w2, err := cr.CreateTeam(ctx, [3]int64{next(), next(), next()}, "别的组", e.outsdr)
	if err != nil {
		t.Fatal(err)
	}
	e.otherWS = w2.ID

	mrepo := marketingrepo.New(pool)
	e.media = mediaapp.New(mediarepo.New(pool), nil, next)
	e.media.AddReferenceChecker(productRefs{mrepo})
	e.media.AddReferenceChecker(fakeSessionRefs{held: e.held})
	lib := wiring.NewMarketingLibrary(wiring.MarketingLibraryInput{Repo: mrepo, Media: e.media, NextID: next,
		Settings: marketingapp.LibrarySettings{MaxProductImages: 10, MaxProducts: 3}})

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
	wsGuard := func(n http.Handler) http.Handler { return collabH.RequireWorkspace(collabhttp.GuardByMethod(n)) }
	h := marketinghttp.NewHandler(wiring.NewMarketingService(wiring.MarketingServiceInput{Repo: mrepo,
		Settings: marketingapp.Settings{MaxProductImages: 10, MaxVariants: 4}}), true)
	h.SetLibrary(lib, func(r *http.Request) int64 { v, _ := userIDs(r.Context()); return v })
	r := chi.NewRouter()
	r.Use(httpx.TraceID)
	h.MountPublic(r)
	h.MountLibrary(r, requireUser, wsGuard)
	e.srv = httptest.NewServer(r)
	t.Cleanup(e.srv.Close)
	return e
}

// mkMedia 往 media_object 插一行（不需要真文件：删除只改状态）。
func (e *env) mkMedia(ws int64, contentType, purpose string) int64 {
	e.t.Helper()
	mid := e.next()
	if _, err := e.pool.Exec(context.Background(), `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes, purpose, created_by_user_id)
		values ($1,$2,'seeaitv',$3,$4,1,$5,$6)`, mid, ws, "t/"+strconv.FormatInt(mid, 10), contentType, purpose, e.owner); err != nil {
		e.t.Fatal(err)
	}
	return mid
}

func (e *env) mediaStatus(mid int64) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `select status from media_object where id = $1`, mid).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

type resp struct {
	status int
	body   map[string]any
}

func (r resp) code() int { c, _ := r.body["code"].(float64); return int(c) }

func (e *env) do(method, path string, user, ws int64, body any) resp {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User", strconv.FormatInt(user, 10))
	req.Header.Set("X-Workspace-Id", strconv.FormatInt(ws, 10))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, body: map[string]any{}}
	_ = json.NewDecoder(res.Body).Decode(&out.body)
	return out
}

func sid(id int64) string { return strconv.FormatInt(id, 10) }

func (e *env) create(imgs ...int64) string {
	e.t.Helper()
	ids := make([]string, len(imgs))
	for i, m := range imgs {
		ids[i] = sid(m)
	}
	r := e.do("POST", "/marketing/products", e.owner, e.ws, map[string]any{"name": "柚子汽水",
		"sellingPoints": "清爽", "imageMediaIds": ids})
	if r.status != http.StatusCreated {
		e.t.Fatalf("新建失败：%d %v", r.status, r.body)
	}
	return r.body["id"].(string)
}

func TestLibraryRoutes_只读成员写入403(t *testing.T) {
	e := newEnv(t)
	img := e.mkMedia(e.ws, "image/png", "upload_reference")
	pid := e.create(img)
	if r := e.do("GET", "/marketing/products", e.viewer, e.ws, nil); r.status != 200 {
		t.Fatalf("只读成员列表应 200：%d %v", r.status, r.body)
	}
	if r := e.do("GET", "/marketing/products/"+pid, e.viewer, e.ws, nil); r.status != 200 {
		t.Fatalf("只读成员详情应 200：%d", r.status)
	}
	body := map[string]any{"name": "x", "imageMediaIds": []string{sid(img)}, "version": 1}
	for _, c := range []struct{ m, p string }{{"POST", "/marketing/products"},
		{"PUT", "/marketing/products/" + pid}, {"DELETE", "/marketing/products/" + pid}} {
		r := e.do(c.m, c.p, e.viewer, e.ws, body)
		if r.status != http.StatusForbidden {
			t.Fatalf("只读成员 %s %s 应 403，得 %d %v", c.m, c.p, r.status, r.body)
		}
		if msg, _ := r.body["message"].(string); msg == "" {
			t.Fatalf("403 要说清缺哪一项权限：%v", r.body)
		}
	}
}

func TestLibraryRoutes_校验与工作区隔离(t *testing.T) {
	e := newEnv(t)
	png := e.mkMedia(e.ws, "image/png", "upload_reference")
	gif := e.mkMedia(e.ws, "image/gif", "upload_reference")
	mp4 := e.mkMedia(e.ws, "video/mp4", "upload_reference")
	theirs := e.mkMedia(e.otherWS, "image/png", "upload_reference")

	// MKT-AC-32：0 张 / 11 张 / 31 码点 / 301 码点 / GIF / 视频 / 他人工作区媒体 —— 40001 并点名字段。
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = sid(png)
	}
	for _, c := range []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"0 张", map[string]any{"name": "x", "imageMediaIds": []string{}}, "imageMediaIds"},
		{"11 张", map[string]any{"name": "x", "imageMediaIds": eleven}, "imageMediaIds"},
		{"名称 31", map[string]any{"name": strings.Repeat("🍊", 31), "imageMediaIds": []string{sid(png)}}, "name"},
		{"卖点 301", map[string]any{"name": "x", "sellingPoints": strings.Repeat("卖", 301), "imageMediaIds": []string{sid(png)}}, "sellingPoints"},
		{"描述 301", map[string]any{"name": "x", "description": strings.Repeat("描", 301), "imageMediaIds": []string{sid(png)}}, "description"},
		{"GIF", map[string]any{"name": "x", "imageMediaIds": []string{sid(png), sid(gif)}}, "imageMediaIds[1]"},
		{"视频", map[string]any{"name": "x", "imageMediaIds": []string{sid(mp4)}}, "imageMediaIds[0]"},
		{"他人媒体", map[string]any{"name": "x", "imageMediaIds": []string{sid(theirs)}}, "imageMediaIds[0]"},
	} {
		r := e.do("POST", "/marketing/products", e.owner, e.ws, c.body)
		if r.status != 400 || r.code() != 40001 {
			t.Fatalf("%s 应 40001，得 %d %v", c.name, r.status, r.body)
		}
		details, _ := r.body["details"].(map[string]any)
		issues, _ := details["issues"].([]any)
		found := false
		for _, i := range issues {
			if m, _ := i.(map[string]any); m["field"] == c.field {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s 应在 details.issues 点名 %s：%v", c.name, c.field, issues)
		}
	}

	// MKT-AC-33：A 的商品在 B 工作区一律 404。
	pid := e.create(png)
	ok := map[string]any{"name": "改", "imageMediaIds": []string{sid(theirs)}, "version": 1}
	for _, c := range []struct{ m, p string }{{"GET", "/marketing/products/" + pid},
		{"PUT", "/marketing/products/" + pid}, {"DELETE", "/marketing/products/" + pid}} {
		if r := e.do(c.m, c.p, e.outsdr, e.otherWS, ok); r.status != 404 {
			t.Fatalf("B 工作区 %s 应 404，得 %d %v", c.m, r.status, r.body)
		}
	}
	if r := e.do("GET", "/marketing/products", e.outsdr, e.otherWS, nil); len(r.body["items"].([]any)) != 0 {
		t.Fatalf("B 的列表不应看到 A 的商品：%v", r.body)
	}

	// 乐观锁：旧版本 40902 带当前版本。
	upd := map[string]any{"name": "新名", "imageMediaIds": []string{sid(png)}, "version": 1}
	if r := e.do("PUT", "/marketing/products/"+pid, e.owner, e.ws, upd); r.status != 200 || r.body["version"].(float64) != 2 {
		t.Fatalf("更新应 200 且 version=2：%d %v", r.status, r.body)
	}
	r := e.do("PUT", "/marketing/products/"+pid, e.owner, e.ws, upd)
	if r.code() != 40902 || r.body["details"].(map[string]any)["currentVersion"].(float64) != 2 {
		t.Fatalf("旧版本应 40902 + currentVersion：%v", r.body)
	}

	// 配额（本环境 3）：满额 409 + reason=quota。
	e.create(png)
	e.create(png)
	r = e.do("POST", "/marketing/products", e.owner, e.ws, map[string]any{"name": "第四个", "imageMediaIds": []string{sid(png)}})
	if r.status != 409 || r.body["details"].(map[string]any)["reason"] != "quota" {
		t.Fatalf("满额应 409 reason=quota：%d %v", r.status, r.body)
	}
}

// MKT-AC-34 / 46（商品部分）：删除后列表消失；未被引用的图软删，被（模拟的）会话引用的图保留；
// 商品未删除时其图片的媒体删除被拒、文案「正被营销商品引用」，软删后不再拦。
func TestProduct_软删并清理媒体(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	free := e.mkMedia(e.ws, "image/png", "upload_reference")
	held := e.mkMedia(e.ws, "image/jpeg", "upload_reference")
	gen := e.mkMedia(e.ws, "image/webp", "generation_output")
	e.held[held] = true
	pid := e.create(free, held, gen)

	err := e.media.Delete(ctx, e.ws, free)
	if err == nil || !strings.Contains(err.Error(), "正被 营销商品 引用") {
		t.Fatalf("商品未删除时其图片应删不掉且点名「营销商品」：%v", err)
	}

	if r := e.do("DELETE", "/marketing/products/"+pid, e.owner, e.ws, nil); r.status != http.StatusNoContent {
		t.Fatalf("删除应 204：%d %v", r.status, r.body)
	}
	if r := e.do("GET", "/marketing/products", e.owner, e.ws, nil); len(r.body["items"].([]any)) != 0 {
		t.Fatalf("删除后列表应消失：%v", r.body)
	}
	if s := e.mediaStatus(free); s != "deleted" {
		t.Fatalf("未被引用的商品图应随之软删：%s", s)
	}
	if s := e.mediaStatus(held); s != "active" {
		t.Fatalf("被会话引用的图应保留：%s", s)
	}
	if s := e.mediaStatus(gen); s != "active" {
		t.Fatalf("不是自有上传的图（生成产物）不应被碰：%s", s)
	}
	if r := e.do("DELETE", "/marketing/products/"+pid, e.owner, e.ws, nil); r.status != 404 {
		t.Fatalf("重复删除应 404：%d", r.status)
	}
	// 软删后商品检查器不再拦：解除会话引用后可以正常删除。
	delete(e.held, held)
	if err := e.media.Delete(ctx, e.ws, held); err != nil {
		t.Fatalf("商品软删后其图片不应再被商品检查器拦：%v", err)
	}
}
