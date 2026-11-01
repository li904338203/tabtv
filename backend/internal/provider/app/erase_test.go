package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	gendomain "github.com/li904338203/tabtv/backend/internal/generation/domain"
)

// SC-AC-56（上游往返，docs/agent/smartcut SCT-38）：60 秒素材取 [2000, 5000] 去字幕 ——
// 截出来的 3 秒放进存储、发给上游的是它的签名地址（Bearer 认证）；查询第一次处理中、第二次完成；
// 结果链接当场下载归档（结果在另一个域名上：不带我们的密钥）；到终态之后截出来的那份删掉。
func TestMediaKitErase_区间片段发出3秒并取回结果(t *testing.T) {
	needFFmpeg(t)
	erased := []byte("ERASED-MP4")
	var mu sync.Mutex
	var tosAuth, submitAuth string
	var submitted map[string]any
	polls := 0
	tos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tosAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(erased)
	}))
	defer tos.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tools/erase-video-subtitle-pro":
			submitAuth = r.Header.Get("Authorization")
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &submitted)
			fmt.Fprint(w, `{"task_id":"erase-1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/erase-1":
			polls++
			if polls == 1 {
				fmt.Fprint(w, `{"task_id":"erase-1","status":"processing"}`)
				return
			}
			fmt.Fprintf(w, `{"task_id":"erase-1","status":"completed","video_url":%q,"result":{"video_url":%q}}`,
				submitted["video_url"], tos.URL+"/erase/erase-1.mp4?X-Tos-Signature=def")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p, _ := newProvider(t, RunConfig{ModelCode: "erase-m", ProtocolCode: "volc_mediakit_erase", BaseURL: srv.URL,
		SubmitPath: "/api/v1/tools/erase-video-subtitle-pro", QueryPath: "/api/v1/tasks/{taskId}",
		AuthType: "bearer", APIKey: "mk-key"})
	p.SetMediaResolver(&openMedia{src: sourceVideo(t, 60)})
	store := newMemScratch("https://scratch.example.com")
	p.SetScratch(store, nil)
	job := jobWith("volc_mediakit_erase", map[string]any{"mode": "subtitle"},
		[]map[string]any{{"kind": "reference", "order": 1, "mediaId": 31, "mediaKind": "video", "rangeMs": []int{2000, 5000}}})
	job.OperationCode, job.ResultType = "video_desubtitle", "video"

	res, err := p.Submit(context.Background(), job)
	if err != nil || res.Outcome != gendomain.SubmitAccepted || res.ProviderTaskID != "erase-1" {
		t.Fatalf("应受理并记下任务 ID: %+v %v", res, err)
	}
	key := "scratch/range/7001/1-2000-5000.mp4"
	objs, _ := store.snapshot()
	if secs, _ := probeSeconds(t, objs[key]); secs < 2.96 || secs > 3.04 {
		t.Fatalf("发出去的应是截出来的 3 秒，实际 %.3f 秒", secs)
	}
	mu.Lock()
	if submitted["video_url"] != "https://scratch.example.com/"+key+"?X-Amz-Signature=abc" || submitted["mode"] != "subtitle" ||
		submitAuth != "Bearer mk-key" {
		t.Fatalf("提交体与认证: %+v %q", submitted, submitAuth)
	}
	mu.Unlock()

	q, err := p.Query(context.Background(), job, "erase-1")
	if err != nil || q.Done {
		t.Fatalf("第一次查询处理中: %+v %v", q, err)
	}
	q, err = p.Query(context.Background(), job, "erase-1")
	if err != nil || !q.Done || !q.Success || len(q.Outputs) != 1 {
		t.Fatalf("第二次查询完成: %+v %v", q, err)
	}
	if got := readAll(t, q.Outputs[0]); string(got) != string(erased) {
		t.Fatalf("取回的应是结果链接的内容（不是回显的输入地址）: %q", got)
	}
	mu.Lock()
	if tosAuth != "" {
		t.Fatalf("结果在别的域名上：不带我们的密钥（%q）", tosAuth)
	}
	mu.Unlock()
	if objs, deleted := store.snapshot(); len(objs) != 0 || len(deleted) != 1 || deleted[0] != key {
		t.Fatalf("到终态之后删掉截出来的那份: objs=%d deleted=%v", len(objs), deleted)
	}
}
