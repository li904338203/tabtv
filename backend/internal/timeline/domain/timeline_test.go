package domain

import (
	"strings"
	"testing"
)

func clip(id int64, idx, in, out int) Clip {
	return Clip{ID: id, Idx: idx, MediaID: 100 + id, SourceInMs: in, SourceOutMs: out,
		Speed: DefaultSpeed, Volume: DefaultVolume}
}

// ⚠️ 视频轨的 StartMs 必须由服务端算：信客户端的话成片里会出现黑帧
// （有空隙）或画面重叠（有交叠），而时间线界面上看着是连续的。
func TestReflow_首尾相接并重编号(t *testing.T) {
	// 故意给乱的 Idx 与错的 StartMs。
	got := Reflow([]Clip{
		{ID: 2, Idx: 5, SourceInMs: 0, SourceOutMs: 3000, StartMs: 9999, Speed: 100},
		{ID: 1, Idx: 2, SourceInMs: 500, SourceOutMs: 2500, StartMs: 7777, Speed: 100},
	})
	if got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("应按 Idx 升序：%+v", got)
	}
	if got[0].Idx != 1 || got[1].Idx != 2 {
		t.Fatalf("Idx 应重编成 1..N：%d %d", got[0].Idx, got[1].Idx)
	}
	if got[0].StartMs != 0 {
		t.Fatalf("第一段应从 0 起，实际 %d", got[0].StartMs)
	}
	if got[1].StartMs != 2000 {
		t.Fatalf("第二段应接在 2000ms，实际 %d", got[1].StartMs)
	}
	if got[1].DurationMs != 3000 {
		t.Fatalf("时长应按取用区间算，实际 %d", got[1].DurationMs)
	}
}

// ⚠️ 变速之后时间线时长 ≠ 素材时长：2 倍速的 10 秒素材只占 5 秒。
// 直接拿素材长度当时长的话，后面每一段的落点都错，画面看上去只是"对不上音"。
func TestReflow_变速要算进落点(t *testing.T) {
	fast := clip(1, 1, 0, 4000)
	fast.Speed = 200 // 2 倍速
	slow := clip(2, 2, 0, 1000)
	got := Reflow([]Clip{fast, slow})

	if got[0].DurationMs != 2000 {
		t.Fatalf("2 倍速的 4 秒应占 2 秒，实际 %d", got[0].DurationMs)
	}
	if got[1].StartMs != 2000 {
		t.Fatalf("第二段应接在 2000ms，实际 %d", got[1].StartMs)
	}
	if TotalDuration(got) != 3000 {
		t.Fatalf("总时长应为 3000，实际 %d", TotalDuration(got))
	}
}

func TestReflow_空轨道不崩(t *testing.T) {
	if got := Reflow(nil); len(got) != 0 {
		t.Fatalf("空轨道应回空：%+v", got)
	}
	if TotalDuration(nil) != 0 {
		t.Fatal("空轨道总时长应为 0")
	}
}

func TestReorder_移动并重排(t *testing.T) {
	clips := []Clip{clip(1, 1, 0, 1000), clip(2, 2, 0, 2000), clip(3, 3, 0, 3000)}

	got, err := Reorder(clips, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != 3 || got[1].ID != 1 || got[2].ID != 2 {
		t.Fatalf("第 3 段应移到最前：%v", ids(got))
	}
	// 落点跟着重算。
	if got[0].StartMs != 0 || got[1].StartMs != 3000 || got[2].StartMs != 4000 {
		t.Fatalf("落点应重算：%+v", starts(got))
	}
}

// ⚠️ 位置越界夹回范围：前端传 0 或 999 不该报错 —— 那不是用户能理解的错误，
// 夹回去就是他想要的效果（拖到最前/最后）。
func TestReorder_越界夹回范围(t *testing.T) {
	clips := []Clip{clip(1, 1, 0, 1000), clip(2, 2, 0, 2000)}

	first, _ := Reorder(clips, 2, 0)
	if first[0].ID != 2 {
		t.Fatalf("位置 0 应当成最前：%v", ids(first))
	}
	last, _ := Reorder(clips, 1, 999)
	if last[1].ID != 1 {
		t.Fatalf("超界应当成最后：%v", ids(last))
	}
}

func TestReorder_不在轨上的段报错(t *testing.T) {
	if _, err := Reorder([]Clip{clip(1, 1, 0, 1000)}, 99, 1); err == nil {
		t.Fatal("应报错")
	}
}

func TestValidateClip(t *testing.T) {
	ok := clip(1, 1, 0, 5000)
	if err := ValidateClip(ok, 10000); err != nil {
		t.Fatalf("正常片段不该被拒：%v", err)
	}
	// ⚠️ 超出素材本身的后果是渲染出一段黑屏，而任务是"成功"的。
	over := clip(1, 1, 0, 20000)
	err := ValidateClip(over, 10000)
	if err == nil || !strings.Contains(err.Error(), "超出素材") {
		t.Fatalf("应拒绝超界并说清两个数字：%v", err)
	}
	// ⚠️ 素材时长未知时放行：拿不到数字就没法比较。
	if err := ValidateClip(over, 0); err != nil {
		t.Fatalf("时长未知时应放行：%v", err)
	}
	// ⚠️ 比一帧还短的片段在 concat 里会被丢掉，成片比时间线短。
	tiny := clip(1, 1, 0, 50)
	if err := ValidateClip(tiny, 10000); err == nil {
		t.Fatal("过短的片段应被拒")
	}
	// ⚠️ 淡入+淡出超过片段本身会被 afade 压成静音。
	loud := clip(1, 1, 0, 1000)
	loud.FadeInMs, loud.FadeOutMs = 600, 600
	if err := ValidateClip(loud, 10000); err == nil {
		t.Fatal("淡入淡出超长应被拒")
	}
	bad := clip(1, 1, 0, 5000)
	bad.Speed = 1000
	if err := ValidateClip(bad, 10000); err == nil {
		t.Fatal("超范围的变速应被拒")
	}
}

// ⚠️ 裁切改的是**素材内的取用区间**，落点由 Reflow 重算 ——
// 两件事混在一起的话，裁短第 2 段之后第 3 段还停在原处，成片多一段黑屏。
func TestTrim_只改取用区间(t *testing.T) {
	c := clip(1, 1, 0, 5000)
	c.StartMs = 12345
	got, err := Trim(c, 1000, 3000, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceInMs != 1000 || got.SourceOutMs != 3000 {
		t.Fatalf("取用区间应更新：%+v", got)
	}
	if got.DurationMs != 2000 {
		t.Fatalf("时长应跟着变，实际 %d", got.DurationMs)
	}
	// 落点这一步不动 —— 它归 Reflow 管。
	if got.StartMs != 12345 {
		t.Fatalf("Trim 不该改落点，实际 %d", got.StartMs)
	}
}

func TestTrim_超界被拒(t *testing.T) {
	if _, err := Trim(clip(1, 1, 0, 5000), 0, 99999, 10000); err == nil {
		t.Fatal("裁到素材外应被拒")
	}
}

// ⚠️ 切点换算回素材坐标时必须带上变速：2 倍速时时间线上的 1 秒
// 对应素材里的 2 秒。不换算的话接缝处跳帧，而两段各自看都是好的。
func TestSplit_切点要按变速换算(t *testing.T) {
	c := clip(1, 1, 1000, 5000) // 素材 1000~5000
	c.Speed = 200               // 2 倍速 → 时间线上占 2000ms

	left, right, err := Split(c, 500) // 时间线上的 500ms
	if err != nil {
		t.Fatal(err)
	}
	// 素材坐标：1000 + 500*2 = 2000
	if left.SourceOutMs != 2000 || right.SourceInMs != 2000 {
		t.Fatalf("切点应换算成素材内的 2000：左 %d 右 %d",
			left.SourceOutMs, right.SourceInMs)
	}
	// 两段加起来还是原来那么长。
	if left.PlayLen()+right.PlayLen() != c.PlayLen() {
		t.Fatalf("切开前后总时长应相等：%d + %d ≠ %d",
			left.PlayLen(), right.PlayLen(), c.PlayLen())
	}
}

func TestSplit_接缝处不留淡入淡出(t *testing.T) {
	c := clip(1, 1, 0, 4000)
	c.FadeInMs, c.FadeOutMs = 200, 300
	left, right, err := Split(c, 2000)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 都带上的话接缝处会"淡出再淡入"，而用户只是切了一刀。
	if left.FadeOutMs != 0 {
		t.Fatalf("左半段不该留淡出，实际 %d", left.FadeOutMs)
	}
	if right.FadeInMs != 0 {
		t.Fatalf("右半段不该留淡入，实际 %d", right.FadeInMs)
	}
	if left.FadeInMs != 200 || right.FadeOutMs != 300 {
		t.Fatal("外侧的淡入淡出应保留")
	}
}

// ⚠️ 右半段不继承溯源：它不再是"第 3 镜自动装配出来的那一段" ——
// 继承的话唯一约束会撞车，而且"只补缺口"会误判成已有。
func TestSplit_右半段不继承溯源(t *testing.T) {
	c := clip(1, 1, 0, 4000)
	c.SourceShotID = 77
	left, right, _ := Split(c, 2000)
	if left.SourceShotID != 77 {
		t.Fatal("左半段应保留溯源")
	}
	if right.SourceShotID != 0 {
		t.Fatalf("右半段不该继承溯源，实际 %d", right.SourceShotID)
	}
	if right.ID != 0 {
		t.Fatal("右半段应是新段，ID 由仓储分配")
	}
}

func TestSplit_切点太靠边被拒(t *testing.T) {
	c := clip(1, 1, 0, 4000)
	for _, at := range []int{0, 10, 3995, 4000} {
		if _, _, err := Split(c, at); err == nil {
			t.Fatalf("切点 %d 应被拒", at)
		}
	}
}

func TestInsertAfter(t *testing.T) {
	clips := []Clip{clip(1, 1, 0, 1000), clip(2, 2, 0, 2000)}
	fresh := clip(3, 0, 0, 500)

	got := InsertAfter(clips, 1, fresh)
	if got[1].ID != 3 {
		t.Fatalf("应插在第 1 段之后：%v", ids(got))
	}
	if got[1].StartMs != 1000 || got[2].StartMs != 1500 {
		t.Fatalf("落点应重算：%v", starts(got))
	}
	// 找不到锚点时追加到末尾，而不是报错或丢掉。
	tail := InsertAfter(clips, 999, fresh)
	if tail[len(tail)-1].ID != 3 {
		t.Fatalf("锚点不存在时应追加到末尾：%v", ids(tail))
	}
}

func ids(list []Clip) []int64 {
	out := make([]int64, len(list))
	for i, c := range list {
		out[i] = c.ID
	}
	return out
}

func starts(list []Clip) []int {
	out := make([]int, len(list))
	for i, c := range list {
		out[i] = c.StartMs
	}
	return out
}
