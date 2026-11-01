package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/catalog/app"
)

// 管理端的画布配置快照必须有自己的路由，并受 canvas:read 保护。
//
// ⚠️ 起因：管理端此前去调用户端的 /canvas/config —— admin token 在那里是 401，
// 而管理端收到 401 会清会话，于是「一点画布配置就被踢回登录页」。
// 这里用桩中间件记下路由要求的权限，并在它那里截住请求（不走到 handler）。
func TestAdminCanvasConfig_有独立路由且要求画布读权限(t *testing.T) {
	var admin bool
	var perm string
	requireAdmin := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { admin = true; next.ServeHTTP(w, r) })
	}
	requirePerm := func(p string) func(http.Handler) http.Handler {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				perm = p
				w.WriteHeader(http.StatusTeapot)
			})
		}
	}
	r := chi.NewRouter()
	(&Handler{}).MountAdmin(r, requireAdmin, requirePerm)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/canvas/config", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("GET /admin/canvas/config 没有落到权限闸上（状态 %d）—— 路由不存在或没挂权限", rec.Code)
	}
	if !admin {
		t.Fatalf("路由没有经过管理员鉴权")
	}
	if perm != "canvas:read" {
		t.Fatalf("要求的权限是 %q，应为 canvas:read", perm)
	}
}

// 操作目录要有只读路由（受 model:read 保护）：计价与模型绑定表单要能选到
// 还没有任何价格 / 绑定的操作 —— 此前只能从已有的价格里凑操作码，新操作永远配不上价。
func TestAdminOperations_有只读路由且要求模型读权限(t *testing.T) {
	var admin bool
	var perm string
	requireAdmin := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { admin = true; next.ServeHTTP(w, r) })
	}
	requirePerm := func(p string) func(http.Handler) http.Handler {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				perm = p
				w.WriteHeader(http.StatusTeapot)
			})
		}
	}
	r := chi.NewRouter()
	(&Handler{}).MountModelAdmin(r, requireAdmin, requirePerm)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/operations", nil))
	if rec.Code != http.StatusTeapot || !admin || perm != "model:read" {
		t.Fatalf("GET /admin/operations 应经过管理员鉴权并要求 model:read（状态 %d，admin=%v，perm=%q）", rec.Code, admin, perm)
	}
}

// 提示词规格的变量列表必须是数组，不能是 null：没有必需变量的用途此前回 `required: null`，
// 管理端按数组展开时直接抛错，整个编辑表单渲染失败。
func TestPromptSpecs_变量列表不为null(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{svc: app.New(nil, nil)}).promptSpecs(rec, httptest.NewRequest(http.MethodGet, "/admin/prompts/specs", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 %d：%s", rec.Code, body)
	}
	if strings.Contains(body, `"required":null`) || strings.Contains(body, `"optional":null`) {
		t.Fatalf("变量列表出现了 null：%s", body)
	}
	if !strings.Contains(body, `"required":[]`) {
		t.Fatalf("没有必需变量的用途应回空数组：%s", body)
	}
}

// 型号级参数覆盖（P15-01）要有用户端路由，且挂在用户鉴权之后。
//
// ⚠️ 起因：前端一直在调 `/models/{code}/params`，后端却没有这条路由 —— 404 被前端当成
// 「没有覆盖」静默吞掉，画布视频节点因此永远只有通用的时长 / 清晰度 / 画幅。
func TestModelParams_用户端有路由且要求登录(t *testing.T) {
	var user bool
	requireUser := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user = true
			w.WriteHeader(http.StatusTeapot)
		})
	}
	pass := func(next http.Handler) http.Handler { return next }
	r := chi.NewRouter()
	(&Handler{}).MountPricing(r, requireUser, pass, func(string) func(http.Handler) http.Handler { return pass })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/models/seedance_2_5/params", nil))
	if rec.Code != http.StatusTeapot || !user {
		t.Fatalf("GET /models/{code}/params 应落到用户鉴权上（状态 %d）—— 路由不存在或没挂鉴权", rec.Code)
	}
}
