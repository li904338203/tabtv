package domain

import (
	"reflect"
	"testing"
)

func intp(v int) *int { return &v }

// 2026-10-24 修订（会话 107419662995685376）：一支视频一次生成；只有上限已知且总时长超过它才是多段，
// 而且多段要用户决定。「不支持多镜头」与「上限没配」都不再触发拆段。
func TestDecideSegments_只有超过已知上限才是多段(t *testing.T) {
	cases := []struct {
		name  string
		total int
		caps  ModelCaps
		want  Segmentation
	}{
		{"恰好等于上限", 15, ModelCaps{MaxDurationSec: intp(15), MultiShot: true}, SegmentSingle},
		{"低于上限", 8, ModelCaps{MaxDurationSec: intp(15), MultiShot: true}, SegmentSingle},
		{"超过上限一秒", 16, ModelCaps{MaxDurationSec: intp(15), MultiShot: true}, SegmentMulti},
		// 会话 107419662995685376：上限 30 秒、目录标着不支持多镜头，15 秒的视频被拆成两段。
		{"不支持多镜头也不拆", 15, ModelCaps{MaxDurationSec: intp(30), MultiShot: false}, SegmentSingle},
		{"不支持多镜头且超上限", 40, ModelCaps{MaxDurationSec: intp(30)}, SegmentMulti},
		// 改目录里的最长时长，分流随之变化（无代码改动）。
		{"目录上限放到 30 秒", 25, ModelCaps{MaxDurationSec: intp(30), MultiShot: true}, SegmentSingle},
		{"目录上限收到 10 秒", 12, ModelCaps{MaxDurationSec: intp(10), MultiShot: true}, SegmentMulti},
		{"总时长未知", 0, ModelCaps{MaxDurationSec: intp(15), MultiShot: true}, SegmentSingle},
	}
	for _, c := range cases {
		if got := DecideSegments(c.total, c.caps); got != c.want {
			t.Errorf("%s：得到 %s，期望 %s", c.name, got, c.want)
		}
	}
}

func TestDecideSegments_上限没配时按单段(t *testing.T) {
	for _, caps := range []ModelCaps{
		{MaxDurationSec: nil, MultiShot: true},
		{MaxDurationSec: intp(0), MultiShot: true},
		{},
	} {
		if got := DecideSegments(40, caps); got != SegmentSingle {
			t.Errorf("caps=%+v 上限不知道时先按单段（越界由报价拦），得到 %s", caps, got)
		}
		if _, ok := SingleSegmentLimit(caps); ok {
			t.Errorf("caps=%+v 不应有单段上限", caps)
		}
	}
	if limit, ok := SingleSegmentLimit(ModelCaps{MaxDurationSec: intp(30)}); !ok || limit != 30 {
		t.Errorf("上限只看目录的单次最长时长，与多镜头无关：%d %v", limit, ok)
	}
}

func TestSplitShots_每段不超过上限且总和等于总时长(t *testing.T) {
	for total := 1; total <= 120; total++ {
		for _, max := range []int{4, 5, 10, 15} {
			segs := SplitShots(total, max)
			sum := 0
			for _, s := range segs {
				if s <= 0 || s > max {
					t.Fatalf("total=%d max=%d 出现非法段 %v", total, max, segs)
				}
				sum += s
			}
			if sum != total {
				t.Fatalf("total=%d max=%d 段之和 %d", total, max, sum)
			}
			want := (total + max - 1) / max
			if len(segs) != want {
				t.Fatalf("total=%d max=%d 段数 %d，期望最少的 %d", total, max, len(segs), want)
			}
		}
	}
	if got := SplitShots(40, 15); !reflect.DeepEqual(got, []int{14, 13, 13}) {
		t.Errorf("40/15 应均匀切成 14/13/13，得到 %v", got)
	}
	if SplitShots(0, 15) != nil {
		t.Error("总时长 0 应返回 nil")
	}
	if got := SplitShots(40, 0); !reflect.DeepEqual(got, []int{40}) {
		t.Errorf("上限未知时整段一段，得到 %v", got)
	}
}
