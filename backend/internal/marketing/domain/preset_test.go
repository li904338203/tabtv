package domain

import (
	"strings"
	"testing"
)

// 三级：任何时候拦 / 草稿告警启用时拦 / 只告警（MKT-F-121）。
func TestValidatePreset_三级(t *testing.T) {
	full := PresetInput{Name: "柚子君", Summary: "圆脸柚子拟人", Appearance: "绿色叶子发型",
		ImageMediaIDs: ids(1)}.Normalized()
	neg := int64(-1)
	cases := []struct {
		name    string
		in      PresetInput
		enable  bool
		errs    map[string]IssueCode // 期望的 error（field → code）
		warns   map[string]IssueCode // 期望的 warn
		noOther bool
	}{
		{name: "完整内容草稿与启用都无问题", in: full, noOther: true},
		{name: "完整内容启用", in: full, enable: true, noOther: true},
		{name: "无图草稿只告警", in: PresetInput{Name: "柚子君", Summary: "s", Appearance: "a"},
			warns: map[string]IssueCode{"imageMediaIds": IssueRequired}, noOther: true},
		{name: "无图启用即拦", in: PresetInput{Name: "柚子君", Summary: "s", Appearance: "a"}, enable: true,
			errs: map[string]IssueCode{"imageMediaIds": IssueRequired}, noOther: true},
		{name: "无说明无形象只告警", in: PresetInput{Name: "柚子君", ImageMediaIDs: ids(1)}, enable: true,
			warns: map[string]IssueCode{"summary": IssueRequired, "appearance": IssueRequired}, noOther: true},
		{name: "名称禁用字符任何时候拦", in: PresetInput{Name: "小@美", Summary: "s", Appearance: "a",
			ImageMediaIDs: ids(1)}, errs: map[string]IssueCode{"name": IssueBadFormat}, noOther: true},
		{name: "全角括号也拦", in: PresetInput{Name: "小美（新）", Summary: "s", Appearance: "a",
			ImageMediaIDs: ids(1)}, errs: map[string]IssueCode{"name": IssueBadFormat}, noOther: true},
		{name: "空名", in: PresetInput{Name: "  ", Summary: "s", Appearance: "a", ImageMediaIDs: ids(1)},
			errs: map[string]IssueCode{"name": IssueRequired}, noOther: true},
		{name: "一次报完：超长 + 超张数 + 重复 + 非法音频", in: PresetInput{
			Name: strings.Repeat("名", MaxCharacterName+1), Summary: strings.Repeat("说", MaxPresetSummary+1),
			Appearance: strings.Repeat("形", MaxAppearance+1), VoiceText: strings.Repeat("音", MaxVoiceText+1),
			ImageMediaIDs: append(ids(MaxCharacterImages), 1000), VoiceMediaID: &neg},
			errs: map[string]IssueCode{"name": IssueTooLong, "summary": IssueTooLong, "appearance": IssueTooLong,
				"voiceText": IssueTooLong, "imageMediaIds": IssueTooMany, "imageMediaIds[9]": IssueDuplicate,
				"voiceMediaId": IssueBadValue}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			is := ValidatePreset(c.in.Normalized(), c.enable)
			gotErr, gotWarn := map[string]IssueCode{}, map[string]IssueCode{}
			for _, i := range is {
				if i.Level == LevelError {
					gotErr[i.Field] = i.Code
				} else {
					gotWarn[i.Field] = i.Code
				}
			}
			for f, code := range c.errs {
				if gotErr[f] != code {
					t.Errorf("error %s 期望 %s，得到 %q（全部 %+v）", f, code, gotErr[f], is)
				}
			}
			for f, code := range c.warns {
				if gotWarn[f] != code {
					t.Errorf("warn %s 期望 %s，得到 %q（全部 %+v）", f, code, gotWarn[f], is)
				}
			}
			if c.noOther && (len(gotErr) != len(c.errs) || len(gotWarn) != len(c.warns)) {
				t.Errorf("多出了问题：%+v", is)
			}
		})
	}
}

// 名称码点计数：emoji 与汉字都按 1 计（50 个汉字合法，51 个超长）。
func TestCharacterNameIssue_码点与禁用字符(t *testing.T) {
	if is := CharacterNameIssue("name", strings.Repeat("汉", MaxCharacterName)); is != nil {
		t.Fatalf("50 个汉字应合法：%+v", is)
	}
	if is := CharacterNameIssue("name", strings.Repeat("😀", MaxCharacterName)); is != nil {
		t.Fatalf("50 个 emoji 应合法：%+v", is)
	}
	for _, r := range CharacterNameForbidden {
		if is := CharacterNameIssue("name", "小"+string(r)+"美"); len(is) != 1 || is[0].Code != IssueBadFormat {
			t.Errorf("「%c」应被拒：%+v", r, is)
		}
	}
}
