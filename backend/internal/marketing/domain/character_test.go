package domain

import (
	"strings"
	"testing"
)

// 每个禁用字符单独一行：漏掉任何一个，「@小美」这类名字就会被切成两段（MKD-36）。
// 规则本身在 library.go（与营销角色共用），这里钉住角色库依赖的那部分行为。
func TestCharacterNameIssue_禁用字符逐个(t *testing.T) {
	forbidden := []rune(CharacterNameForbidden)
	if len(forbidden) != 13 {
		t.Fatalf("禁用字符表应为 13 个（@ < > / \\ | : * ? ( ) 与全角括号），实际 %d", len(forbidden))
	}
	for _, r := range forbidden {
		c := string(r)
		t.Run(c, func(t *testing.T) {
			is := CharacterNameIssue("name", "柚子"+c+"君")
			if len(is) != 1 {
				t.Fatalf("含 %q 应被拒：%+v", c, is)
			}
			if is[0].Code != IssueBadFormat || is[0].Field != "name" {
				t.Fatalf("问题分类不对：%+v", is)
			}
			if !strings.Contains(is[0].Message, "「"+c+"」") {
				t.Fatalf("文案要点名禁用字符 %q：%s", c, is[0].Message)
			}
		})
	}
	for _, ok := range []string{"柚子君", "Yuzu Kun", "小美-2号", "a_b", "柚子·君"} {
		if is := CharacterNameIssue("name", ok); is != nil {
			t.Errorf("%q 应合法：%+v", ok, is)
		}
	}
}

// 码点计数：emoji 计 1，不能用字节长度（§5.2 per_kchar 同一坑）。
func TestCharacterName_码点计数(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
		code IssueCode
	}{
		{"", false, IssueRequired},
		{"柚", true, ""},
		{strings.Repeat("柚", 50), true, ""},
		{strings.Repeat("😀", 50), true, ""},
		{strings.Repeat("柚", 51), false, IssueTooLong},
		{strings.Repeat("a", 51), false, IssueTooLong},
	}
	for _, c := range cases {
		is := CharacterNameIssue("name", c.name)
		if c.ok != (len(is) == 0) {
			t.Errorf("%d 码点：期望 ok=%v，实际 %+v", RuneLen(c.name), c.ok, is)
			continue
		}
		if len(is) > 0 && is[0].Code != c.code {
			t.Errorf("%d 码点：期望 %s，实际 %s", RuneLen(c.name), c.code, is[0].Code)
		}
	}
}

func TestValidateCharacter_图片与音频(t *testing.T) {
	ok := CharacterInput{Name: "柚子君", ImageMediaIDs: ids(1)}
	if is := ValidateCharacter(ok); len(is) != 0 {
		t.Fatalf("合法角色不应有问题：%+v", is)
	}
	edge := CharacterInput{Name: strings.Repeat("柚", 50), ImageMediaIDs: ids(9),
		Appearance: strings.Repeat("😀", 500), VoiceText: strings.Repeat("音", 500), VoiceMediaID: 7}
	if is := ValidateCharacter(edge); len(is) != 0 {
		t.Fatalf("边界值应通过：%+v", is)
	}
	cases := []struct {
		name  string
		in    CharacterInput
		field string
		code  IssueCode
	}{
		{"无图", CharacterInput{Name: "a"}, "imageMediaIds", IssueRequired},
		{"10 张", CharacterInput{Name: "a", ImageMediaIDs: ids(10)}, "imageMediaIds", IssueTooMany},
		{"重复图", CharacterInput{Name: "a", ImageMediaIDs: []int64{5, 5}}, "imageMediaIds[1]", IssueDuplicate},
		{"非法图 ID", CharacterInput{Name: "a", ImageMediaIDs: []int64{0}}, "imageMediaIds[0]", IssueBadValue},
		{"形象 501", CharacterInput{Name: "a", ImageMediaIDs: ids(1), Appearance: strings.Repeat("x", 501)},
			"appearance", IssueTooLong},
		{"音色 501", CharacterInput{Name: "a", ImageMediaIDs: ids(1), VoiceText: strings.Repeat("x", 501)},
			"voiceText", IssueTooLong},
		{"空名", CharacterInput{ImageMediaIDs: ids(1)}, "name", IssueRequired},
		{"音频 ID 非法", CharacterInput{Name: "a", ImageMediaIDs: ids(1), VoiceMediaID: -1}, "voiceMediaId", IssueBadValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := issueFields(ValidateCharacter(c.in))
			if got[c.field] != c.code {
				t.Fatalf("期望 %s=%s，实际 %v", c.field, c.code, got)
			}
		})
	}
	// 一次报完：无图 + 坏名字 + 超长描述同时报。
	all := ValidateCharacter(CharacterInput{Name: "a@b", Appearance: strings.Repeat("x", 501)})
	if f := issueFields(all); f["imageMediaIds"] == "" || f["name"] == "" || f["appearance"] == "" {
		t.Fatalf("应一次报完：%v", f)
	}

	for ct, want := range map[string]bool{"audio/mpeg": true, "audio/wav": true, "AUDIO/OGG; x=1": true,
		"video/mp4": false, "image/png": false, "audio/": false, "": false} {
		if AllowedAudioMime(ct) != want {
			t.Errorf("AllowedAudioMime(%q) 应为 %v", ct, want)
		}
	}
}
