package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// docs/agent/smartcut SCT-33：识别协议经 HTTPProvider 的失败分类（可重试 / 不可重试）与产物 meta。
// 火山的状态在响应头、HTTP 一律 200：只看 HTTP 码的话「服务端忙」会被判成解析失败、永久失败。
func TestSubmit_火山识别的失败分类(t *testing.T) {
	ok, err := os.ReadFile(filepath.Join("..", "protocol", "testdata", "volc_asr", "flash_success.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		code    string
		body    []byte
		outcome gendomain.SubmitOutcome
		status  int
		class   gendomain.RetryClass
	}{
		{"完成", "20000000", ok, gendomain.SubmitCompleted, 0, ""},
		{"静音照常完成", "20000003", []byte(`{}`), gendomain.SubmitCompleted, 0, ""},
		{"服务端忙：可重试", "55000031", []byte(`{}`), gendomain.SubmitRejected, 503, gendomain.RetryTransient},
		{"参数错误：不可重试", "45000001", []byte(`{}`), gendomain.SubmitRejected, 400, gendomain.RetryPermanent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotHeaders http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeaders = r.Header.Clone()
				w.Header().Set("X-Api-Status-Code", c.code)
				w.Header().Set("X-Api-Message", "msg")
				_, _ = w.Write(c.body)
			}))
			defer srv.Close()
			p, _ := newProvider(t, RunConfig{ModelCode: "asr-m", ProtocolCode: "volc_asr", BaseURL: srv.URL,
				AuthType: "header", APIKey: "volc-key", HeaderName: "X-Api-Key"})
			p.SetMediaResolver(stubMedia{url: "http://127.0.0.1:9000/a.mp3", mime: "audio/mpeg", data: []byte("ID3x")})
			job := jobWith("volc_asr", map[string]any{"keep_fillers": true},
				[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 9, "mediaKind": "audio"}})
			job.OperationCode, job.ResultType = "speech2text", "text"
			res, err := p.Submit(context.Background(), job)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != c.outcome || res.HTTPStatus != c.status {
				t.Fatalf("结论 %v / %d，期望 %v / %d（%s）", res.Outcome, res.HTTPStatus, c.outcome, c.status, res.Message)
			}
			if c.class != "" && gendomain.ClassifyHTTP(res.HTTPStatus) != c.class {
				t.Fatalf("分类应为 %v", c.class)
			}
			if gotHeaders.Get("X-Api-Key") != "volc-key" || gotHeaders.Get("X-Api-Request-Id") == "" ||
				gotHeaders.Get("X-Api-Resource-Id") != "volc.bigasr.auc_turbo" {
				t.Fatalf("认证与识别头: %v", gotHeaders)
			}
			if c.outcome == gendomain.SubmitCompleted {
				if len(res.Outputs) != 1 || res.Outputs[0].Type != "text" {
					t.Fatalf("应是一个文本产物: %+v", res.Outputs)
				}
				var meta map[string]any
				if err := json.Unmarshal(res.Outputs[0].Meta, &meta); err != nil || meta["granularity"] != "char" {
					t.Fatalf("产物要带识别 meta: %s", res.Outputs[0].Meta)
				}
			}
		})
	}
}
