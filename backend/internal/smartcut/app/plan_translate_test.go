package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// echoTranslator 按请求里的序号回译文（「EN:原文」）；skip 里的序号不回，shuffle 时倒着回（考对齐靠 n 不靠位置）。
type echoTranslator struct {
	mu      sync.Mutex
	users   []string
	skip    map[string]bool
	shuffle bool
	raw     string
}

func (e *echoTranslator) Complete(_ context.Context, in TextRequest) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.users = append(e.users, in.User)
	if e.raw != "" {
		return e.raw, nil
	}
	var items []map[string]any
	for _, line := range strings.Split(in.User, "\n") {
		var n int
		var text string
		if _, err := fmt.Sscanf(line, "%d. %s", &n, &text); err != nil || n == 0 {
			continue
		}
		if e.skip[text] {
			continue
		}
		items = append(items, map[string]any{"n": n, "text": "EN:" + text})
	}
	if e.shuffle {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return string(b), nil
}

func translateService(t *echoTranslator) *Service {
	s := &Service{}
	s.plan.Text = t
	s.plan.Config = PlanConfig{}.withDefaults()
	return s
}

func sourceLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("第%d句", i+1)
	}
	return out
}

// SCT-36：按 50 条一批调用，每批的序号从 1 起，译文按序号对回全局的那一条（不按位置：模型倒着回也对得上）。
func TestTranslate_分批与一一对应(t *testing.T) {
	tr := &echoTranslator{shuffle: true}
	texts, failed, err := translateService(tr).translateBatches(context.Background(), "m", "sys", "en", sourceLines(120))
	if err != nil || len(failed) != 0 || len(texts) != 120 {
		t.Fatalf("%d 条，失败 %v，%v", len(texts), failed, err)
	}
	if len(tr.users) != 3 {
		t.Fatalf("50 + 50 + 20 分三批：%d", len(tr.users))
	}
	if !strings.Contains(tr.users[1], "\n1. 第51句") || !strings.Contains(tr.users[2], "\n20. 第120句") ||
		!strings.Contains(tr.users[0], "英文（en）") {
		t.Fatalf("每批序号从 1 起、带目标语言：\n%s", tr.users[1][:80])
	}
	if texts[51] != "EN:第51句" || texts[120] != "EN:第120句" || texts[1] != "EN:第1句" {
		t.Fatalf("按序号对回全局：%q %q %q", texts[1], texts[51], texts[120])
	}
}

// SCT-36（SC-F-126）：部分条目没译出来 —— 说清是哪几条（全局序号），其余照常；修过一次仍缺的照样记失败。
func TestTranslate_部分失败只写成功的(t *testing.T) {
	tr := &echoTranslator{skip: map[string]bool{"第3句": true, "第57句": true}}
	texts, failed, err := translateService(tr).translateBatches(context.Background(), "m", "sys", "ja", sourceLines(60))
	if err != nil || len(texts) != 58 {
		t.Fatalf("%d %v", len(texts), err)
	}
	if fmt.Sprint(failed) != "[{3 AI 没有给出这一条的译文} {57 AI 没有给出这一条的译文}]" {
		t.Fatalf("失败的序号：%v", failed)
	}
	// 缺条会回灌修一次（两批各 2 次调用）。
	if len(tr.users) != 4 || !strings.Contains(tr.users[1], "## 上一次的输出") {
		t.Fatalf("缺条时回灌修一次：%d", len(tr.users))
	}
	if _, ok := texts[3]; ok {
		t.Fatal("没译出来的不在结果里")
	}
}

// SCT-36：一条都没译出来 —— 回灌修过仍解析不了，全部记失败（不是调用失败，所以不按调用错误报）。
func TestTranslate_全部失败不改时间线(t *testing.T) {
	tr := &echoTranslator{raw: "我翻译不了"}
	texts, failed, err := translateService(tr).translateBatches(context.Background(), "m", "sys", "ko", sourceLines(3))
	if len(texts) != 0 || len(failed) != 3 || err != nil || !strings.Contains(failed[0].Reason, "AI 的译文无法解析") {
		t.Fatalf("%v %v %v", texts, failed, err)
	}
	// 写入那一步（Translate 里）见 texts 为空就不写；集成测试 TestTranslatePlan_全部失败不改时间线 核对版本没动。
}

// stubPorts 只为让 planReady 通过：校验失败的用例不会碰到它们。
type stubCalls struct{ PlanCalls }
type stubCatalog struct{ PlanCatalog }
type stubPrompts struct{ Prompts }
type stubTimelines struct{ PlanTimelines }

// SCT-36（SCK-12）：目标语言只有中英日韩；写法只有新轨 / 替换；baseVersion 要来自服务端。都在碰任何端口之前拦下。
func TestTranslate_语言白名单(t *testing.T) {
	s := &Service{}
	s.SetPlanPorts(PlanPorts{Calls: stubCalls{}, Catalog: stubCatalog{}, Text: &echoTranslator{}, Prompts: stubPrompts{},
		Timelines: stubTimelines{}, Material: &talkMaterial{}, NextID: func() int64 { return 1 }})
	for _, c := range []struct {
		req  TranslateRequest
		path string
	}{
		{TranslateRequest{TargetLang: "fr", BaseVersion: 3}, "targetLang"},
		{TranslateRequest{TargetLang: "zh-TW", BaseVersion: 3}, "targetLang"},
		{TranslateRequest{TargetLang: "en", Mode: "merge", BaseVersion: 3}, "mode"},
		{TranslateRequest{TargetLang: "en"}, "baseVersion"},
	} {
		_, err := s.Translate(context.Background(), c.req)
		be := bizOf(t, err)
		if be.Code != errcode.InvalidArgument || be.Details["path"] != c.path {
			t.Fatalf("%+v：%v %v", c.req, be.Message, be.Details)
		}
	}
	for _, lang := range TranslateLangs {
		if langNames[lang] == "" {
			t.Fatalf("白名单里的语言都要有叫法：%s", lang)
		}
	}
	if fmt.Sprint(TranslateLangs) != "[zh-CN en ja ko]" || domain.TranslateBatch != 50 {
		t.Fatal("中英日韩，50 条一批")
	}
}
