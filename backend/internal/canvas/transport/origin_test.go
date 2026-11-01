package transport

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 项目来源 origin（MKD-3、MKT-AC-21③）：建后不可改。
//
// ⚠️ PATCH 按 map 解码、未知键静默忽略（B-9）：不显式拒绝的话带 origin 的请求回 200
// 而什么都没改 —— 这条测试拦的就是「看起来通过」。
func TestUpdateProject_带origin被拒(t *testing.T) {
	for _, body := range []string{
		`{"origin":"marketing"}`,
		`{"origin":null}`,
		`{"name":"新名字","origin":"marketing"}`,
		`{"origin":""}`,
	} {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatal(err)
		}
		_, err := parseUpdate(raw)
		var he *httpx.BizError
		if !errors.As(err, &he) || he.Code != errcode.InvalidArgument || !strings.Contains(he.Message, "origin") {
			t.Errorf("%s 应回 40001「origin 建后不可修改」，得到 %v", body, err)
		}
	}

	// 只带 name 照常解析。
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"name":"新名字"}`), &raw)
	in, err := parseUpdate(raw)
	if err != nil || in.Name == nil || *in.Name != "新名字" || in.FolderID != nil || in.CoverID != nil {
		t.Errorf("只带 name 应正常解析，得到 %+v %v", in, err)
	}
}

func TestParseKind_接受marketing(t *testing.T) {
	if k, ok := parseKind("marketing"); !ok || k != repo.KindMarketing {
		t.Errorf("parseKind(marketing) = (%q,%v)", k, ok)
	}
	if _, ok := parseKind("Marketing"); ok {
		t.Errorf("大小写不同的 Marketing 应被拒")
	}
}

// 响应里的 origin 字段一定在：历史项目为 null，营销项目为 "marketing"。
func TestProjectResp_origin字段一定在(t *testing.T) {
	h := &Handler{}
	for _, c := range []struct {
		origin string
		want   string
	}{{"", `"origin":null`}, {"marketing", `"origin":"marketing"`}} {
		b, err := json.Marshal(h.toResp(repo.Project{ID: 1, Origin: c.origin}, false))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("origin=%q 时响应应含 %s，得到 %s", c.origin, c.want, b)
		}
	}
}
