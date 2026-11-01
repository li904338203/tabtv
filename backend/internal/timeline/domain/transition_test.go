package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func fade(ms int) Transition { return Transition{Kind: TransitionFade, DurationMs: ms} }

func TestValidateTransition_正常取值放行(t *testing.T) {
	if err := ValidateTransition(fade(500), 4000, 4000); err != nil {
		t.Fatalf("正常转场不该被拒：%v", err)
	}
	// 没设转场时什么都不用判。
	if err := ValidateTransition(Transition{}, 0, 100); err != nil {
		t.Fatalf("没有转场不该报错：%v", err)
	}
	if err := ValidateTransition(Transition{Kind: TransitionNone, DurationMs: 500}, 0, 100); err != nil {
		t.Fatalf("none 等于没有转场：%v", err)
	}
}

// ⚠️ 转场会"吃掉"两段各一半的时间：时长超过任一侧的一半时，
// 那一侧会被整段吃光 —— 渲染出来直接少一个镜头，
// 而时间线上那一段还好端端地画着。
func TestValidateTransition_不能把相邻段吃光(t *testing.T) {
	// 本段只有 800ms，500ms 的转场要占 1000ms。
	err := ValidateTransition(fade(500), 4000, 800)
	if err == nil || !strings.Contains(err.Error(), "本段") {
		t.Fatalf("应拒绝并说清是本段被吃光：%v", err)
	}
	// 上一段只有 800ms。
	err = ValidateTransition(fade(500), 800, 4000)
	if err == nil || !strings.Contains(err.Error(), "上一段") {
		t.Fatalf("应拒绝并说清是上一段被吃光：%v", err)
	}
}

// ⚠️ 第一段的转入是"从黑场淡入"，没有上一段可吃 —— prevLen 传 0 时不判那一侧。
func TestValidateTransition_第一段从黑场淡入(t *testing.T) {
	if err := ValidateTransition(fade(500), 0, 4000); err != nil {
		t.Fatalf("第一段的淡入不该被上一段的长度拦住：%v", err)
	}
}

func TestValidateTransition_类型与时长边界(t *testing.T) {
	if err := ValidateTransition(Transition{Kind: "explode", DurationMs: 500}, 0, 4000); err == nil {
		t.Fatal("不支持的类型应被拒")
	}
	// ⚠️ 比两帧还短的转场在 30fps 下根本看不出来，
	// 而它仍然会让渲染多走一条 filter 链。
	if err := ValidateTransition(fade(10), 0, 4000); err == nil {
		t.Fatal("过短的转场应被拒")
	}
	if err := ValidateTransition(fade(99999), 0, 999999); err == nil {
		t.Fatal("过长的转场应被拒")
	}
}

// ⚠️ 每个生效的转场都让总时长少一个转场时长（两段重叠那么久）。
// 不算这一笔的话，界面上的总时长与渲染出来的成片对不上 ——
// 而用户是按界面上那个数字去配乐、配字幕的。
func TestEffectiveDuration_转场要减掉重叠(t *testing.T) {
	clips := []Clip{
		{SourceOutMs: 4000, Speed: 100},
		{SourceOutMs: 4000, Speed: 100, TransitionIn: fade(500)},
		{SourceOutMs: 4000, Speed: 100, TransitionIn: fade(500)},
	}
	if got := TotalDuration(clips); got != 12000 {
		t.Fatalf("不算转场时应为 12000，实际 %d", got)
	}
	if got := EffectiveDuration(clips); got != 11000 {
		t.Fatalf("两处转场应各减 500，期望 11000，实际 %d", got)
	}
}

// ⚠️ 第一段的转入是"从黑场淡入"，不与任何东西重叠，所以不减。
func TestEffectiveDuration_第一段的淡入不减(t *testing.T) {
	clips := []Clip{
		{SourceOutMs: 4000, Speed: 100, TransitionIn: fade(500)},
		{SourceOutMs: 4000, Speed: 100},
	}
	if got := EffectiveDuration(clips); got != 8000 {
		t.Fatalf("第一段的淡入不该减，期望 8000，实际 %d", got)
	}
}

func TestEffectiveDuration_空轨道与全转场(t *testing.T) {
	if got := EffectiveDuration(nil); got != 0 {
		t.Fatalf("空轨道应为 0，实际 %d", got)
	}
}

// ⚠️ 坏值不报错：这一列是 jsonb，历史数据与手工改库都可能留下奇怪的东西，
// 而"有一段的转场字段坏了"不该让整条时间线打不开。
func TestParseTransition_坏值当没有转场(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `"不是对象"`, `{"kind":"explode","durationMs":500}`} {
		got := ParseTransition(json.RawMessage(raw))
		if got.IsActive() {
			t.Fatalf("%q 应当解析成「没有转场」，实际 %+v", raw, got)
		}
	}
	good := ParseTransition(json.RawMessage(`{"kind":"fade","durationMs":500}`))
	if !good.IsActive() || good.DurationMs != 500 {
		t.Fatalf("正常值应解析出来：%+v", good)
	}
}

// ⚠️ `volume: 0` 是**静音**，是一个正当的取值。用零值当"没传"的话，
// 用户永远调不到静音 —— 他点了没反应，而界面上那个滑块确实滑到了 0。
func TestApplyProps_零值与未传要分开(t *testing.T) {
	base := Clip{SourceOutMs: 4000, Speed: 100, Volume: 100}

	zero := 0
	muted := ApplyProps(base, ClipProps{Volume: &zero})
	if muted.Volume != 0 {
		t.Fatalf("音量应能调到 0（静音），实际 %d", muted.Volume)
	}
	// 没传的项保持原样。
	if muted.Speed != 100 {
		t.Fatalf("没传的项不该被动，实际 %d", muted.Speed)
	}

	untouched := ApplyProps(base, ClipProps{})
	if untouched.Volume != 100 || untouched.Speed != 100 {
		t.Fatalf("什么都没传时应原样不动：%+v", untouched)
	}
}

// 改变速之后时长要跟着重算 —— 不重算的话落点全错。
func TestApplyProps_变速后时长重算(t *testing.T) {
	base := Clip{SourceOutMs: 4000, Speed: 100, Volume: 100, DurationMs: 4000}
	double := 200
	got := ApplyProps(base, ClipProps{Speed: &double})
	if got.DurationMs != 2000 {
		t.Fatalf("2 倍速后应为 2000，实际 %d", got.DurationMs)
	}
}
