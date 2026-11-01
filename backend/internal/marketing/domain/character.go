package domain

import (
	"fmt"
	"strings"
)

// 角色库（MKT-T-34；02 MK-23、MKT-F-103~105、MKT-D-51；dev/05 MKD-33、MKD-36）。
//
// ⚠️ 名称规则（CharacterNameForbidden / CharacterNameIssue）在 library.go，与营销角色（MKT-T-36）共用一处。
// ⚠️ 本文件只管「字段本身合不合法」；图片 / 音频的**归属与格式**要查媒体域，在 app 层判
// （AllowedImageMime / AllowedAudioMime 是那一步用的白名单）。
//
// （卡片写的是并入 library.go；拆成独立文件是因为 MKT-T-36 同时在改 library.go。）

// AllowedAudioMime 判断一个 content_type 能不能当参考音频（按媒体库记录的、文件头嗅探出的
// content_type；只看主类型 audio/*，容忍参数与大小写）。具体收哪几种由上传接口的嗅探决定
// （media/domain/sniff.go：mp3 / wav / ogg），这里不再抄一份子类型表。
//
// ⚠️ 只认 audio/*：视频文件（video/mp4、video/webm）里也有声音，但当成「参考音频」存进来的话，
// 界面上的播放器与上游的预期都对不上。
func AllowedAudioMime(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return strings.HasPrefix(ct, "audio/") && len(ct) > len("audio/")
}

// CharacterInput 是新建 / 整份更新工作区角色时的内容（ID 已由调用方解析成 int64）。
type CharacterInput struct {
	Name string
	// ImageMediaIDs 有序，[0] 为主图；1~MaxCharacterImages。
	ImageMediaIDs []int64
	Appearance    string
	// StyleCode 最近一次生成用的画风（风格素材 code），可空；只用来回填下拉，不参与计算。
	StyleCode string
	VoiceText string
	// VoiceMediaID 参考音频；0 = 没有。只存不发（MKD-33）。
	VoiceMediaID int64
}

// Normalized 去掉名称与画风两端空白（描述原样保留：换行可能是用户有意写的）。
func (c CharacterInput) Normalized() CharacterInput {
	c.Name = strings.TrimSpace(c.Name)
	c.StyleCode = strings.TrimSpace(c.StyleCode)
	return c
}

// maxStyleCode 与 marketing_character.style_code 的列宽一致。
const maxStyleCode = 64

// ValidateCharacter 校验角色内容（MKT-F-103~105、MKT-AC-36），**一次报完**。调用前先 Normalized()。
//
// 张数 1~MaxCharacterImages、图片不重复、名称规则、形象 / 音色描述长度。
// 参考音频「≤ 1 段」由形状保证（voiceMediaId 是单值，传数组在解码时就被拒）。
func ValidateCharacter(in CharacterInput) []Issue {
	var out []Issue
	switch n := len(in.ImageMediaIDs); {
	case n == 0:
		out = append(out, errIssue("imageMediaIds", IssueRequired, "请至少添加 1 张角色图片"))
	case n > MaxCharacterImages:
		out = append(out, errIssue("imageMediaIds", IssueTooMany,
			fmt.Sprintf("角色图片最多 %d 张，当前 %d 张", MaxCharacterImages, n)))
	}
	seen := make(map[int64]int, len(in.ImageMediaIDs))
	for i, id := range in.ImageMediaIDs {
		field := fmt.Sprintf("imageMediaIds[%d]", i)
		if id <= 0 {
			out = append(out, errIssue(field, IssueBadValue, fmt.Sprintf("第 %d 张图片的 ID 非法", i+1)))
			continue
		}
		if j, dup := seen[id]; dup {
			out = append(out, errIssue(field, IssueDuplicate,
				fmt.Sprintf("第 %d 张与第 %d 张是同一张图片", i+1, j+1)))
			continue
		}
		seen[id] = i
	}
	out = append(out, CharacterNameIssue("name", in.Name)...)
	if n := RuneLen(in.Appearance); n > MaxAppearance {
		out = append(out, tooLong("appearance", "形象描述", n, MaxAppearance))
	}
	if n := RuneLen(in.VoiceText); n > MaxVoiceText {
		out = append(out, tooLong("voiceText", "音色描述", n, MaxVoiceText))
	}
	if n := len(in.StyleCode); n > maxStyleCode {
		out = append(out, errIssue("styleCode", IssueTooLong, fmt.Sprintf("画风 code 最多 %d 个字符", maxStyleCode)))
	}
	if in.VoiceMediaID < 0 {
		out = append(out, errIssue("voiceMediaId", IssueBadValue, "参考音频的 ID 非法"))
	}
	return out
}
