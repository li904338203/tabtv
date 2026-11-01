package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 同步文本调用的请求日志要记调用方的操作名（DRT-19，DRD-24）：AI 搭景记 scene_build。
// 写死 text2text 的话，排查时按操作筛不出搭景的请求。请求体本身仍按 text2text 构造。
func TestCompleteText_请求日志记录调用方的操作名(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"{\"objects\":[]}"}}]}`)
	}))
	defer srv.Close()

	for _, c := range []struct{ op, want string }{{"scene_build", "scene_build"}, {"", "text2text"}} {
		p, logs := newProvider(t, chatCfg(srv.URL+"/v1", "openai_chat"))
		out, err := p.CompleteText(context.Background(), TextInput{ModelCode: "m-chat", System: "s", User: "u",
			Operation: c.op})
		if err != nil || out == "" {
			t.Fatalf("调用应成功：%q %v", out, err)
		}
		entries := logs.all()
		if len(entries) != 1 || entries[0].OperationCode != c.want {
			t.Fatalf("操作名 %q：请求日志应记 %q，得到 %+v", c.op, c.want, entries)
		}
		if gotBody["model"] != "up-chat" {
			t.Errorf("请求体照旧按文本对话构造，得到 %v", gotBody)
		}
	}
}
