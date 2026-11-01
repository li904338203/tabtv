package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

func intp(v int) *int { return &v }

// 单段 / 多段的分界要从模型目录读：模型看不到这一节就只能猜秒数（AGT-30 发现）。
func TestVideoModelSection_按目录事实给出且不知道时不编数(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		info    *VideoModelInfo
		err     error
		want    []string
		mustNot []string
	}{
		{name: "未指定", code: "", want: []string{"智能匹配", "quote"}},
		{name: "查不到", code: "sd2", err: errors.New("not found"), want: []string{"sd2", "查不到"}, mustNot: []string{"秒\n"}},
		{name: "不是视频模型", code: "gpt", info: &VideoModelInfo{IsVideo: false}, want: []string{"不是视频模型", "ask_user"}},
		{name: "已配置", code: "sd2", info: &VideoModelInfo{IsVideo: true, MaxDurationSec: intp(15), MultiShot: true},
			want: []string{"单次最长时长：15 秒", "多个镜头：是", "自带音轨：否", "一支视频一次生成", "超过 15 秒时，先用 ask_user",
				"用户明确要做更长的片子时"},
			mustNot: []string{"连续镜头", "按多段规划"}},
		// 改目录里的最长时长，分界随之变化（无代码改动）。
		{name: "目录上限改成 30 秒", code: "sd2", info: &VideoModelInfo{IsVideo: true, MaxDurationSec: intp(30), MultiShot: true},
			want: []string{"超过 30 秒时"}, mustNot: []string{"15 秒"}},
		// 会话 107419662995685376：目录标着不支持多镜头（库里全是默认的否），原先因此要求拆段。
		{name: "不支持多镜头也不拆段", code: "sd2", info: &VideoModelInfo{IsVideo: true, MaxDurationSec: intp(30)},
			want:    []string{"一支视频一次生成", "连续镜头", "不要把一支视频拆成几段", "超过 30 秒时"},
			mustNot: []string{"按多段规划", "单段条件：不满足"}},
		{name: "时长未配置", code: "sd2", info: &VideoModelInfo{IsVideo: true},
			want: []string{"后台未配置", "不要假设", "一支视频一次生成", "不要自行拆段"}, mustNot: []string{"15 秒", "按多段规划"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := VideoModelSection(c.code, c.info, c.err)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("应包含 %q：\n%s", w, got)
				}
			}
			for _, w := range c.mustNot {
				if strings.Contains(got, w) {
					t.Errorf("不应包含 %q：\n%s", w, got)
				}
			}
		})
	}
}

func TestResolveVideoModel_读会话偏好video_model(t *testing.T) {
	s := repo.Session{Prefs: json.RawMessage(`{"video":{"model":" sd2 "},"reasoning":{"model":"gpt"}}`)}
	if got := resolveVideoModel(s); got != "sd2" {
		t.Errorf("得到 %q", got)
	}
	if got := resolveVideoModel(repo.Session{}); got != "" {
		t.Errorf("无偏好应为空，得到 %q", got)
	}
}

func TestBuildContext_当前视频模型进系统提示词(t *testing.T) {
	out := BuildContext(ContextInput{VideoModel: "- 模型：sd2", Budget: 100})
	if !strings.Contains(out.System, "## 当前视频模型\n- 模型：sd2") {
		t.Errorf("系统提示词应包含当前视频模型一节：\n%s", out.System)
	}
	if out := BuildContext(ContextInput{Budget: 100}); strings.Contains(out.System, "当前视频模型") {
		t.Errorf("没有内容时不写这一节")
	}
}

// 04 文档 P-59：选了视频模型时列出它能用的生成方式（原先模型不知道有没有图生视频）；
// 没选时列出此刻可用的模型（原先模型手上一个 modelCode 都没有）。
func TestVideoModelExtras_能用的生成方式与可选模型(t *testing.T) {
	fits := []domain.OperationFit{
		{Code: "text2video", Output: "video", Roles: map[string]domain.RoleRange{"text": {Min: 0, Max: 8}}},
		{Code: "img2video", Output: "video", Roles: map[string]domain.RoleRange{"image": {Min: 1, Max: 1}},
			Limits: "单次时长 4~30 秒"},
	}
	got := VideoModelExtras("sd2", fits, nil)
	for _, want := range []string{"能用的生成方式", "  - text2video：只收文字", "  - img2video：图片恰好 1 个；单次时长 4~30 秒",
		"带参考图用收图片的方式"} {
		if !strings.Contains(got, want) {
			t.Errorf("应包含 %q：\n%s", want, got)
		}
	}
	got = VideoModelExtras("", nil, []ModelBrief{{Code: "sd2", Name: "Seedance 2", Operations: []string{"text2video", "img2video"}},
		{Code: "kl", Name: "kl", Operations: []string{"text2video"}}})
	if !strings.Contains(got, "- sd2（Seedance 2）：text2video、img2video") || !strings.Contains(got, "- kl：text2video") {
		t.Errorf("智能匹配应列出可选模型：\n%s", got)
	}
	// 假上游按第一行「- 模型：」认视频模型：补充段里不能出现这种行。
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "- 模型：") {
			t.Errorf("补充段不能有「- 模型：」开头的行：%q", line)
		}
	}
}

// prefsSection 按偏好算规格（没有问答卡）再拼「创作偏好」一节。
func prefsSection(prefs json.RawMessage, images []ModelBrief) string {
	return PrefsSection(prefs, ResolveVideoSpec(ParseGenPrefs(prefs), nil), images)
}

// 04 文档 P-60：偏好浮层里选的画幅 / 清晰度 / 时长 / 图片模型，原先服务端一个都不读。
func TestPrefsSection_偏好写进提示词(t *testing.T) {
	got := prefsSection(json.RawMessage(`{"video":{"model":"sd2","ratio":"9:16","resolution":"1080p","duration":15},`+
		`"image":{"model":"gpt_image_2"},"reasoning":{"model":"x"}}`), nil)
	for _, want := range []string{"- 画幅：9:16", "- 视频清晰度：1080p", "- 视频时长：15 秒", "不要拆成几段", "- 图片模型：gpt_image_2"} {
		if !strings.Contains(got, want) {
			t.Errorf("应包含 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "智能") || strings.Contains(got, "拆段并说明") {
		t.Errorf("都选定了就不要求确认，也不再提拆段：\n%s", got)
	}
	got = prefsSection(nil, []ModelBrief{{Code: "img1", Name: "图1", Operations: []string{"text2img", "img2img"}}})
	if !strings.Contains(got, "未指定（智能匹配）") || !strings.Contains(got, "img1（图1）：text2img、img2img") {
		t.Errorf("没选图片模型时列出可选的：\n%s", got)
	}
}

// 会话 107419662995685376：画幅 / 清晰度 / 时长都是「智能」时，Agent 一项没问就自己定了 9:16 · 15 秒 · 720p。
func TestPrefsSection_智能的规格要求先确认且确认过的照用(t *testing.T) {
	got := prefsSection(json.RawMessage(`{"video":{"model":"sd2"}}`), nil)
	for _, want := range []string{"画幅、清晰度、时长：用户没有指定", "必须先用 ask_user", "aspect_ratio / resolution / duration"} {
		if !strings.Contains(got, want) {
			t.Errorf("应包含 %q：\n%s", want, got)
		}
	}
	// 问答卡里确认过画幅与时长，清晰度的回答取不出具体值：确认过的照用、原文照写，只剩没问的。
	spec := ResolveVideoSpec(GenPrefs{}, map[string]string{domain.SpecRatio: "9:16 竖屏（推荐）",
		domain.SpecResolution: "高清", domain.SpecDuration: "15 秒"})
	if spec.Ratio != "9:16" || spec.Resolution != "" || spec.Duration != 15 || len(spec.Missing) != 0 {
		t.Fatalf("确认过的规格：%+v", spec)
	}
	got = PrefsSection(nil, spec, nil)
	for _, want := range []string{"- 画幅：9:16", "- 视频清晰度：用户回答「高清」", "- 视频时长：15 秒"} {
		if !strings.Contains(got, want) {
			t.Errorf("应包含 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "必须先用 ask_user") {
		t.Errorf("都确认过了不再要求：\n%s", got)
	}
	// 偏好里选定的优先于问答卡（用户在输入框里改了就以输入框为准）；只缺没问过的那一项。
	spec = ResolveVideoSpec(GenPrefs{Ratio: "16:9", Duration: "10"}, map[string]string{domain.SpecRatio: "9:16"})
	if spec.Ratio != "16:9" || spec.Duration != 10 || len(spec.Missing) != 1 || spec.Missing[0] != domain.SpecResolution {
		t.Fatalf("偏好优先、只缺清晰度：%+v", spec)
	}
}
