package app

import (
	"context"
	"os"
	"testing"

	genapp "github.com/li904338203/tabtv/backend/internal/generation/app"
	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
	"github.com/li904338203/tabtv/backend/internal/provider/runcfg"
)

// SC-AC-57（联调）：在**真的跑着的假上游**（scripts/fake-upstream）下，两家配乐与可灵视频配音效各走一遍：
// 豆包音乐带 V4 签名头、查询两次；ElevenLabs 同步回字节；可灵发出截出来的那一段、查询两次。
// 结果都是真的 MP3（量得出时长）。设了 SEEAITV_FAKE_UPSTREAM（例如 http://127.0.0.1:18195）才跑。
func TestFakeUpstream_配乐与音效(t *testing.T) {
	base := os.Getenv("SEEAITV_FAKE_UPSTREAM")
	if base == "" {
		t.Skip("未设 SEEAITV_FAKE_UPSTREAM")
	}
	needFFmpeg(t)
	run := func(t *testing.T, p *HTTPProvider, job genapp.Job) []byte {
		t.Helper()
		res, err := p.Submit(context.Background(), job)
		if err != nil {
			t.Fatal(err)
		}
		switch res.Outcome {
		case gendomain.SubmitCompleted:
			return readAll(t, res.Outputs[0])
		case gendomain.SubmitAccepted:
			for i := 0; i < 3; i++ {
				q, err := p.Query(context.Background(), job, res.ProviderTaskID)
				if err != nil {
					t.Fatal(err)
				}
				if q.Done {
					if !q.Success || len(q.Outputs) != 1 {
						t.Fatalf("应完成: %+v", q)
					}
					return readAll(t, q.Outputs[0])
				}
			}
			t.Fatal("查询三次还没完成")
		}
		t.Fatalf("没被受理: %+v", res)
		return nil
	}
	checkMP3 := func(t *testing.T, b []byte) {
		t.Helper()
		if secs, _ := probeAudioSeconds(t, b); secs < 1.5 || secs > 3 {
			t.Fatalf("结果应是一段量得出时长的 MP3，实际 %.3f 秒", secs)
		}
	}
	t.Run("火山豆包音乐（V4 签名、异步）", func(t *testing.T) {
		p, _ := newProvider(t, RunConfig{ModelCode: "bgm-volc", ProtocolCode: "volc_music", BaseURL: base + "/volc",
			SubmitPath: "/?Action=GenBGM&Version=2024-08-12", AuthType: runcfg.AuthVolcV4, APIKey: "fake-ak:fake-sk",
			SignRegion: "cn-beijing", SignService: "imagination"})
		job := jobWith("volc_music", map[string]any{"prompt": "轻快的钢琴", "duration": 30, "instrumental": true}, nil)
		job.OperationCode, job.ResultType = "text2music", "audio"
		checkMP3(t, run(t, p, job))
	})
	t.Run("ElevenLabs（同步字节）", func(t *testing.T) {
		p, _ := newProvider(t, RunConfig{ModelCode: "bgm-eleven", ProtocolCode: "elevenlabs_music", BaseURL: base,
			SubmitPath: "/v1/music", AuthType: "header", HeaderName: "xi-api-key", APIKey: "fake-key", UpstreamModel: "music_v1"})
		job := jobWith("elevenlabs_music", map[string]any{"prompt": "史诗弦乐", "duration": 30}, nil)
		job.OperationCode, job.ResultType = "text2music", "audio"
		checkMP3(t, run(t, p, job))
	})
	t.Run("可灵视频配音效（截出那一段、异步）", func(t *testing.T) {
		p, _ := newProvider(t, RunConfig{ModelCode: "sfx-kling", ProtocolCode: "kling_v2a", BaseURL: base,
			SubmitPath: "/v1/audio/video-to-audio", AuthType: "bearer", APIKey: "fake-key"})
		p.SetMediaResolver(&openMedia{src: sourceVideo(t, 10)})
		p.SetScratch(newMemScratch("https://scratch.example.com"), nil)
		job := jobWith("kling_v2a", map[string]any{"prompt": "脚步声"},
			[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 31, "mediaKind": "video", "rangeMs": []int{1000, 9000}}})
		job.OperationCode, job.ResultType = "video2audio", "audio"
		checkMP3(t, run(t, p, job))
	})
}
