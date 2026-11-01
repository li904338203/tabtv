package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// withRole 造一个已经装好工作区上下文的请求（模拟 RequireWorkspace 跑过）。
func withRole(method string, role domain.Role) *http.Request {
	r := httptest.NewRequest(method, "/x", nil)
	ctx := context.WithValue(r.Context(), wsCtxKey{},
		WorkspaceContext{WorkspaceID: 1, Role: role, PayerAccountID: 2})
	return r.WithContext(ctx)
}

func run(mw func(http.Handler) http.Handler, r *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	passed := false
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true })).
		ServeHTTP(rec, r)
	if passed {
		return 0, ""
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code, body.Message
}

// ⚠️ 这张表就是 P12-03 修掉的那个漏洞的回归：到 P12-02 为止，
// media / generation / script / workflow / timeline 五个域一次都没查过角色 ——
// 一个只读成员可以上传素材、**发起生成花掉团队的积分**、跑整张图、渲成片。
func TestGuardByMethod(t *testing.T) {
	cases := []struct {
		method string
		role   domain.Role
		denied bool
	}{
		// 读：所有角色都行。
		{"GET", domain.RoleViewer, false},
		{"GET", domain.RoleEditor, false},
		{"HEAD", domain.RoleViewer, false},
		// 写：**默认要 edit** —— 新增一条写路由时忘了标注，它自动落在这里
		// 而不是无人看管。
		{"POST", domain.RoleViewer, true},
		{"PUT", domain.RoleViewer, true},
		{"PATCH", domain.RoleViewer, true},
		{"DELETE", domain.RoleViewer, true},
		{"POST", domain.RoleEditor, false},
		{"DELETE", domain.RoleAdmin, false},
	}
	for _, c := range cases {
		code, msg := run(GuardByMethod, withRole(c.method, c.role))
		if c.denied {
			if code != 40303 {
				t.Errorf("%s + %s 应被拒（40303），得到 %d", c.method, c.role, code)
			}
			// 说得出缺什么，用户才有下一步 —— 一句"权限不足"他不知道去找谁要什么。
			if msg == "" {
				t.Errorf("%s + %s 被拒时应说清缺哪一项", c.method, c.role)
			}
		} else if code != 0 {
			t.Errorf("%s + %s 应放行，却被拒 %d（%s）", c.method, c.role, code, msg)
		}
	}
}

func TestRequireAction(t *testing.T) {
	// 会扣费的入口标 generate：editor 有，viewer 没有。
	code, _ := run(RequireAction(domain.ActionGenerate), withRole("POST", domain.RoleEditor))
	if code != 0 {
		t.Errorf("editor 应能发起生成，却被拒 %d", code)
	}
	code, msg := run(RequireAction(domain.ActionGenerate), withRole("POST", domain.RoleViewer))
	if code != 40303 {
		t.Errorf("viewer 不该能发起生成，得到 %d", code)
	}
	if msg == "" {
		t.Error("应说清需要的是生成权限")
	}

	// 只读的 POST（运行前预览）放宽回 view —— 它不建 run、不扣费、可反复点，
	// 把只读成员挡在外面等于让他连"这张图要花多少钱"都看不到。
	if code, _ := run(RequireAction(domain.ActionView),
		withRole("POST", domain.RoleViewer)); code != 0 {
		t.Errorf("只读成员应能看运行前预览，却被拒 %d", code)
	}

	// manage 只有 owner/admin 有。
	if code, _ := run(RequireAction(domain.ActionManage),
		withRole("POST", domain.RoleEditor)); code != 40303 {
		t.Errorf("editor 不该有管理权限，得到 %d", code)
	}
}

// ⚠️ 拿不到工作区上下文**一律拒绝**，不是放行。
// 放行的话，任何一次中间件顺序写反都会让整组路由的权限闸静默消失 ——
// 而那不会有任何报错，测试也照样绿。
func TestGuardDeniesWithoutWorkspaceContext(t *testing.T) {
	bare := httptest.NewRequest("GET", "/x", nil)
	if code, _ := run(GuardByMethod, bare); code != 40302 {
		t.Fatalf("没有工作区上下文应被拒（40302），得到 %d", code)
	}
	if code, _ := run(RequireAction(domain.ActionView), bare); code != 40302 {
		t.Fatalf("显式闸同样应拒，得到 %d", code)
	}
}

// Allows 只给"同一个端点里免费与收费两种语义并存"的情形用（渲染的 dryRun）。
func TestAllows(t *testing.T) {
	if !Allows(withRole("POST", domain.RoleEditor), domain.ActionExport) {
		t.Error("editor 应可导出")
	}
	if Allows(withRole("POST", domain.RoleViewer), domain.ActionExport) {
		t.Error("viewer 不该可导出")
	}
	// 没有上下文时同样是拒绝。
	if Allows(httptest.NewRequest("POST", "/x", nil), domain.ActionView) {
		t.Error("没有工作区上下文时不该放行")
	}
}
