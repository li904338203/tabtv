package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// CD-01：HTTP 状态码必须与业务码前 3 位一致。
// 这条是整份契约里最容易被"顺手改回去"的一条，所以用测试钉死。
func TestFail_状态码与业务码一致(t *testing.T) {
	cases := []errcode.Code{
		errcode.InvalidArgument,     // 400
		errcode.Unauthenticated,     // 401
		errcode.PermissionDenied,    // 403
		errcode.NotFound,            // 404
		errcode.VersionConflict,     // 409
		errcode.PayloadTooLarge,     // 413
		errcode.InsufficientCredits, // 422
		errcode.RateLimited,         // 429
		errcode.Internal,            // 500
		errcode.ProviderError,       // 502
	}
	for _, code := range cases {
		t.Run(code.Name(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			Fail(rec, req, Err(code))

			want, _ := strconv.Atoi(strconv.Itoa(int(code))[:3])
			if rec.Code != want {
				t.Fatalf("HTTP %d 与业务码 %d 的前三位不一致", rec.Code, code)
			}

			var body Error
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("响应体不是合法 JSON: %v", err)
			}
			if body.Code != int(code) {
				t.Errorf("body.code = %d，期望 %d", body.Code, code)
			}
			// 下发符号名，前端才不必硬编码数字。
			if body.Name != code.Name() {
				t.Errorf("body.name = %q，期望 %q", body.Name, code.Name())
			}
			if body.Message == "" {
				t.Error("错误文案不应为空")
			}
		})
	}
}

// 底层错误只进日志，不进响应体：它常含连接串、SQL 片段等不该外泄的信息。
func TestFail_底层错误不外泄(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	secret := "postgres://user:P@ssw0rd@10.0.0.1:5432/db"
	Fail(rec, req, Wrap(errcode.Internal, errors.New(secret)))

	if body := rec.Body.String(); contains(body, "P@ssw0rd") || contains(body, "10.0.0.1") {
		t.Fatalf("响应体泄漏了底层错误信息: %s", body)
	}
	if rec.Code != 500 {
		t.Errorf("期望 500，实际 %d", rec.Code)
	}
}

// 非 BizError 一律按内部错误处理，不能因为没包装就返回 200。
func TestFail_未包装错误按500(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	Fail(rec, req, errors.New("某个未预期的错误"))
	if rec.Code != 500 {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	var body Error
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.Code != int(errcode.Internal) {
		t.Errorf("期望 code=%d，实际 %d", errcode.Internal, body.Code)
	}
}

// 上游已生成的 traceId 必须复用 —— AC-26 要求同一 traceId
// 能追受理、任务、Provider 调用与结果，网关生成过就不该再换。
func TestTraceID_复用上游值(t *testing.T) {
	const upstream = "abc123upstream"
	var seen string
	h := TraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TraceIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(HeaderTraceID, upstream)
	h.ServeHTTP(rec, req)

	if seen != upstream {
		t.Errorf("context 中的 traceId = %q，期望 %q", seen, upstream)
	}
	if got := rec.Header().Get(HeaderTraceID); got != upstream {
		t.Errorf("响应头 traceId = %q，期望 %q", got, upstream)
	}
}

func TestTraceID_缺失时生成(t *testing.T) {
	var seen string
	h := TraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TraceIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if seen == "" {
		t.Fatal("应当生成 traceId")
	}
	// 响应头必须带上，否则客户端报障时提供不了线索。
	if rec.Header().Get(HeaderTraceID) != seen {
		t.Error("响应头与 context 中的 traceId 不一致")
	}
}

// 错误响应必须带 traceId，这是用户报障时唯一能提供的线索。
func TestFail_带traceId(t *testing.T) {
	var rec *httptest.ResponseRecorder
	h := TraceID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Fail(w, r, Err(errcode.NotFound))
	}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	var body Error
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body.TraceID == "" {
		t.Fatal("错误响应体必须带 traceId")
	}
	if body.TraceID != rec.Header().Get(HeaderTraceID) {
		t.Error("响应体与响应头的 traceId 应一致")
	}
}

// panic 必须转成标准错误结构，否则连接直接断开、客户端拿不到 traceId。
func TestRecover_panic转500(t *testing.T) {
	h := TraceID(Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != 500 {
		t.Fatalf("期望 500，实际 %d", rec.Code)
	}
	var body Error
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("panic 后响应体应仍是合法 JSON: %v", err)
	}
	if body.TraceID == "" {
		t.Error("panic 响应也必须带 traceId")
	}
	if contains(rec.Body.String(), "boom") {
		t.Error("panic 内容不应下发给客户端")
	}
}

// CD-02：成功响应不套信封，直接返回数据本身。
func TestJSON_不套信封(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusOK, map[string]string{"id": "123"})

	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if _, wrapped := body["data"]; wrapped {
		t.Error("成功响应不应有 data 信封层")
	}
	if _, wrapped := body["code"]; wrapped {
		t.Error("成功响应不应有 code 字段")
	}
	if body["id"] != "123" {
		t.Errorf("数据应直接返回，实际 %v", body)
	}
}

func TestErrcode_重试分类(t *testing.T) {
	if !errcode.RateLimited.Retryable() {
		t.Error("限流应可重试")
	}
	if errcode.InsufficientCredits.Retryable() {
		t.Error("积分不足重试也没用，不应标为可重试")
	}
	if !errcode.ProviderTimeout.Retryable() {
		t.Error("上游超时应可重试")
	}
	if errcode.ProviderRejected.Retryable() {
		t.Error("上游拒绝该输入属不可重试")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
