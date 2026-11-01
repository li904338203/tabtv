package app

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// SC-AC-56（联调）：在**真的跑着的假上游**（scripts/fake-upstream）下走一遍去字幕 —— 发出去的是截出来的 3 秒
// （假上游记下的 video_url 就是那一段的签名地址），查询两次后完成，结果从假上游的 /files/ 取回。
// 设了 SEEAITV_FAKE_UPSTREAM（例如 http://127.0.0.1:18195）才跑。
func TestFakeUpstream_去字幕(t *testing.T) {
	base := os.Getenv("SEEAITV_FAKE_UPSTREAM")
	if base == "" {
		t.Skip("未设 SEEAITV_FAKE_UPSTREAM")
	}
	needFFmpeg(t)
	p, _ := newProvider(t, RunConfig{ModelCode: "erase-m", ProtocolCode: "volc_mediakit_erase", BaseURL: base,
		SubmitPath: "/api/v1/tools/erase-video-subtitle-pro", QueryPath: "/api/v1/tasks/{taskId}",
		AuthType: "bearer", APIKey: "fake-key"})
	p.SetMediaResolver(&openMedia{src: sourceVideo(t, 60)})
	store := newMemScratch("https://scratch.example.com")
	p.SetScratch(store, nil)
	job := jobWith("volc_mediakit_erase", map[string]any{"mode": "subtitle"},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 31, "mediaKind": "video", "rangeMs": []int{2000, 5000}}})
	job.OperationCode, job.ResultType = "video_desubtitle", "video"

	res, err := p.Submit(context.Background(), job)
	if err != nil || res.Outcome != gendomain.SubmitAccepted {
		t.Fatalf("应受理: %+v %v", res, err)
	}
	key := "scratch/range/7001/1-2000-5000.mp4"
	objs, _ := store.snapshot()
	if secs, _ := probeSeconds(t, objs[key]); secs < 2.96 || secs > 3.04 {
		t.Fatalf("截出来的应是 3 秒，实际 %.3f", secs)
	}
	var probe struct {
		Items []struct {
			TaskID, VideoURL, Mode, Outcome string
			Bearer                          bool
		} `json:"items"`
	}
	resp, err := http.Get(base + "/_probe/smartcut/media")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&probe)
	_ = resp.Body.Close()
	var got bool
	for _, it := range probe.Items {
		if it.TaskID == res.ProviderTaskID && it.Outcome == "submitted" {
			got = it.VideoURL == "https://scratch.example.com/"+key+"?X-Amz-Signature=abc" && it.Mode == "subtitle" && it.Bearer
		}
	}
	if !got {
		t.Fatalf("假上游记下的应是那 3 秒的签名地址: %+v", probe.Items)
	}
	var outs []byte
	for i := 0; i < 3 && outs == nil; i++ {
		q, err := p.Query(context.Background(), job, res.ProviderTaskID)
		if err != nil {
			t.Fatal(err)
		}
		if q.Done {
			if !q.Success || len(q.Outputs) != 1 {
				t.Fatalf("应完成: %+v", q)
			}
			outs = readAll(t, q.Outputs[0])
		}
	}
	if secs, _ := probeSeconds(t, outs); secs <= 0 {
		t.Fatal("结果要取回存档（假上游给的是一段真的 MP4）")
	}
}
