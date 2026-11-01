package domain

import (
	"fmt"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 确定性兜底（SC-F-85、SC-AC-32）：模型输出修一次仍不合规时退回规则方案，时间线不会被清空。
//
//   - 素材混剪：按编号顺序拼接、按目标时长从尾部截掉（不卡点、不加标题）；
//   - 分镜成片：照装配硬切，打开字幕时铺台词；
//   - 口播精剪：保留全部句子，只剪超过 600ms 的停顿与口癖；
//   - 解说成片：按句均分画面。
//
// 结果都带 UsedFallback 与固定说明「AI 没能给出方案，已按规则剪好」。

// Fallback 兜底方案。
func Fallback(in PlanInput) (Result, error) {
	switch in.Mode {
	case ModeMix:
		return fallbackMix(in)
	case ModeBoard:
		return compileBoard(in, Draft{Mode: ModeBoard}, true)
	case ModeTalk:
		return compileTalk(in, Draft{Mode: ModeTalk}, true)
	case ModeNarr:
		return compileNarr(in, Draft{Mode: ModeNarr}, true)
	}
	return Result{}, fmt.Errorf("不支持的剪辑方式 %q", in.Mode)
}

// fallbackMix 按编号顺序拼接整段素材（图片 3 秒），超过目标时长时从尾部截掉。
func fallbackMix(in PlanInput) (Result, error) {
	var cuts []cut
	at := 0
	for _, c := range in.Cards {
		if !c.IsPicture() {
			continue
		}
		l := c.DurationMs
		if c.Kind == InputImage {
			l = ImageDefaultMs
		}
		if t := in.Spec.TargetMs; t > 0 {
			if at >= t {
				break
			}
			// 从尾部截：最后一段截到正好够；截下来不到 100ms 的就不要了。
			l = min(l, t-at)
			if l < tl.MinClipMs {
				break
			}
		}
		cuts = append(cuts, cut{card: c, inMs: 0, outMs: l, speed: tl.DefaultSpeed})
		at += l
	}
	if len(cuts) == 0 {
		return Result{}, &ErrUnsuitable{Reason: UnsuitableNoPicture, Detail: "没有可用的画面素材"}
	}
	_, total := placeCuts(cuts)
	res := Result{UsedFallback: true, Notes: []string{FallbackNote}}
	a := arrangement{}
	for k, c := range cuts {
		a.video = append(a.video, c.clipOf(videoClipID(k)))
	}
	if music, ok, _ := pickMusic(in.Cards, ""); ok {
		clip, warn := musicClip(music, total)
		a.music = clip
		if warn != "" {
			res.Warnings = append(res.Warnings, warn)
		}
	}
	if t := in.Spec.TargetMs; t > 0 && total < t {
		res.Warnings = append(res.Warnings, fmt.Sprintf("素材共 %s 秒，不足目标 %s 秒，已按 %s 秒剪", secText(total), secText(t), secText(total)))
		res.Shortage = &Shortage{TotalMs: total, TargetMs: t}
	}
	used := usedOrder(cuts)
	if w := unusedPictureWarning(in.Cards, used); w != "" {
		res.Warnings = append(res.Warnings, w)
	}
	res.UsedClips = used
	return finish(in, a, cuts, res), nil
}
