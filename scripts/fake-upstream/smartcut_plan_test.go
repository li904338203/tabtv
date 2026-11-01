package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 智能剪辑的规划回包（SCT-42）：对 api/fixtures/smartcut/draft/fake-reply-*.json 里的 user 消息，拼出的回包
// 逐字（按 JSON 比）等于 fixture 的 raw；那几份 fixture 又由后端的真解析器一项不丢地收下（TestDraftFixtures）。
// 两边钉在同一份数据上：回包与解析器对不上时红在这里，而不是红在 E2E 里查半天。
func TestSmartcutReply_与fixture逐字一致(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "api", "fixtures", "smartcut", "draft", "fake-reply-*.json"))
	if err != nil || len(files) < 4 {
		t.Fatalf("应有 4 份以上 fake-reply fixture：%v %v", files, err)
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Mode string `json:"mode"`
			User string `json:"user"`
			Raw  string `json:"raw"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		system := "创作要求……\n\n" + smartcutContractHeading + "\n模式：" + f.Mode + "（说明）\n\n只输出一个 JSON 对象"
		got, delay := smartcutReply(system, f.User)
		if delay != 0 {
			t.Errorf("%s：没有魔法词时不等", filepath.Base(path))
		}
		var g, w any
		if json.Unmarshal([]byte(got), &g) != nil || json.Unmarshal([]byte(f.Raw), &w) != nil || !reflect.DeepEqual(g, w) {
			t.Errorf("%s：\n期望 %s\n实际 %s", filepath.Base(path), f.Raw, got)
		}
	}
}

func TestSmartcutReply_模式识别与魔法词(t *testing.T) {
	sys := func(mode string) string { return smartcutContractHeading + "\n模式：" + mode + "（x）" }
	if m := smartcutMode("别的段落里写了 模式：talk\n" + sys("mix")); m != "mix" {
		t.Fatalf("模式码取契约标题之后的那一行：%s", m)
	}
	if !isSmartcutPlan(sys("mix")) || isSmartcutPlan("## 输出格式（导演台搭景）") {
		t.Fatal("按契约标题识别")
	}
	text, _ := smartcutReply(sys("mix"), "## 剪辑要求\n\n"+magicCutParseFail)
	if json.Valid([]byte(text)) {
		t.Fatalf("#剪辑解析失败 应回一段不是 JSON 的文字：%s", text)
	}
	old := smartcutSlowDelay
	smartcutSlowDelay = 20 * time.Millisecond
	defer func() { smartcutSlowDelay = old }()
	if _, d := smartcutReply(sys("mix"), "剪慢一点 "+magicCutSlow); d != 20*time.Millisecond {
		t.Fatalf("#剪辑慢 要先等：%v", d)
	}
	// 有拼图时给每段写一句画面描述；一段画面都没有时也回合法 JSON（服务端据此报素材不够）。
	text, _ = smartcutReply(sys("mix"), "## 素材清单\n\n- 编号 1：视频，2 秒\n\n## 缩略图拼图\n\n- 拼图 1：第 1 行是编号 1\n")
	if !strings.Contains(text, `"clipNotes"`) || !strings.Contains(text, `"1":`) {
		t.Fatalf("有拼图时写 clipNotes：%s", text)
	}
	if text, _ = smartcutReply(sys("mix"), "## 素材清单\n\n"); !json.Valid([]byte(text)) {
		t.Fatalf("没有素材也回合法 JSON：%s", text)
	}
	// 追加指令只有一段时改速度；解说按句轮流配画面。
	if text, _ = smartcutReply(sys("edit"), "- c1：素材编号 1（视频）"); !strings.Contains(text, `"speed"`) {
		t.Fatalf("只有一段时改速度：%s", text)
	}
	text, _ = smartcutReply(sys("narr"), "- s1（0.0~1.0 秒，共 1.0 秒）：第一句\n- s2（1.0~2.0 秒，共 1.0 秒）：第二句\n\n"+
		"## 素材清单\n\n- 编号 1：视频，3 秒\n- 编号 2：图片\n")
	if !strings.Contains(text, `"sentence":"s2"`) || !strings.Contains(text, `"clips":["2"]`) {
		t.Fatalf("解说按句轮流配画面：%s", text)
	}
	// 翻译：原文带 #译不出 的那条跳过（服务端据此报部分失败）。
	text, _ = smartcutReply(sys("translate"), "目标语言：英语（en）\n\n1. 第一句\n2. 第二句 "+magicCutNoTranslate+"\n3. 第三句\n")
	if !strings.Contains(text, `"n":1`) || strings.Contains(text, `"n":2`) || !strings.Contains(text, `"n":3`) {
		t.Fatalf("#译不出 的那条应跳过：%s", text)
	}
}

// 走真的路由：/v1/chat/completions 先于推理模型与「分镜」启发式认出智能剪辑请求，探针只记形态（图片张数与内联数）。
func TestSmartcutPlan_路由与探针(t *testing.T) {
	s := &server{addr: "http://fake"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chat)
	mux.HandleFunc("/_probe/smartcut", s.probeSmartcut)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body := `{"model":"m-plan","messages":[{"role":"system","content":"` + smartcutContractHeading + `\n模式：mix（素材混剪）\n分镜"},` +
		`{"role":"user","content":[{"type":"text","text":"## 素材清单\n\n- 编号 1：视频，2 秒\n- 编号 2：视频，3 秒"},` +
		`{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAAA"}}]}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Choices) != 1 || !strings.Contains(out.Choices[0].Message.Content, `"segments"`) {
		t.Fatalf("应回剪辑方案（不是分镜）：%+v", out)
	}
	resp, err = http.Get(srv.URL + "/_probe/smartcut?mode=mix&limit=1")
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Items []smartcutPlanRecord `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&probe)
	resp.Body.Close()
	if len(probe.Items) != 1 || probe.Items[0].Images != 1 || probe.Items[0].DataImages != 1 ||
		probe.Items[0].HTTPImages != 0 || probe.Items[0].Model != "m-plan" {
		t.Fatalf("探针：%+v", probe.Items)
	}
}
