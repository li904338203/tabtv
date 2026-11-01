package domain

import "fmt"

// 按分镜自动装配（NEW-07、NEW-09）。

// ShotClip 是脚本域给出的"这一镜有没有可用成片"。
type ShotClip struct {
	ShotID  int64
	ShotNo  int
	MediaID int64
	// DurationMs 素材时长；0 表示未知（ffprobe 认不出）。
	DurationMs int
	// Reason 在 MediaID == 0 时说明为什么没有。
	Reason string
}

// AssembleGap 是一处缺口。
type AssembleGap struct {
	ShotNo int    `json:"shotNo"`
	Reason string `json:"reason"`
}

// AssemblePlan 是一次装配的计划。
type AssemblePlan struct {
	// Append 要新加进轨道的片段（已按镜号排好）。
	Append []Clip
	// Skipped 已经装过、这次不动的镜号。
	Skipped []int
	// Gaps 缺素材或失败的镜头。
	//
	// ⚠️ **不能静默跳过**（NEW-09 的原文）。少了两镜的成片看起来是完整的，
	// 用户要逐镜比对才能发现 —— 而那时他可能已经把片子发出去了。
	Gaps []AssembleGap
}

// Complete 这次装配是不是一个完整的片子。
func (p AssemblePlan) Complete() bool { return len(p.Gaps) == 0 }

// Summary 一句话说明，给界面用。
func (p AssemblePlan) Summary() string {
	switch {
	case len(p.Append) == 0 && len(p.Gaps) == 0:
		return "没有新的镜头可装配"
	case len(p.Gaps) == 0 && len(p.Skipped) == 0:
		return fmt.Sprintf("装配 %d 个镜头", len(p.Append))
	case len(p.Gaps) == 0:
		return fmt.Sprintf("装配 %d 个镜头，%d 个已在时间线上未改动",
			len(p.Append), len(p.Skipped))
	default:
		// ⚠️ 缺口要出现在**第一句**里，不能塞在末尾：用户扫一眼只看开头。
		return fmt.Sprintf("%d 个镜头还没有可用视频；本次装配 %d 个",
			len(p.Gaps), len(p.Append))
	}
}

// PlanAssemble 算出这次要往轨道上加哪些段。
//
// ⚠️ **只补缺口，不动已有的**：用户在上一次装配之后往往已经手工裁过、
// 调过顺序。全删重建等于把他的工作扔掉，而那是不可逆的。
// 判据是 `source_shot_id` —— 这一镜已经有自动装配出来的段就跳过。
//
// ⚠️ `force` 为真时才重建，且重建的也只是**自动装配出来的**那些段：
// 用户手工加的（source_shot_id 为 0）永远不动。
func PlanAssemble(shots []ShotClip, existing []Clip, force bool) AssemblePlan {
	have := map[int64]bool{}
	if !force {
		for _, c := range existing {
			if c.SourceShotID != 0 {
				have[c.SourceShotID] = true
			}
		}
	}

	var plan AssemblePlan
	for _, s := range shots {
		if have[s.ShotID] {
			plan.Skipped = append(plan.Skipped, s.ShotNo)
			continue
		}
		if s.MediaID == 0 {
			reason := s.Reason
			if reason == "" {
				reason = "还没有生成视频"
			}
			plan.Gaps = append(plan.Gaps, AssembleGap{ShotNo: s.ShotNo, Reason: reason})
			continue
		}
		// ⚠️ 时长未知**也算缺口**，不猜一个数填进去。
		//
		// 猜的话成片长度会对不上，而"对不上多少"没人说得清 ——
		// 用镜头脚本里的计划时长更糟：那是"打算拍多久"，
		// 而生成出来的视频实际多长是上游说了算的。
		//
		// 所以这里明说原因（多半是 ffprobe 没装或认不出这个封装），
		// 用户看到的是一句能照着修的话，而不是一段莫名其妙的成片。
		if s.DurationMs <= 0 {
			plan.Gaps = append(plan.Gaps, AssembleGap{
				ShotNo: s.ShotNo,
				Reason: "素材时长未知，无法确定取用区间（检查 ffprobe 是否可用）",
			})
			continue
		}
		plan.Append = append(plan.Append, Clip{
			MediaID: s.MediaID, SourceInMs: 0, SourceOutMs: s.DurationMs,
			Speed: DefaultSpeed, Volume: DefaultVolume,
			SourceShotID: s.ShotID,
		})
	}
	return plan
}

// ErrNothingToAssemble 供调用方区分"全都装过了"与"一个镜头都没有"。
var ErrNothingToAssemble = fmt.Errorf("没有可装配的镜头")
