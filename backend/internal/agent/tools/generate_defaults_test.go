package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 会话 107398945801502720：generate 没带画幅，上游按它自己的默认处理（该型号不支持，400）。
// 模型没写的画幅 / 清晰度按「用户的选择（偏好或确认过的回答）→ 营销创意推荐」补；都没有再由 canvasgen 按节点默认补。

func TestFillGenerateDefaults_顺序与只补缺的(t *testing.T) {
	prefs := app.VideoSpecState{Ratio: "1:1", Resolution: "1080p"}
	cases := []struct {
		name     string
		nodeType string
		params   map[string]any
		creative string
		want     map[string]any
	}{
		// 2026-10-24：用户选定 / 确认的画幅优先于创意推荐（推荐只是推荐）。
		{"视频：用户的选择优先于创意推荐", "video", map[string]any{"model": "sd"}, "9:16",
			map[string]any{"model": "sd", "aspect_ratio": "1:1", "resolution": "1080p"}},
		{"视频：没有创意推荐取用户的选择", "video", map[string]any{}, "",
			map[string]any{"aspect_ratio": "1:1", "resolution": "1080p"}},
		{"模型给了的不动（别名也算给了）", "video", map[string]any{"aspectRatio": "16:9", "resolution": "480p"}, "9:16",
			map[string]any{"aspectRatio": "16:9", "resolution": "480p"}},
		{"老形状 size.ratio 也算给了", "video", map[string]any{"size": map[string]any{"ratio": "4:3"}}, "9:16",
			map[string]any{"size": map[string]any{"ratio": "4:3"}, "resolution": "1080p"}},
		{"空串当没给", "video", map[string]any{"aspect_ratio": " "}, "",
			map[string]any{"aspect_ratio": "1:1", "resolution": "1080p"}},
		{"图片：只补画幅（取偏好），不补清晰度", "image", map[string]any{}, "",
			map[string]any{"aspect_ratio": "1:1"}},
		{"音频 / 文本不补", "audio", map[string]any{"voice": "v"}, "9:16", map[string]any{"voice": "v"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := map[string]any{}
			for k, v := range c.params {
				in[k] = v
			}
			got := fillGenerateDefaults(c.params, c.nodeType, c.creative, prefs)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
			if !reflect.DeepEqual(c.params, in) {
				t.Fatal("输入 map 不应被修改")
			}
		})
	}
	// 用户没选（也没确认）画幅：取创意推荐。
	if got := fillGenerateDefaults(map[string]any{}, "video", "9:16", app.VideoSpecState{}); got["aspect_ratio"] != "9:16" {
		t.Fatalf("用户没选时取创意推荐：%v", got)
	}
	// 什么来源都没有：原样（交给节点默认）。
	if got := fillGenerateDefaults(map[string]any{"model": "sd"}, "video", "", app.VideoSpecState{}); len(got) != 1 {
		t.Fatalf("没有来源时不补：%v", got)
	}
}

// ratioPlans 在 fakePlans 上加创意的推荐画幅（wiring.AgentPlanSource 同时实现 CreativeRatioSource）。
type ratioPlans struct {
	*fakePlans
	ratios map[string]string
	err    error
}

func (p ratioPlans) CreativeRatio(_ context.Context, code string) (string, error) {
	return p.ratios[code], p.err
}

func acceptedParams(t *testing.T, g *genGenerator) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(g.got.Params, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGenerate_营销_没写画幅按所选创意的推荐补(t *testing.T) {
	plans := ratioPlans{fakePlans: approvedPlan(), ratios: map[string]string{"unbox": "9:16"}}
	run := func(args string, prefs string) map[string]any {
		t.Helper()
		c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`)}
		tool, _, g := newGenerate(c)
		tool.Plans = plans
		rc := mkRC
		rc.Spec = app.ResolveVideoSpec(app.ParseGenPrefs(json.RawMessage(prefs)), nil)
		if _, err := tool.Run(context.Background(), rc, json.RawMessage(args)); err != nil {
			t.Fatal(err)
		}
		return acceptedParams(t, g)
	}
	// 会话 107398945801502720 的第二次调用：省略了全部参数，用户也没选画幅 —— 按创意推荐（不是节点默认的 16:9）。
	got := run(`{"planItemKey":"v1","prompt":"开箱","wait":false}`, `{"video":{"resolution":"1080p"}}`)
	if got["aspect_ratio"] != "9:16" || got["resolution"] != "1080p" {
		t.Fatalf("画幅按创意推荐、清晰度按偏好：%v", got)
	}
	// 用户在偏好里选定了画幅：以用户的为准（推荐只是推荐）。
	if got = run(`{"planItemKey":"v1","prompt":"开箱","wait":false}`, `{"video":{"ratio":"1:1"}}`); got["aspect_ratio"] != "1:1" {
		t.Fatalf("用户选定的画幅优先于创意推荐：%v", got)
	}
	// 模型写了就以模型为准。
	got = run(`{"planItemKey":"v1","prompt":"开箱","params":{"aspect_ratio":"16:9"},"wait":false}`, `{}`)
	if got["aspect_ratio"] != "16:9" {
		t.Fatalf("模型给的画幅不被推荐顶掉：%v", got)
	}

	// 创意没有推荐 / 取不到：退回偏好。
	plans.ratios = map[string]string{}
	if got = run(`{"planItemKey":"v1","prompt":"开箱","wait":false}`, `{"video":{"ratio":"3:4"}}`); got["aspect_ratio"] != "3:4" {
		t.Fatalf("没有推荐时取偏好：%v", got)
	}
	plans.err = errors.New("创意已下线")
	if got = run(`{"planItemKey":"v1","prompt":"开箱","wait":false}`, `{"video":{"ratio":"3:4"}}`); got["aspect_ratio"] != "3:4" {
		t.Fatalf("取推荐失败不挡生成，退回偏好：%v", got)
	}
}

func TestGenerate_通用会话_没写画幅按创作偏好补(t *testing.T) {
	c := &genCanvas{data: json.RawMessage(`{"nodes":[]}`)}
	tool, _, g := newGenerate(c)
	rc := genRC
	rc.Scene = domain.SceneGeneral
	rc.Spec = app.ResolveVideoSpec(app.ParseGenPrefs(json.RawMessage(`{"video":{"ratio":"9:16","resolution":"1080p","duration":10}}`)), nil)
	if _, err := tool.Run(context.Background(), rc, json.RawMessage(
		`{"nodeType":"video","modelCode":"sd","operationCode":"text2video","prompt":"海边","wait":false}`)); err != nil {
		t.Fatal(err)
	}
	got := acceptedParams(t, g)
	if got["aspect_ratio"] != "9:16" || got["resolution"] != "1080p" {
		t.Fatalf("新建节点按创作偏好补画幅与清晰度：%v", got)
	}
	// 时长不按偏好补（决定扣多少钱，以报价为准）。
	if _, ok := got["duration"]; ok || g.got.DurationSec != 0 {
		t.Fatalf("时长不按偏好补：%v dur=%d", got, g.got.DurationSec)
	}
}
