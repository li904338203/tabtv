package app

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
)

// 给模型的 user 消息（SCD-25）：要求、规格、素材卡片、镜头 / 句子、拼图说明都放在这里；
// 系统提示词只写创作要求与输出格式（上下文进 user 消息，CLAUDE.md §5.7）。

// maxDescriptionRunes 一条素材描述最多带多少字（生成提示词可能很长）。
const maxDescriptionRunes = 300

var modeNames = map[string]string{
	domain.ModeMix: "素材混剪", domain.ModeBoard: "分镜成片", domain.ModeTalk: "口播精剪", domain.ModeNarr: "解说成片",
}

// defaultInstructions 用户没写要求时的一句（不留空：空要求会让模型反问）。
var defaultInstructions = map[string]string{
	domain.ModeMix:   "（没有写要求）按素材剪一条节奏紧凑、开头抓人的短视频。",
	domain.ModeBoard: "（没有写要求）按镜头清单做好标题、转场与台词字幕。",
	domain.ModeTalk:  "（没有写要求）删掉重复说的与说错的句子，保留完整的句子。",
	domain.ModeNarr:  "（没有写要求）给每一句配上最贴切的画面。",
}

func planUserMessage(req PlanRequest, f planFacts, mosaics []Mosaic, k, n int) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("## 剪辑要求\n\n")
	if req.Instruction != "" {
		w("%s\n\n", req.Instruction)
	} else {
		w("%s\n\n", defaultInstructions[req.Mode])
	}
	w("## 规格\n\n")
	w("- 剪辑方式：%s\n", modeNames[req.Mode])
	switch {
	case req.Mode == domain.ModeNarr || req.Mode == domain.ModeTalk:
		w("- 时长：以%s为准\n", map[string]string{domain.ModeNarr: "配音", domain.ModeTalk: "保留下来的句子"}[req.Mode])
	case req.Mode == domain.ModeBoard:
		w("- 时长：以镜头时长为准\n")
	case req.TargetSec > 0:
		w("- 目标时长：%d 秒\n", req.TargetSec)
	default:
		w("- 目标时长：自动（按素材定）\n")
	}
	if req.AspectRatio != "" && req.AspectRatio != "follow" {
		w("- 画幅：%s\n", req.AspectRatio)
	}
	if f.in.Spec.WithSubtitles {
		w("- 字幕：要\n")
	} else {
		w("- 字幕：不要\n")
	}
	if req.Mode == domain.ModeMix {
		w("- %s\n", beatLine(f, req.IgnoreBeat))
	}
	if req.AllowSlow || req.AllowRepeat {
		var parts []string
		if req.AllowSlow {
			parts = append(parts, "允许放慢（最低 0.75 倍）")
		}
		if req.AllowRepeat {
			parts = append(parts, "允许重复使用片段")
		}
		w("- 素材不够时：%s\n", strings.Join(parts, "、"))
	}
	w("\n")

	switch req.Mode {
	case domain.ModeBoard:
		w("## 镜头清单（顺序与每个镜头取用的部分已经装配好，不能改）\n\n")
		for _, sh := range f.in.Shots {
			w("- %s：第 %d 镜", sh.Ref, sh.ShotNo)
			if sh.Source != "" {
				w("（%s）", sh.Source)
			}
			w("，%s 秒。", secs(sh.SourceOutMs-sh.SourceInMs))
			if d := strings.TrimSpace(sh.Description); d != "" {
				w("画面：%s。", clip(d))
			}
			if d := strings.TrimSpace(sh.Dialogue); d != "" {
				w("台词：%s", clip(d))
			}
			w("\n")
		}
		if audio := cardLines(f, func(c domain.Card) bool { return c.Kind == domain.InputAudio }, nil); audio != "" {
			w("\n## 可用的配乐\n\n%s", audio)
		}
	case domain.ModeTalk:
		w("## 口播识别稿（按句编号）\n\n")
		for _, s := range f.sentences {
			w("- %s（%s~%s 秒）：%s\n", s.Ref, secs(s.StartMs), secs(s.EndMs), clip(s.Text))
		}
	case domain.ModeNarr:
		w("## 配音稿（按句编号）\n\n")
		for _, s := range f.sentences {
			w("- %s（%s~%s 秒，共 %s 秒）：%s\n", s.Ref, secs(s.StartMs), secs(s.EndMs), secs(s.EndMs-s.StartMs), s.Text)
		}
		w("\n## 素材清单\n\n%s", cardLines(f, func(c domain.Card) bool { return c.IsPicture() }, mosaics))
		if audio := cardLines(f, func(c domain.Card) bool { return c.Kind == domain.InputAudio && c.AudioRole == domain.RoleMusic }, nil); audio != "" {
			w("\n## 可用的配乐\n\n%s", audio)
		}
	default:
		w("## 素材清单\n\n%s", cardLines(f, func(c domain.Card) bool { return c.Kind != domain.InputText }, mosaics))
	}
	if len(mosaics) > 0 {
		w("\n## 缩略图拼图\n\n共 %d 张，随消息附上，按顺序是拼图 1 到拼图 %d。每张三行，每行是一段素材的开头、中间、结尾三帧（图片只有一格）：\n",
			len(mosaics), len(mosaics))
		for i, m := range mosaics {
			var rows []string
			for r, no := range m.Rows {
				rows = append(rows, fmt.Sprintf("第 %d 行是编号 %s", r+1, no))
			}
			w("- 拼图 %d：%s\n", i+1, strings.Join(rows, "，"))
		}
		w("请在 clipNotes 里用一句话描述你看到的每段素材的画面。\n")
	}
	if n > 1 {
		w("\n## 方案\n\n这是第 %d 套剪法（共 %d 套）：与另外几套在选段、顺序或节奏上要有明显区别。\n", k+1, n)
	}
	return strings.TrimRight(b.String(), "\n")
}

// cardLines 素材卡片：编号、种类、时长、声音与描述；没有描述、在拼图里的写明看哪一张哪一行。
func cardLines(f planFacts, pick func(domain.Card) bool, mosaics []Mosaic) string {
	where := map[string]string{}
	for i, m := range mosaics {
		for r, no := range m.Rows {
			where[no] = fmt.Sprintf("见拼图 %d 第 %d 行", i+1, r+1)
		}
	}
	var b strings.Builder
	for _, c := range f.in.Cards {
		if !pick(c) {
			continue
		}
		fmt.Fprintf(&b, "- 编号 %s：", c.No)
		switch c.Kind {
		case domain.InputVideo:
			fmt.Fprintf(&b, "视频，%s 秒", secs(c.DurationMs))
			if c.HasAudio {
				b.WriteString("，有声音")
			}
		case domain.InputImage:
			b.WriteString("图片")
		case domain.InputAudio:
			role := "配乐"
			if c.AudioRole == domain.RoleVoice {
				role = "人声"
			}
			fmt.Fprintf(&b, "音频（%s），%s 秒", role, secs(c.DurationMs))
			if bpm := bpmOf(f.in.Beats[c.No]); bpm > 0 {
				fmt.Fprintf(&b, "，约 %d BPM", bpm)
			}
		case domain.InputText:
			fmt.Fprintf(&b, "文本：%s", clip(c.Text))
		}
		if c.Title != "" {
			fmt.Fprintf(&b, "。标题：%s", clip(c.Title))
		}
		switch {
		case c.Description != "":
			fmt.Fprintf(&b, "。描述：%s", clip(c.Description))
		case where[c.No] != "":
			fmt.Fprintf(&b, "。画面：%s", where[c.No])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// beatLine 卡点说明：有配乐时说明会按节拍对齐（时间由系统算，模型只管选段与节奏）。
func beatLine(f planFacts, ignore bool) string {
	if ignore {
		return "卡点：不卡点"
	}
	for _, c := range f.in.Cards {
		if c.Kind == domain.InputAudio && c.AudioRole == domain.RoleMusic {
			if bpm := bpmOf(f.in.Beats[c.No]); bpm > 0 {
				return fmt.Sprintf("卡点：配乐编号 %s，约 %d BPM，切换点会自动对齐到节拍上", c.No, bpm)
			}
			return "卡点：配乐编号 " + c.No + "（节拍还没分析出来，这一版不卡点）"
		}
	}
	return "卡点：没有配乐"
}

// bpmOf 拍距中位数折算的 BPM（0 = 没有节拍）。
func bpmOf(b domain.Beats) int {
	if len(b.BeatsMs) < 2 {
		return 0
	}
	beats := append([]int(nil), b.BeatsMs...)
	sort.Ints(beats)
	gaps := make([]int, 0, len(beats)-1)
	for i := 1; i < len(beats); i++ {
		if g := beats[i] - beats[i-1]; g > 0 {
			gaps = append(gaps, g)
		}
	}
	if len(gaps) == 0 {
		return 0
	}
	sort.Ints(gaps)
	return (60000 + gaps[len(gaps)/2]/2) / gaps[len(gaps)/2]
}

// secs 毫秒 → 「4.1」（一位小数，整秒不写 .0）。
func secs(ms int) string {
	if ms < 0 {
		ms = 0
	}
	t := (ms + 50) / 100
	if t%10 == 0 {
		return strconv.Itoa(t / 10)
	}
	return strconv.Itoa(t/10) + "." + strconv.Itoa(t%10)
}

func clip(s string) string {
	return headText(strings.Join(strings.Fields(s), " "), maxDescriptionRunes)
}
