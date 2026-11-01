package domain

import (
	"strings"
	"testing"
)

func shot(no int, id, media int64, dur int, reason string) ShotClip {
	return ShotClip{ShotID: id, ShotNo: no, MediaID: media, DurationMs: dur, Reason: reason}
}

func TestPlanAssemble_按镜号装配(t *testing.T) {
	plan := PlanAssemble([]ShotClip{
		shot(1, 11, 101, 3000, ""),
		shot(2, 12, 102, 4000, ""),
	}, nil, false)

	if len(plan.Append) != 2 {
		t.Fatalf("应装 2 段，实际 %d", len(plan.Append))
	}
	if !plan.Complete() {
		t.Fatalf("没有缺口时应是完整的：%+v", plan.Gaps)
	}
	if plan.Append[0].SourceShotID != 11 || plan.Append[0].SourceOutMs != 3000 {
		t.Fatalf("应记下溯源并取整段：%+v", plan.Append[0])
	}
	if plan.Append[0].Speed != DefaultSpeed || plan.Append[0].Volume != DefaultVolume {
		t.Fatal("新段应带默认变速与音量")
	}
}

// ⚠️ NEW-09 的原文：缺素材或失败镜头**不静默跳过**。
// 少了两镜的成片看起来是完整的，用户要逐镜比对才能发现 ——
// 而那时他可能已经把片子发出去了。
func TestPlanAssemble_缺口要报出来不能静默跳过(t *testing.T) {
	plan := PlanAssemble([]ShotClip{
		shot(1, 11, 101, 3000, ""),
		shot(2, 12, 0, 0, "生成失败：上游拒绝"),
		shot(3, 13, 0, 0, ""),
	}, nil, false)

	if len(plan.Append) != 1 {
		t.Fatalf("只有 1 镜可装，实际 %d", len(plan.Append))
	}
	if len(plan.Gaps) != 2 {
		t.Fatalf("应报 2 个缺口，实际 %d", len(plan.Gaps))
	}
	if plan.Complete() {
		t.Fatal("有缺口时不该说完整")
	}
	// 原因要原样带上，没有原因时给一句能看懂的默认。
	if plan.Gaps[0].Reason != "生成失败：上游拒绝" {
		t.Fatalf("应保留原因：%s", plan.Gaps[0].Reason)
	}
	if plan.Gaps[1].Reason == "" {
		t.Fatal("没有原因时也要给一句话")
	}
	// ⚠️ 缺口要出现在摘要的**第一句**：用户扫一眼只看开头。
	if !strings.HasPrefix(plan.Summary(), "2 个镜头还没有可用视频") {
		t.Fatalf("摘要应先说缺口：%s", plan.Summary())
	}
}

// ⚠️ 时长未知也算缺口，不猜一个数填进去：猜的话成片长度对不上，
// 而"对不上多少"没人说得清。
func TestPlanAssemble_时长未知算缺口(t *testing.T) {
	plan := PlanAssemble([]ShotClip{shot(1, 11, 101, 0, "")}, nil, false)
	if len(plan.Append) != 0 {
		t.Fatal("时长未知不该装进去")
	}
	if len(plan.Gaps) != 1 || !strings.Contains(plan.Gaps[0].Reason, "ffprobe") {
		t.Fatalf("应说清是时长探测的问题：%+v", plan.Gaps)
	}
}

// ⚠️ 只补缺口、不动已有的：用户在上一次装配之后往往已经手工裁过、调过顺序，
// 全删重建等于把他的工作扔掉，而那是不可逆的。
func TestPlanAssemble_已装过的跳过(t *testing.T) {
	existing := []Clip{{ID: 1, Idx: 1, SourceShotID: 11}}
	plan := PlanAssemble([]ShotClip{
		shot(1, 11, 101, 3000, ""),
		shot(2, 12, 102, 4000, ""),
	}, existing, false)

	if len(plan.Append) != 1 || plan.Append[0].SourceShotID != 12 {
		t.Fatalf("只该补第 2 镜：%+v", plan.Append)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0] != 1 {
		t.Fatalf("第 1 镜应记为跳过：%v", plan.Skipped)
	}
	if !strings.Contains(plan.Summary(), "未改动") {
		t.Fatalf("摘要要说清有几个没动：%s", plan.Summary())
	}
}

func TestPlanAssemble_force时重建(t *testing.T) {
	existing := []Clip{{ID: 1, Idx: 1, SourceShotID: 11}}
	plan := PlanAssemble([]ShotClip{shot(1, 11, 101, 3000, "")}, existing, true)
	if len(plan.Append) != 1 {
		t.Fatalf("force 时应重建，实际 %+v", plan.Append)
	}
	if len(plan.Skipped) != 0 {
		t.Fatal("force 时不该有跳过")
	}
}

// ⚠️ 用户手工加的段（SourceShotID == 0）永远不参与"已装过"的判断：
// 否则一条手工片段会把整镜的自动装配挡掉。
func TestPlanAssemble_手工段不算已装过(t *testing.T) {
	existing := []Clip{{ID: 1, Idx: 1, SourceShotID: 0}}
	plan := PlanAssemble([]ShotClip{shot(1, 11, 101, 3000, "")}, existing, false)
	if len(plan.Append) != 1 {
		t.Fatalf("手工段不该挡住自动装配：%+v", plan)
	}
}

func TestSummary_没东西可装时说清楚(t *testing.T) {
	plan := PlanAssemble(nil, nil, false)
	if plan.Summary() != "没有新的镜头可装配" {
		t.Fatalf("实际 %s", plan.Summary())
	}
}
