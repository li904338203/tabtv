package typeset

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fakeResult(pngBytes int) *Result { return &Result{PNG: make([]byte, pngBytes)} }

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	one := entrySize("a", fakeResult(1000))
	c := newLRU(3 * one) // 正好放三条
	c.put("a", fakeResult(1000))
	c.put("b", fakeResult(1000))
	c.put("c", fakeResult(1000))
	if _, ok := c.get("a"); !ok { // 用过一次，a 变成最近用过的
		t.Fatal("a 应在缓存里")
	}
	c.put("d", fakeResult(1000)) // 超出上限：淘汰最久没用的 b，不是最早放进去的 a
	for key, want := range map[string]bool{"a": true, "b": false, "c": true, "d": true} {
		if _, ok := c.get(key); ok != want {
			t.Errorf("%s 在不在缓存里 = %v，期望 %v", key, ok, want)
		}
	}
	if n, size := c.stats(); n != 3 || size > c.limit {
		t.Errorf("条数 %d、占用 %d，上限 %d", n, size, c.limit)
	}
}

func TestLRUCountsBytesNotEntries(t *testing.T) {
	c := newLRU(0)
	if c.limit != 64<<20 {
		t.Fatalf("默认上限 = %d，应为 64 MB", c.limit)
	}
	// 一条 40 MB 放得下；再来一条 40 MB 就要把第一条挤掉 —— 按条数计量的话两条都会留着。
	c.put("big1", fakeResult(40<<20))
	c.put("big2", fakeResult(40<<20))
	if _, ok := c.get("big1"); ok {
		t.Error("超过 64 MB 时应淘汰最早的那一条")
	}
	if _, ok := c.get("big2"); !ok {
		t.Error("最近放进去的应留着")
	}
	// 比整个上限还大的一条不缓存，也不能把别的条目挤光。
	c.put("huge", fakeResult(65<<20))
	if _, ok := c.get("huge"); ok {
		t.Error("超过上限的一条不该缓存")
	}
	if _, ok := c.get("big2"); !ok {
		t.Error("放不进去的那一条不该挤掉别人")
	}
}

func TestTypesetterCachesByContent(t *testing.T) {
	testFonts(t) // 字体缺失时在这里给出明确的报错
	ts := New(Options{})
	req := Request{Text: "缓存命中", Width: 1280, Height: 720, Style: map[string]any{"color": "#FFF"}}
	first, err := ts.Typeset(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// 等价写法（#FFF 与 #FFFFFF）与多余的未知字段落在同一个键上：直接拿到同一份结果。
	req2 := req
	req2.Style = map[string]any{"color": "#FFFFFF", "unknown": true}
	again, err := ts.Typeset(context.Background(), req2)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("同一个内容哈希应命中缓存")
	}
	req3 := req
	req3.Style = map[string]any{"color": "#FFD400"}
	other, err := ts.Typeset(context.Background(), req3)
	if err != nil {
		t.Fatal(err)
	}
	if other == first || other.Key == first.Key {
		t.Error("改了颜色不该命中缓存")
	}
	if n, _ := ts.cache.stats(); n != 2 {
		t.Errorf("缓存条数 = %d，期望 2", n)
	}
}

func TestTypesetterFontsMissing(t *testing.T) {
	ts := New(Options{FontDir: t.TempDir()})
	_, err := ts.Typeset(context.Background(), Request{Text: "字", Width: 640, Height: 360})
	if err == nil || !strings.Contains(err.Error(), "make fonts") {
		t.Fatalf("字体缺失要说清怎么补：%v", err)
	}
	// 参数错误先于字体：不用等字体装好才知道请求本身不对。
	_, err = ts.Typeset(context.Background(), Request{Text: "", Width: 640, Height: 360})
	if err == nil || !strings.Contains(err.Error(), "text") {
		t.Fatalf("空文字应报 text：%v", err)
	}
}

func TestTypesetterWaitsForSlotUntilContextEnds(t *testing.T) {
	testFonts(t)
	ts := New(Options{MaxConcurrent: 1})
	ts.sem <- struct{}{} // 占住唯一的名额
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ts.Typeset(ctx, Request{Text: "排队", Width: 640, Height: 360})
	if err != context.Canceled {
		t.Fatalf("名额被占满、请求已取消时应返回 context.Canceled：%v", err)
	}
}

// TestTypesetterConcurrentMatchesSequential：并发排版与逐个排版字节一致。go-text 的 Face 与
// 整形器不能并发使用（里面有缓存），共用的话这条在 -race 下会报数据竞争，平时则偶发地排错字形。
func TestTypesetterConcurrentMatchesSequential(t *testing.T) {
	fs := testFonts(t)
	ts := New(Options{MaxConcurrent: 4})
	texts := []string{"并发一", "并发二 Two", "並發三 😀", "동시 넷", "مرحبا 五", "六六六，六六六。"}
	want := make([][]byte, len(texts))
	for i, s := range texts {
		r, err := Layout(fs, Request{Text: s, Width: 1280, Height: 720})
		if err != nil {
			t.Fatal(err)
		}
		want[i] = r.PNG
	}
	errs := make(chan error, len(texts)*4)
	done := make(chan struct{})
	for round := 0; round < 4; round++ {
		for i, s := range texts {
			go func() {
				defer func() { done <- struct{}{} }()
				r, err := ts.Typeset(context.Background(), Request{Text: s, Width: 1280, Height: 720})
				if err == nil && string(r.PNG) != string(want[i]) {
					err = errors.New("并发排出来的与逐个排的不一致：" + s)
				}
				if err != nil {
					errs <- err
				}
			}()
		}
	}
	for range len(texts) * 4 {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func BenchmarkLayoutSubtitle(b *testing.B) {
	fs, err := LoadFonts(ResolveFontDir(""))
	if err != nil {
		b.Fatal(err)
	}
	req := Request{Text: "这是一条普通长度的字幕，带一点 English 和 emoji 😀。", Kind: KindSubtitle, Width: 1920, Height: 1080}
	for b.Loop() {
		if _, err := Layout(fs, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLayoutWorstCase 是接口上限里最重的一种：500 字、4K 画面、描边加底框。
func BenchmarkLayoutWorstCase(b *testing.B) {
	fs, err := LoadFonts(ResolveFontDir(""))
	if err != nil {
		b.Fatal(err)
	}
	req := Request{
		Text: strings.Repeat("排版最重的一种情形，", 50), Width: 3840, Height: 3840,
		Style: map[string]any{"fontSizePct": 20, "maxWidthPct": 100,
			"stroke": map[string]any{"widthPct": 5}, "background": map[string]any{"paddingPct": 10}},
	}
	for b.Loop() {
		if _, err := Layout(fs, req); err != nil {
			b.Fatal(err)
		}
	}
}
