package transport

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// 非法 scene 在调服务之前就 400（svc 为 nil：走到服务层会 panic，测试即失败）。
func TestSkills_非法scene400(t *testing.T) {
	pass := func(n http.Handler) http.Handler { return n }
	r := chi.NewRouter()
	(&Handler{enabled: true}).Mount(r, pass, pass)
	for _, q := range []string{"?scene=writing", "?scene=Marketing", "?scene=general,marketing"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/skills"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 状态 %d，期望 400", q, rec.Code)
		}
	}
}

// 技能封面挂在 requireUser / requireWorkspace 之外（MKD-19、B-5），但受总开关控制。
func TestSkillCover_不经过登录闸(t *testing.T) {
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
	r := chi.NewRouter()
	(&Handler{enabled: false}).Mount(r, deny, deny)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/skills/x/cover", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("总开关关闭时封面应 404 AGENT_DISABLED（而不是先要求登录），得到 %d", rec.Code)
	}
}
