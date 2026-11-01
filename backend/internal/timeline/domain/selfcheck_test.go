package domain

import (
	"strings"
	"testing"
)

func TestParseSelfCheck(t *testing.T) {
	stderr := []byte(`[blackdetect @ 0x1] black_start:0 black_end:2.04 black_duration:2.04
[Parsed_ebur128_0 @ 0x2] Summary:

  Integrated loudness:
    I:         -14.3 LUFS
    Threshold: -24.4 LUFS
[blackdetect @ 0x1] black_start:9.5 black_end:10.75 black_duration:1.25
`)
	blacks, lufs := ParseSelfCheck(stderr)
	if len(blacks) != 2 || blacks[0] != (TimeSpan{0, 2040}) || blacks[1] != (TimeSpan{9500, 10750}) {
		t.Fatalf("黑场：%+v", blacks)
	}
	if lufs == nil || *lufs != -14.3 {
		t.Fatalf("响度：%v", lufs)
	}
	if b, l := ParseSelfCheck([]byte("没有任何输出")); b != nil || l != nil {
		t.Fatal("什么都没测到时为空")
	}
}

// 只警告：时长差一帧以内不提醒；响度偏 2 LU 以上提醒；每段黑场一条。
func TestBuildSelfCheck(t *testing.T) {
	ok := -14.5
	if c := BuildSelfCheck(10020, 10000, 30, &ok, nil); len(c.Warnings) != 0 || c.BlackSpans == nil {
		t.Fatalf("一帧以内、响度达标、没有黑场：%+v", c)
	}
	loud := -9.0
	c := BuildSelfCheck(10500, 10000, 30, &loud, []TimeSpan{{0, 2000}})
	if len(c.Warnings) != 3 || !strings.Contains(c.Warnings[0], "相差 500 毫秒") ||
		!strings.Contains(c.Warnings[1], "-9.0 LUFS") || !strings.Contains(c.Warnings[2], "0.0~2.0 秒") {
		t.Fatalf("三条提醒：%+v", c.Warnings)
	}
	if args := strings.Join(SelfCheckArgs("/o.mp4", false), " "); strings.Contains(args, "ebur128") || !strings.Contains(args, "-an") {
		t.Fatalf("没有声音时不测响度：%s", args)
	}
}
