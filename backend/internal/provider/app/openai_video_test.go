package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
)

// stubMediaWithDuration 额外实现 MediaDurationResolver。
type stubMediaWithDuration struct {
	stubMedia
	ms int
}

func (s stubMediaWithDuration) DurationMs(context.Context, int64) (int, error) { return s.ms, nil }

func openAIVideoJob(op string, params map[string]any, inputs any) genapp.Job {
	j := jobWith("openai_video", params, inputs)
	j.OperationCode, j.ResultType = op, "video"
	return j
}

// OpenAI 兼容视频接口 全链路：提交体里带上**媒体域给的**参考视频时长；完成后的相对地址按 Provider 补全并带 key 下载。
func TestOpenAIVideo_提交带视频时长_相对产物同源带凭据(t *testing.T) {
	var submitted map[string]any
	var contentAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &submitted)
			fmt.Fprint(w, `{"id":"video_1","status":"queued"}`)
		case r.URL.Path == "/v1/videos/video_1":
			fmt.Fprint(w, `{"id":"video_1","status":"completed","result_url":"/v1/videos/video_1/content","requires_auth":true}`)
		case r.URL.Path == "/v1/videos/video_1/content":
			contentAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_video", BaseURL: srv.URL,
		SubmitPath: "/v1/videos", QueryPath: "/v1/videos/{taskId}",
		AuthType: "bearer", APIKey: "sk-uni",
	})
	p.SetMediaResolver(stubMediaWithDuration{
		// OpenAI 兼容视频接口 只收地址形态的参考媒体（R-13）：给一个上游够得着的签名地址。
		stubMedia: stubMedia{url: "https://cdn.example.com/src.mp4", mime: "video/mp4", data: []byte("SRC")}, ms: 7400,
	})
	job := openAIVideoJob("video2video", map[string]any{"prompt": "改成黄昏"},
		[]map[string]any{{"kind": "media", "order": 1, "mediaId": 42, "mediaKind": "video"}})

	res, err := p.Submit(context.Background(), job)
	if err != nil || res.ProviderTaskID != "video_1" {
		t.Fatalf("提交应受理: %+v %v", res, err)
	}
	meta, _ := submitted["metadata"].(map[string]any)
	if meta["input_video_duration"] != float64(8) {
		t.Fatalf("7.4 秒应报 8 秒: %+v", meta)
	}
	q, err := p.Query(context.Background(), job, "video_1")
	if err != nil || !q.Done || !q.Success || len(q.Outputs) != 1 {
		t.Fatalf("应成功取回产物: %+v %v", q, err)
	}
	if got := string(readAll(t, q.Outputs[0])); got != "MP4" {
		t.Fatalf("产物不对: %q", got)
	}
	if contentAuth != "Bearer sk-uni" {
		t.Fatalf("同源内容端点要带凭据: %q", contentAuth)
	}
}

// requires_auth=true 而 result_url 指向别的域名：改走同源内容端点，**不把 key 发给那个域名**。
func TestOpenAIVideo_要认证但不同源时改走内容端点不外发密钥(t *testing.T) {
	cdnHit := false
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHit = true
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer cdn.Close()

	var contentAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/videos/video_2":
			fmt.Fprintf(w, `{"id":"video_2","status":"completed","result_url":%q,"requires_auth":true}`,
				cdn.URL+"/out.mp4")
		case "/v1/videos/video_2/content":
			contentAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("MP4"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_video", BaseURL: srv.URL,
		SubmitPath: "/v1/videos", QueryPath: "/v1/videos/{taskId}",
		AuthType: "bearer", APIKey: "sk-uni",
	})
	job := openAIVideoJob("text2video", map[string]any{"prompt": "雪山"}, nil)
	q, err := p.Query(context.Background(), job, "video_2")
	if err != nil || !q.Success || len(q.Outputs) != 1 {
		t.Fatalf("应改走内容端点并成功: %+v %v", q, err)
	}
	_ = readAll(t, q.Outputs[0])
	if cdnHit {
		t.Fatal("不该去请求那个不同源的地址（要认证却不能带 key，请求注定 401）")
	}
	if contentAuth != "Bearer sk-uni" {
		t.Fatalf("内容端点要带凭据: %q", contentAuth)
	}
}
