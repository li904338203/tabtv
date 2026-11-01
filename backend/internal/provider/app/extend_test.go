package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
	"github.com/li904338203/tabtv/backend/internal/provider/transport"
)

// docs/agent/smartcut SCT-32：Provider 层的通用扩展（V4 签名、产物 meta）。

// 火山 V4：签名要覆盖**最终发出的正文**。用 multipart（正文在构造请求之后才编码）验：上游按收到的
// 方法、地址、Content-Type、正文与 X-Date 重算一遍签名，必须与请求头里的一致。
func TestVolcV4_签名覆盖最终的multipart正文(t *testing.T) {
	cred := transport.VolcCredential{AccessKey: "AKtest", SecretKey: "SKtest", Region: "cn-north-1", Service: "imagination"}
	var verified bool
	var gotHeaders http.Header
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Date"))
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		want, err := transport.SignVolcV4(r.Method, srv.URL+r.URL.RequestURI(),
			map[string]string{"Content-Type": r.Header.Get("Content-Type")}, body, cred, at)
		if err != nil || want["Authorization"] != r.Header.Get("Authorization") {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"SignatureDoesNotMatch"}`)
			return
		}
		verified = true
		fmt.Fprint(w, `{"data":[{"b64_json":"`+base64.StdEncoding.EncodeToString([]byte("EDITED"))+`"}]}`)
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{
		ModelCode: "img-1", ProtocolCode: "openai_images", BaseURL: srv.URL,
		AuthType: runcfg.AuthVolcV4, APIKey: "AKtest:SKtest", SignRegion: "cn-north-1", SignService: "imagination",
		Headers: map[string]string{"X-Note": "plain"},
	})
	p.SetMediaResolver(stubMedia{url: "http://127.0.0.1:9000/a.png", mime: "image/png", data: []byte("SRCIMG")})
	res, err := p.Submit(context.Background(), jobWith("openai_images", map[string]any{"prompt": "改成夜景"},
		[]map[string]any{{"kind": "generation", "order": 1, "mediaId": 5, "mediaKind": "image"}}))
	if err != nil {
		t.Fatal(err)
	}
	if !verified || res.Outcome != gendomain.SubmitCompleted {
		t.Fatalf("上游重算的签名应与请求头一致: %+v / %s", res.Outcome, res.Message)
	}
	if !strings.HasPrefix(gotHeaders.Get("Content-Type"), "multipart/form-data; boundary=") {
		t.Fatalf("这条用例要的是 multipart 正文: %q", gotHeaders.Get("Content-Type"))
	}
	if gotHeaders.Get("X-Note") != "plain" {
		t.Fatal("普通自定义头照常发出")
	}
	if strings.Contains(gotHeaders.Get("Authorization"), "SKtest") {
		t.Fatal("SK 不能出现在请求里")
	}
	_ = readAll(t, res.Outputs[0])
}

// 缺地域 / 服务：在发请求之前就判永久失败（不发上游、退款），原因点名要配的请求头。
func TestVolcV4_缺地域时不发请求并说清缺什么(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	p, _ := newProvider(t, RunConfig{
		ModelCode: "t-1", ProtocolCode: "generic_sync", BaseURL: srv.URL,
		AuthType: runcfg.AuthVolcV4, APIKey: "AK:SK", SignService: "imagination",
	})
	res, err := p.Submit(context.Background(), jobWith("generic_sync", map[string]any{"prompt": "x"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("签不出名就不该发请求")
	}
	if res.Outcome != gendomain.SubmitRejected || !strings.Contains(res.Message, runcfg.VolcRegionHeader) {
		t.Fatalf("应判永久失败并点名 %s: %+v", runcfg.VolcRegionHeader, res)
	}
}

// 产物 meta（SCD-32）：协议给的结构化信息原样带到生成侧；没有 meta 的产物仍是空（落库时写 {}）。
func TestMaterialize_产物meta原样带到生成侧(t *testing.T) {
	p, _ := newProvider(t, RunConfig{ModelCode: "m-1"})
	meta := json.RawMessage(`{"utterances":[{"text":"大家好","start":120,"end":880}]}`)
	outs, err := p.materialize(context.Background(), RunConfig{}, "text", []protocol.Output{
		{Kind: "text", Value: "大家好", Meta: meta},
		{Kind: "base64", Value: base64.StdEncoding.EncodeToString([]byte("RIFF")), MIME: "audio/wav", Meta: json.RawMessage(`{"k":1}`)},
		{Kind: "text", Value: "没有 meta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(outs[0].Meta) != string(meta) || string(outs[1].Meta) != `{"k":1}` {
		t.Fatalf("meta 要原样带过去: %s / %s", outs[0].Meta, outs[1].Meta)
	}
	if outs[2].Meta != nil {
		t.Fatalf("没有 meta 的产物不该凭空多出一个: %s", outs[2].Meta)
	}
	_ = readAll(t, outs[1])
}
