package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 建营销会话的请求体（MKT-T-17）：slot / order / scene / marketing 原样进到 app 入参。
func TestCreateSession_请求体slot与order往返(t *testing.T) {
	body := `{"createProject":{"name":"营销项目"},"scene":"marketing",
	  "message":{"text":"做一条","refs":[
	    {"kind":"marketing_product","id":"301","slot":"product"},
	    {"kind":"marketing_character","id":"401","slot":"character","order":2},
	    {"kind":"asset","id":"201","slot":"character","order":1}]},
	  "marketing":{"picks":{"creative":"cr_a","hook":null,"style":"st_b"}},
	  "skillCode":"mk_skill"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/agent/sessions", strings.NewReader(body))
	in, ok := parseCreate(rec, req)
	if !ok {
		t.Fatalf("解析失败：%d %s", rec.Code, rec.Body.String())
	}
	if in.Scene != domain.SceneMarketing || in.CreateProjectName == nil || *in.CreateProjectName != "营销项目" {
		t.Fatalf("scene / createProject 不对：%+v", in)
	}
	if len(in.Refs) != 3 || in.Refs[0].Kind != "marketing_product" || in.Refs[0].Slot != "product" ||
		in.Refs[0].Order != 0 || in.Refs[1].Kind != "marketing_character" || in.Refs[1].Order != 2 ||
		in.Refs[2].Slot != "character" || in.Refs[2].Order != 1 {
		t.Fatalf("refs 的 kind / slot / order 丢了：%+v", in.Refs)
	}
	m := in.Marketing
	if m == nil || m.LegacyProduct || m.Picks.Creative != "cr_a" || m.Picks.Hook != "" || m.Picks.Style != "st_b" {
		t.Fatalf("marketing 不对：%+v", m)
	}

	// 旧形状 marketing.product（MKD-27）：解析层认出来，交给 app 拒绝并点名（不报「未知字段」）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/agent/sessions", strings.NewReader(
		`{"scene":"marketing","message":{"text":"x"},"marketing":{"product":{"name":"杯","sellingPoints":"好"}}}`))
	if in, ok = parseCreate(rec, req); !ok || in.Marketing == nil || !in.Marketing.LegacyProduct {
		t.Fatalf("marketing.product 应标成旧形状：%d %+v", rec.Code, in.Marketing)
	}

	// 通用会话的请求体逐字不变：不带 scene 即 general（空串由 app 解析成 general）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/agent/sessions",
		strings.NewReader(`{"projectId":"5","message":{"text":"hi","refs":[{"kind":"media","id":"1"}]}}`))
	in, ok = parseCreate(rec, req)
	if !ok || in.Scene != "" || in.Marketing != nil || in.ProjectID != 5 {
		t.Fatalf("通用请求解析不对：%+v %s", in, rec.Body.String())
	}

	// 营销字段写错键名不能当成没传（严格解码）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/agent/sessions",
		strings.NewReader(`{"scene":"marketing","marketing":{"picks":{"creatve":"cr_a"}},"message":{"text":"x"}}`))
	if _, ok = parseCreate(rec, req); ok || rec.Code != http.StatusBadRequest {
		t.Fatalf("未知键应 400：%d", rec.Code)
	}
}

// 会话的 scene 建后不可改（B-18）：PATCH 用 patchReq 严格解码，带 scene / sceneInput 一律 400，
// 不静默忽略（Go 的解码默认忽略未知键，那样「改了 scene」会回 200 而什么都没发生）。
func TestPatchSession_带scene被拒(t *testing.T) {
	for _, body := range []string{`{"scene":"marketing"}`, `{"title":"x","sceneInput":{}}`, `{"scene":"general"}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/agent/sessions/1", strings.NewReader(body))
		var pr patchReq
		if decode(rec, req, &pr) || rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400，得到 %d", body, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/agent/sessions/1", strings.NewReader(`{"title":"新标题"}`))
	var pr patchReq
	if !decode(rec, req, &pr) || pr.Title == nil || *pr.Title != "新标题" {
		t.Fatalf("只改标题应通过：%d", rec.Code)
	}
}
