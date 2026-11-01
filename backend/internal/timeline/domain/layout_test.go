package domain

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const layoutFixtureDir = "../../../../api/fixtures/smartcut/layout"

// TestLayout_与fixture一致：每份 fixture 的内容本身合法，算出来的成片时间与期望逐项相等（前端读同一份）。
func TestLayout_与fixture一致(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(layoutFixtureDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 8 {
		t.Fatalf("layout 用例只有 %d 个", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Why     string          `json:"why"`
			Content json.RawMessage `json:"content"`
			Expect  LayoutResult    `json:"expect"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("%s：%v", f, err)
		}
		content, err := DecodeContent(c.Content)
		if err != nil {
			t.Errorf("%s：fixture 的内容本身应当合法：%v", filepath.Base(f), err)
			continue
		}
		got := Layout(content)
		if !reflect.DeepEqual(got, c.Expect) {
			gj, _ := json.Marshal(got)
			ej, _ := json.Marshal(c.Expect)
			t.Errorf("%s（%s）：\n实际 %s\n期望 %s", filepath.Base(f), c.Why, gj, ej)
		}
	}
}

// ---------------------------------------------------------------- 回归：与改动前的三份实现逐一对照

// 改动前 Sequence 的落点：首尾相接、不扣转场重叠。
func oldSequenceStarts(clips []Clip) []int {
	out := make([]int, len(clips))
	at := 0
	for i, c := range clips {
		out[i] = at
		at += c.PlayLen()
	}
	return out
}

// 改动前的 EffectiveDuration。
func oldEffective(clips []Clip) int {
	total := 0
	for i, c := range clips {
		total += c.PlayLen()
		if i > 0 && c.TransitionIn.IsActive() {
			total -= c.TransitionIn.DurationMs
		}
	}
	if total < 0 {
		return 0
	}
	return total
}

// 改动前渲染的 accumulated（xfade offset = accumulated(clips[:i]) − 转场）。
func oldAccumulated(clips []SnapClip) int {
	total := 0
	for i, c := range clips {
		total += c.DurationMs
		if i > 0 && c.Transition.IsActive() {
			total -= c.Transition.DurationMs
		}
	}
	return total
}

func randomVideo(r *rand.Rand, withTransitions bool) []Clip {
	n := r.Intn(9)
	clips := make([]Clip, n)
	speeds := []int{25, 50, 75, 100, 125, 150, 200, 400}
	for i := range clips {
		in := r.Intn(2000)
		clips[i] = Clip{ID: int64(i + 1), MediaID: 1, SourceInMs: in, SourceOutMs: in + 400 + r.Intn(9600),
			Speed: speeds[r.Intn(len(speeds))]}
		if withTransitions && i > 0 && r.Intn(3) > 0 {
			limit := min(clips[i-1].PlayLen(), clips[i].PlayLen()) / 2
			if limit >= MinTransitionMs {
				clips[i].TransitionIn = Transition{Kind: TransitionDissolve,
					DurationMs: MinTransitionMs + r.Intn(limit-MinTransitionMs+1)}
			}
		}
	}
	return clips
}

func TestPlacement_与改动前的实现一致(t *testing.T) {
	r := rand.New(rand.NewSource(20261028))
	for round := 0; round < 500; round++ {
		// 没有转场的时间线：落点与改动前逐个相同。
		plain := randomVideo(r, false)
		seq := Sequence(plain)
		for i, want := range oldSequenceStarts(plain) {
			if seq[i].StartMs != want {
				t.Fatalf("第 %d 轮：没有转场时第 %d 段起点 %d，改动前是 %d", round, i, seq[i].StartMs, want)
			}
		}
		// 有转场：成片总长与改动前相同；xfade offset 与改动前的 accumulated 算法相同。
		withT := randomVideo(r, true)
		if got, want := EffectiveDuration(withT), oldEffective(withT); got != want {
			t.Fatalf("第 %d 轮：成片总长 %d，改动前 %d", round, got, want)
		}
		snaps := make([]SnapClip, len(withT))
		for i, c := range withT {
			snaps[i] = SnapClip{DurationMs: c.PlayLen(), Transition: c.TransitionIn}
		}
		starts, _ := placeVideo(snapSeams(snaps))
		seqT := Sequence(withT)
		for i := 1; i < len(snaps); i++ {
			if !snaps[i].Transition.IsActive() {
				continue
			}
			want := oldAccumulated(snaps[:i]) - snaps[i].Transition.DurationMs
			if starts[i] != want {
				t.Fatalf("第 %d 轮：第 %d 段的 offset %d，改动前 %d", round, i, starts[i], want)
			}
			// 时间线上的落点与渲染的 offset 是同一个数（SCK-15）。
			if seqT[i].StartMs != starts[i] {
				t.Fatalf("第 %d 轮：第 %d 段时间线起点 %d 与渲染 offset %d 不一致", round, i, seqT[i].StartMs, starts[i])
			}
		}
		if got, want := SnapDuration(RenderSnapshot{Clips: snaps}), oldEffective(withT); got != want {
			t.Fatalf("第 %d 轮：快照成片总长 %d，改动前 %d", round, got, want)
		}
	}
}

// ---------------------------------------------------------------- 按起点放置

func TestPlaceByStart_起点不动只重编序号(t *testing.T) {
	clips := []Clip{
		{ID: 1, Idx: 1, StartMs: 5000, SourceOutMs: 2000, Speed: 100},
		{ID: 2, Idx: 2, StartMs: 1000, SourceOutMs: 4000, Speed: 200},
		{ID: 3, Idx: 3, StartMs: 5000, SourceOutMs: 1000, Speed: 100},
	}
	got := PlaceByStart(clips)
	ids := []int64{got[0].ID, got[1].ID, got[2].ID}
	if !reflect.DeepEqual(ids, []int64{2, 1, 3}) {
		t.Fatalf("应按（起点，原 idx）排序：%v", ids)
	}
	for i, c := range got {
		if c.Idx != i+1 {
			t.Errorf("idx 应重编为 %d，实际 %d", i+1, c.Idx)
		}
	}
	if got[0].StartMs != 1000 || got[0].DurationMs != 2000 || got[1].StartMs != 5000 {
		t.Errorf("起点不动、时长按成片长度：%+v", got)
	}
	if clips[0].Idx != 1 {
		t.Error("PlaceByStart 改了传进来的切片")
	}
}

func TestOverlapIndex_报后一段且首尾相接不算(t *testing.T) {
	touching := []Clip{
		{ID: 1, StartMs: 0, SourceOutMs: 1000, Speed: 100},
		{ID: 2, StartMs: 1000, SourceOutMs: 1000, Speed: 100},
	}
	if i := OverlapIndex(touching); i != -1 {
		t.Fatalf("首尾相接不算重叠，实际报 %d", i)
	}
	// 数组里排第一的那段起点更晚：报它（按起点排序后的后一段）。
	overlap := []Clip{
		{ID: 7, StartMs: 1500, SourceOutMs: 1000, Speed: 100},
		{ID: 8, StartMs: 0, SourceOutMs: 4000, Speed: 200},
	}
	if i := OverlapIndex(overlap); i != 0 {
		t.Fatalf("应报起点更晚的那一段（下标 0；另一段占 0~2000），实际 %d", i)
	}
	if end := TrackEnd(overlap); end != 2500 {
		t.Fatalf("最后结束在 2500，实际 %d", end)
	}
	if TrackEnd(nil) != 0 {
		t.Fatal("空轨结束在 0")
	}
}

func TestPlaceAnchored_一半是边界(t *testing.T) {
	clip := Clip{SourceInMs: 1000, SourceOutMs: 3000, Speed: 0}
	stored := SpanPlace{StartMs: 42, EndMs: 99}
	if p := PlaceAnchored(500, 1500, 10000, clip, stored); p.Hidden || p.StartMs != 10000 || p.EndMs != 10500 {
		t.Fatalf("恰好一半可见应显示（speed 0 按原速）：%+v", p)
	}
	if p := PlaceAnchored(499, 1500, 10000, clip, stored); !p.Hidden || p.StartMs != 42 || p.EndMs != 99 {
		t.Fatalf("不到一半应隐藏并按存储值：%+v", p)
	}
	if p := PlaceAnchored(3000, 3500, 10000, clip, stored); !p.Hidden {
		t.Fatalf("完全在取用区间之后应隐藏：%+v", p)
	}
}
