package domain

import (
	"strings"
	"testing"
)

// 风格只修饰图片、特效只修饰视频，不可互换。
func TestMaterialType_修饰目标(t *testing.T) {
	if got, _ := MaterialStyle.TargetNodeType(); got != "image" {
		t.Errorf("风格应修饰 image，得到 %q", got)
	}
	if got, _ := MaterialEffect.TargetNodeType(); got != "video" {
		t.Errorf("特效应修饰 video，得到 %q", got)
	}
	// 音色不是修饰输入，是参数。
	if _, err := MaterialVoice.TargetNodeType(); err == nil {
		t.Error("音色不应作为修饰输入")
	}
}

// 提示词拼接在服务端：前端拼出来的词会落进 canvas_data，
// 用户改一个字玩法就失效，历史节点也永远停在旧词。
func TestMaterialPromptSuffix(t *testing.T) {
	got := MaterialPromptSuffix([]string{"水彩风格"}, []string{"慢镜头"})
	if got != "水彩风格，慢镜头" {
		t.Errorf("得到 %q", got)
	}
	// 空白项被剔除，不留下孤立的分隔符。
	got = MaterialPromptSuffix([]string{"  ", "赛博朋克"}, []string{""})
	if got != "赛博朋克" {
		t.Errorf("空白应被剔除，得到 %q", got)
	}
	if got := MaterialPromptSuffix(nil, nil); got != "" {
		t.Errorf("无素材应返回空串，得到 %q", got)
	}
	// 风格在前、特效在后。
	got = MaterialPromptSuffix([]string{"A"}, []string{"B"})
	if !strings.HasPrefix(got, "A") {
		t.Errorf("风格应排在特效之前，得到 %q", got)
	}
}

func TestValidateMaterial(t *testing.T) {
	ok := Material{Code: "watercolor", Name: "水彩", Type: MaterialStyle, Prompt: "水彩风格"}
	if err := ValidateMaterial(ok); err != nil {
		t.Errorf("正常素材应通过: %v", err)
	}
	// 没有提示词的风格 = 插入画布后不产生任何效果。
	noPrompt := ok
	noPrompt.Prompt = "   "
	if err := ValidateMaterial(noPrompt); err == nil {
		t.Error("风格缺提示词应被拒绝")
	}
	for _, bad := range []Material{
		{Code: "x", Name: "X", Type: "sticker", Prompt: "p"},
		{Code: "", Name: "X", Type: MaterialStyle, Prompt: "p"},
		{Code: "x", Name: "", Type: MaterialStyle, Prompt: "p"},
	} {
		if err := ValidateMaterial(bad); err == nil {
			t.Errorf("非法素材应被拒绝: %+v", bad)
		}
	}
	// 音色不要求 prompt
	voice := Material{Code: "v1", Name: "音色", Type: MaterialVoice}
	if err := ValidateMaterial(voice); err != nil {
		t.Errorf("音色不应要求 prompt: %v", err)
	}
}

// 没有上游 voice ID 的音色无法真正合成，
// 启用它等于让用户选一个必然失败的选项。
func TestValidateVoice(t *testing.T) {
	ok := Voice{Code: "xiaoyun", Name: "小云", UpstreamVoiceID: "zh-CN-XiaoyunNeural",
		Gender: "female", IsEnabled: true}
	if err := ValidateVoice(ok); err != nil {
		t.Errorf("正常音色应通过: %v", err)
	}
	noUpstream := ok
	noUpstream.UpstreamVoiceID = ""
	if err := ValidateVoice(noUpstream); err == nil {
		t.Error("启用但无上游 voice ID 应被拒绝")
	}
	// 未启用时允许暂缺上游 ID（先建档后补）。
	draft := noUpstream
	draft.IsEnabled = false
	if err := ValidateVoice(draft); err != nil {
		t.Errorf("未启用时应允许暂缺上游 ID: %v", err)
	}
	badGender := ok
	badGender.Gender = "robot"
	if err := ValidateVoice(badGender); err == nil {
		t.Error("非法 gender 应被拒绝")
	}
}
