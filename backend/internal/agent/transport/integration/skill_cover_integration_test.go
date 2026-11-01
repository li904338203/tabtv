//go:build integration

// 技能封面公开端点（MKT-T-05、MKD-19）：未登录可取已启用技能的封面，停用 / 不存在 / 没封面
// 一律同一个 404；出字节交给媒体域的 ServePublic（这里用假实现记下收到的 mediaID 与宽度）。
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	agentapp "github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	agenthttp "github.com/li904338203/tabtv/backend/internal/agent/transport"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type fakeMediaServer struct {
	mediaID int64
	width   int
}

func (f *fakeMediaServer) ServePublic(w http.ResponseWriter, _ *http.Request, mediaID int64, width int) {
	f.mediaID, f.width = mediaID, width
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
}

// RedirectPublic 技能封面现在走跳转（2026-11-02）；桩照样记下 ID 与宽度。
func (f *fakeMediaServer) RedirectPublic(w http.ResponseWriter, r *http.Request, mediaID int64, width int) {
	f.ServePublic(w, r, mediaID, width)
}

func TestSkillCover_公开且只出启用技能(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	r := repo.New(pool)
	ctx := context.Background()
	mk := func(code string, cover int64, enabled bool) {
		if _, err := r.UpsertSkill(ctx, repo.Skill{ID: gen.Next(), Code: code, Name: code, Category: "营销视频",
			Body: "x", CoverMediaID: cover, IsEnabled: enabled}); err != nil {
			t.Fatal(err)
		}
	}
	mk("cover_on", 9001, true)
	mk("cover_off", 9002, false)
	mk("cover_none", 0, true)

	media := &fakeMediaServer{}
	h := agenthttp.NewHandler(agentapp.NewService(r, gen.Next, agentapp.ServicePorts{}, nil),
		func(*http.Request) int64 { return 0 }, true)
	h.SetMedia(media)
	// 登录闸与工作区闸一律拒：封面若误挂进那一组，这里就是 401。
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
	rt := chi.NewRouter()
	rt.Use(httpx.TraceID)
	h.Mount(rt, deny, deny)
	srv := httptest.NewServer(rt)
	defer srv.Close()

	get := func(path string) (*http.Response, map[string]any) {
		res, err := http.Get(srv.URL + path) // 不带任何身份
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body := map[string]any{}
		_ = json.NewDecoder(res.Body).Decode(&body)
		return res, body
	}

	res, _ := get("/agent/skills/cover_on/cover?w=640")
	if res.StatusCode != http.StatusOK || media.mediaID != 9001 || media.width != 640 {
		t.Fatalf("已启用技能的封面应公开可取：%d media=%d w=%d", res.StatusCode, media.mediaID, media.width)
	}
	if res.Header.Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("缓存头不对：%q", res.Header.Get("Cache-Control"))
	}
	media.mediaID = 0
	if res, _ := get("/agent/skills/cover_on/cover"); res.StatusCode != http.StatusOK || media.width != 320 {
		t.Fatalf("缺省宽度应为 320：%d w=%d", res.StatusCode, media.width)
	}
	for _, code := range []string{"cover_off", "cover_none", "nope"} {
		media.mediaID = 0
		res, body := get("/agent/skills/" + code + "/cover")
		if res.StatusCode != http.StatusNotFound || media.mediaID != 0 {
			t.Fatalf("%s 应 404 且不出图：%d %v", code, res.StatusCode, body)
		}
	}
	// 用户端列表仍在登录闸之后。
	if res, _ := get("/agent/skills"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/agent/skills 应仍要求登录：%d", res.StatusCode)
	}
}
