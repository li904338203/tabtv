package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// 只收地址形态参考媒体的协议（protocol.MediaURLRequiring）。会话 107440483873062912 与 2026-10-24 实测（R-13）：
// UniArt 收到内联图时，按大小分别回「413」「提示词过长」「requires prompt media references」，换成公网地址就成功。

// countingMedia 记下是否取过内联字节：只收地址的协议一次都不该取。
type countingMedia struct {
	stubMedia
	inlined *atomic.Int32
}

func (c countingMedia) Inline(ctx context.Context, id int64) ([]byte, string, error) {
	c.inlined.Add(1)
	return c.stubMedia.Inline(ctx, id)
}

func TestSubmit_只收地址的协议签不出外链时提交前判失败(t *testing.T) {
	cases := []struct {
		name   string
		media  stubMedia
		reason string
	}{
		{"本地 MinIO 的内网地址", stubMedia{url: "http://127.0.0.1:9100/b/a.png", mime: "image/png", data: []byte("PNG")},
			"签出的地址上游访问不到"},
		{"签发失败", stubMedia{urlErr: errors.New("存储不可用"), mime: "image/png", data: []byte("PNG")},
			"签发地址失败：存储不可用"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				fmt.Fprint(w, `{"id":"video_1","status":"queued"}`)
			}))
			defer srv.Close()
			p, _ := newProvider(t, RunConfig{
				ModelCode: "m-1", ProtocolCode: "uniart_video", BaseURL: srv.URL,
				SubmitPath: "/v1/videos", QueryPath: "/v1/videos/{taskId}", AuthType: "bearer", APIKey: "sk",
			})
			var inlined atomic.Int32
			p.SetMediaResolver(countingMedia{stubMedia: c.media, inlined: &inlined})
			job := uniartJob("ref2video", map[string]any{"prompt": "沙发", "aspect_ratio": "9:16"},
				[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 42, "mediaKind": "image"}})

			res, err := p.Submit(context.Background(), job)
			if err != nil || res.Outcome != gendomain.SubmitRejected {
				t.Fatalf("应在提交前判永久失败（退款、不重试）：%+v %v", res, err)
			}
			for _, want := range []string{"只收公网可访问的参考媒体地址", "参考媒体 42", c.reason, "对象存储"} {
				if !strings.Contains(res.Message, want) {
					t.Fatalf("原因要说清（缺 %q）：%s", want, res.Message)
				}
			}
			if hits.Load() != 0 || inlined.Load() != 0 {
				t.Fatalf("不该发上游、也不该取内联字节：上游 %d 次、内联 %d 次", hits.Load(), inlined.Load())
			}
		})
	}
}

func TestSubmit_只收地址的协议签得出外链时照常发地址(t *testing.T) {
	var submitted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &submitted)
		fmt.Fprint(w, `{"id":"video_1","status":"queued"}`)
	}))
	defer srv.Close()
	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "uniart_video", BaseURL: srv.URL,
		SubmitPath: "/v1/videos", QueryPath: "/v1/videos/{taskId}", AuthType: "bearer", APIKey: "sk",
	})
	p.SetMediaResolver(stubMedia{url: "https://cdn.example.com/a.png?sig=x", mime: "image/png", data: []byte("PNG")})
	job := uniartJob("ref2video", map[string]any{"prompt": "沙发", "aspect_ratio": "9:16"},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 42, "mediaKind": "image"}})
	if res, err := p.Submit(context.Background(), job); err != nil || res.ProviderTaskID != "video_1" {
		t.Fatalf("提交应受理: %+v %v", res, err)
	}
	meta, _ := submitted["metadata"].(map[string]any)
	if urls, _ := meta["image_urls"].([]any); len(urls) != 1 || urls[0] != "https://cdn.example.com/a.png?sig=x" {
		t.Fatalf("应发签名地址: %v", meta)
	}

	// 查询不发参考图：签不出外链不该让轮询失败（只有提交才要求地址形态）。
	p.SetMediaResolver(stubMedia{url: "http://127.0.0.1:9100/b/a.png", mime: "image/png", data: []byte("PNG")})
	if _, err := p.Query(context.Background(), job, "video_1"); err != nil {
		t.Fatalf("轮询不该因为参考媒体签不出外链而失败: %v", err)
	}
}

// 没声明的协议：外链够不着时照旧回落内联（本地开发唯一跑得通的路）。
func TestSubmit_没声明只收地址的协议照旧内联(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"看到了"}}]}`)
	}))
	defer srv.Close()
	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "openai_chat", BaseURL: srv.URL, SubmitPath: "/chat/completions",
	})
	src := []byte("\x89PNG-bytes")
	p.SetMediaResolver(stubMedia{url: "http://127.0.0.1:9100/b/a.png", mime: "image/png", data: src})
	if _, err := p.Submit(context.Background(), jobWith("openai_chat", nil,
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 99, "mediaKind": "image"}})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(src)) {
		t.Fatal("没声明的协议应原样内联")
	}
}

// ---------------------------------------------------------------- 公网中转（R-15）

type fakeRelay struct {
	url   string
	err   error
	calls atomic.Int32
}

func (f *fakeRelay) URL(context.Context, int64) (string, string, error) {
	f.calls.Add(1)
	return f.url, "image/jpeg", f.err
}

// uniartSubmit 用本地 MinIO 式的内网地址提交一张参考图，返回提交结果与上游收到的 metadata。
func uniartSubmit(t *testing.T, relay MediaRelay, signed string) (gendomain.SubmitOutcome, string, map[string]any, int32) {
	t.Helper()
	var submitted map[string]any
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &submitted)
		fmt.Fprint(w, `{"id":"video_1","status":"queued"}`)
	}))
	defer srv.Close()
	p, _ := newProvider(t, RunConfig{
		ModelCode: "m-1", ProtocolCode: "uniart_video", BaseURL: srv.URL,
		SubmitPath: "/v1/videos", QueryPath: "/v1/videos/{taskId}", AuthType: "bearer", APIKey: "sk",
	})
	var inlined atomic.Int32
	p.SetMediaResolver(countingMedia{stubMedia: stubMedia{url: signed, mime: "image/png", data: []byte("PNG")}, inlined: &inlined})
	if relay != nil {
		p.SetMediaRelay(relay)
	}
	res, err := p.Submit(context.Background(), uniartJob("ref2video", map[string]any{"prompt": "沙发", "aspect_ratio": "9:16"},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 42, "mediaKind": "image"}}))
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := submitted["metadata"].(map[string]any)
	if inlined.Load() != 0 {
		t.Fatal("只收地址的协议不该取内联字节")
	}
	return res.Outcome, res.Message, meta, hits.Load()
}

func TestSubmit_只收地址的协议够不着主存储时走公网中转(t *testing.T) {
	relay := &fakeRelay{url: "https://ss3.example.com/seeaitv/relay/a.png?sig=r"}
	outcome, msg, meta, _ := uniartSubmit(t, relay, "http://127.0.0.1:9100/b/a.png")
	if outcome == gendomain.SubmitRejected {
		t.Fatalf("配了中转就该照常提交：%s", msg)
	}
	if urls, _ := meta["image_urls"].([]any); len(urls) != 1 || urls[0] != relay.url {
		t.Fatalf("应发中转存储的签名地址：%v", meta)
	}

	// 主存储本来就够得着：不走中转（不白占中转服务器的带宽）。
	relay2 := &fakeRelay{url: relay.url}
	_, _, meta, _ = uniartSubmit(t, relay2, "https://cdn.example.com/a.png?sig=x")
	if urls, _ := meta["image_urls"].([]any); relay2.calls.Load() != 0 || len(urls) != 1 || urls[0] != "https://cdn.example.com/a.png?sig=x" {
		t.Fatalf("主存储够得着时直接发主存储的地址：calls=%d %v", relay2.calls.Load(), meta)
	}
}

func TestSubmit_公网中转失败时提交前判失败并说清(t *testing.T) {
	cases := []struct {
		name  string
		relay *fakeRelay
		want  string
	}{
		{"转存失败", &fakeRelay{err: errors.New("连接中断")}, "转存到公网中转存储也失败了：连接中断"},
		{"中转地址也是内网", &fakeRelay{url: "http://127.0.0.1:9101/relay/a.png"}, "公网中转存储签出的地址同样访问不到"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outcome, msg, _, hits := uniartSubmit(t, c.relay, "http://127.0.0.1:9100/b/a.png")
			if outcome != gendomain.SubmitRejected || hits != 0 {
				t.Fatalf("应在提交前判永久失败、不发上游：%v 上游 %d 次", outcome, hits)
			}
			for _, want := range []string{c.want, "SEEAITV_RELAY_STORAGE_*"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("原因要说清（缺 %q）：%s", want, msg)
				}
			}
		})
	}
}

// 收内联的协议：配了中转也照旧内联（本地开发时更快，也不占中转服务器的带宽）。
func TestSubmit_收内联的协议不走公网中转(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		fmt.Fprint(w, `{"choices":[{"message":{"content":"看到了"}}]}`)
	}))
	defer srv.Close()
	p, _ := newProvider(t, RunConfig{ModelCode: "m-1", ProtocolCode: "openai_chat", BaseURL: srv.URL, SubmitPath: "/chat/completions"})
	relay := &fakeRelay{url: "https://ss3.example.com/relay/a.png"}
	p.SetMediaResolver(stubMedia{url: "http://127.0.0.1:9100/b/a.png", mime: "image/png", data: []byte("PNG")})
	p.SetMediaRelay(relay)
	if _, err := p.Submit(context.Background(), jobWith("openai_chat", nil,
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 99, "mediaKind": "image"}})); err != nil {
		t.Fatal(err)
	}
	if relay.calls.Load() != 0 || !strings.Contains(body, "data:image/png;base64,") {
		t.Fatalf("应照旧内联、不走中转：calls=%d", relay.calls.Load())
	}
}
