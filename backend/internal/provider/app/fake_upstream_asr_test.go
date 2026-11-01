package app

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// SC-AC-52（联调）：在**真的跑着的假上游**（scripts/fake-upstream）下，三条识别路径都拿到字级或词级结果，单位是毫秒；
// OpenAI 其它模型只拿到整段文字。设了 SEEAITV_FAKE_UPSTREAM（例如 http://127.0.0.1:18195）才跑。
//
// 与 asr_test.go 里 httptest 的区别：那边的服务端是测试自己写的，这边是独立进程、按各家形状校验请求 ——
// 证明「协议拼出来的请求」与「假上游认的形状」是同一个，SCT-42 的 E2E 沿用同一个假上游。
func TestFakeUpstream_识别三条路径(t *testing.T) {
	base := os.Getenv("SEEAITV_FAKE_UPSTREAM")
	if base == "" {
		t.Skip("未设 SEEAITV_FAKE_UPSTREAM")
	}
	data := []byte("ID3fake-speech-audio")
	cases := []struct {
		name, protocol, upstream, auth, header string
		url                                    string // 非空：签名地址（标准版只收地址）
		gran                                   string
	}{
		{"火山极速版（内联 base64）", "volc_asr", "volc.bigasr.auc_turbo", "header", "X-Api-Key", "", "char"},
		{"火山标准版（地址）", "volc_asr_standard", "volc.bigasr.auc", "header", "X-Api-Key", "https://relay.example.com/a.mp3?sig=1", "char"},
		{"OpenAI whisper-1（词级）", "openai_transcribe", "whisper-1", "bearer", "", "", "word"},
		{"OpenAI 其它模型（只有文字）", "openai_transcribe", "gpt-4o-transcribe", "bearer", "", "", "text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := base
			if c.protocol == "openai_transcribe" {
				b = base + "/v1"
			}
			p, logs := newProvider(t, RunConfig{ModelCode: "asr-m", ProtocolCode: c.protocol, UpstreamModel: c.upstream,
				BaseURL: b, AuthType: c.auth, APIKey: "fake-key", HeaderName: c.header})
			p.SetMediaResolver(stubMedia{url: c.url, mime: "audio/mpeg", data: data})
			job := jobWith(c.protocol, map[string]any{"keep_fillers": true},
				[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 9, "mediaKind": "audio"}})
			job.OperationCode, job.ResultType, job.IdempotencyKey = "speech2text", "text", "asr:9:abc:1"
			res, err := p.Submit(context.Background(), job)
			if err != nil {
				t.Fatal(err)
			}
			outs := res.Outputs
			if res.Outcome == gendomain.SubmitAccepted {
				for i := 0; i < 3 && len(outs) == 0; i++ {
					q, err := p.Query(context.Background(), job, res.ProviderTaskID)
					if err != nil {
						t.Fatal(err)
					}
					if q.Done {
						if !q.Success {
							t.Fatalf("查询失败: %s", q.Message)
						}
						outs = q.Outputs
					}
				}
			} else if res.Outcome != gendomain.SubmitCompleted {
				t.Fatalf("提交: %+v", res)
			}
			if len(outs) != 1 || outs[0].Type != "text" || outs[0].Text == "" {
				t.Fatalf("应拿到一个有文字的产物: %+v", outs)
			}
			var meta struct {
				Granularity string `json:"granularity"`
				Utterances  []struct {
					StartMs, EndMs int
					Words          []struct{ StartMs, EndMs int }
				} `json:"utterances"`
			}
			if err := json.Unmarshal(outs[0].Meta, &meta); err != nil || meta.Granularity != c.gran {
				t.Fatalf("粒度 %q，期望 %q（%s）", meta.Granularity, c.gran, outs[0].Meta)
			}
			if c.gran != "text" && (len(meta.Utterances) == 0 || len(meta.Utterances[0].Words) == 0 ||
				meta.Utterances[0].EndMs < 1000) {
				t.Fatalf("要有句与字（词），时间是毫秒: %s", outs[0].Meta)
			}
			if len(logs.all()) == 0 {
				t.Fatal("请求日志要落")
			}
		})
	}
}
