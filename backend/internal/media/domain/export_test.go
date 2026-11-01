package domain

import (
	"strings"
	"testing"
	"time"
)

func pngMIME(int64) string { return "image/png" }

// ⚠️ 同名条目在多数解压工具里后一个覆盖前一个：用户导出 9 张、解出来 7 张，
// 而下载和解压都"成功"了。
func TestPlanEntries_同名要去重(t *testing.T) {
	got := PlanEntries([]ExportItem{
		{MediaID: 1, Name: "第 1 镜"},
		{MediaID: 2, Name: "第 1 镜"},
		{MediaID: 3, Name: "第 1 镜"},
	}, pngMIME)
	want := []string{"第 1 镜.png", "第 1 镜 (2).png", "第 1 镜 (3).png"}
	for i, w := range want {
		if got[i].Path != w {
			t.Fatalf("第 %d 项应为 %s，实际 %s", i, w, got[i].Path)
		}
	}
}

// ⚠️ macOS 与 Windows 的默认文件系统不区分大小写：`A.png` 与 `a.png`
// 在那里仍然是同一个文件，只按精确匹配去重等于没去重。
func TestPlanEntries_去重不区分大小写(t *testing.T) {
	got := PlanEntries([]ExportItem{
		{MediaID: 1, Name: "Cover"},
		{MediaID: 2, Name: "cover"},
	}, pngMIME)
	if got[1].Path != "cover (2).png" {
		t.Fatalf("应按大小写不敏感去重，实际 %s", got[1].Path)
	}
}

// ⚠️ zip slip：名字来自用户可控文本，带 `..` 或分隔符的条目名会让
// 一部分解压工具写到目标目录之外。
func TestPlanEntries_清洗危险的文件名(t *testing.T) {
	cases := []struct{ in, wantNot string }{
		{"../../etc/passwd", "/"},
		{`C:\windows\system32`, `\`},
		{"a/b", "/"},
	}
	for _, c := range cases {
		got := PlanEntries([]ExportItem{{MediaID: 1, Name: c.in}}, pngMIME)
		if strings.Contains(got[0].Path, c.wantNot) {
			t.Fatalf("%q 清洗后仍含 %q: %s", c.in, c.wantNot, got[0].Path)
		}
		if strings.Contains(got[0].Path, "..") {
			t.Fatalf("%q 清洗后仍含 ..: %s", c.in, got[0].Path)
		}
	}
}

func TestPlanEntries_没有名字时用序号(t *testing.T) {
	got := PlanEntries([]ExportItem{{MediaID: 197212345678}, {MediaID: 197212345679}}, pngMIME)
	// ⚠️ 用序号不用 mediaID：一个文件夹里全是 19 位雪花开头的文件，
	// 用户没法分辨顺序。
	if got[0].Path != "001.png" || got[1].Path != "002.png" {
		t.Fatalf("应按序号命名: %s / %s", got[0].Path, got[1].Path)
	}
}

func TestPlanEntries_按MIME给扩展名(t *testing.T) {
	got := PlanEntries([]ExportItem{{MediaID: 1, Name: "片段"}},
		func(int64) string { return "video/mp4" })
	if got[0].Path != "片段.mp4" {
		t.Fatalf("扩展名应跟随 MIME: %s", got[0].Path)
	}
	// 认不出的类型退回 .bin，而不是干脆没有扩展名 ——
	// 没有扩展名的文件在 Windows 上双击打不开。
	got = PlanEntries([]ExportItem{{MediaID: 1, Name: "未知"}},
		func(int64) string { return "application/x-weird" })
	if got[0].Path != "未知.bin" {
		t.Fatalf("未知类型应回落 .bin: %s", got[0].Path)
	}
}

func TestPlanEntries_超长名字截断(t *testing.T) {
	long := strings.Repeat("镜", 200)
	got := PlanEntries([]ExportItem{{MediaID: 1, Name: long}}, pngMIME)
	if n := len([]rune(got[0].Path)); n > maxNameRunes+8 {
		t.Fatalf("文件名应被截断，实际 %d 字符", n)
	}
}

func TestValidateExportItems(t *testing.T) {
	if err := ValidateExportItems(nil); err == nil {
		t.Fatal("空清单应被拒绝")
	}
	// ⚠️ 重复项**拒绝**而不是悄悄去重：悄悄去重的话用户选了 10 项、
	// 包里只有 8 个文件，他会以为丢了东西。
	err := ValidateExportItems([]ExportItem{{MediaID: 1}, {MediaID: 1}})
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复项应被拒绝并说明原因，实际 %v", err)
	}
	if err := ValidateExportItems([]ExportItem{{MediaID: 0}}); err == nil {
		t.Fatal("非法 ID 应被拒绝")
	}
	big := make([]ExportItem, MaxExportItems+1)
	for i := range big {
		big[i] = ExportItem{MediaID: int64(i + 1)}
	}
	if err := ValidateExportItems(big); err == nil {
		t.Fatal("超过项数上限应被拒绝")
	}
}

// ⚠️ 过期是算出来的：靠清理作业标记的话，作业停了一天，界面上就有
// 一整天的下载按钮点下去 404 —— 而用户看到的是"导出成功"。
func TestEffectiveExportStatus_过期是算出来的(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	if got := EffectiveExportStatus(ExportSuccess, past, now); got != ExportExpired {
		t.Fatalf("成功但已过期应显示 expired，实际 %s", got)
	}
	if got := EffectiveExportStatus(ExportSuccess, future, now); got != ExportSuccess {
		t.Fatalf("未过期应保持 success，实际 %s", got)
	}
	// 失败的任务过不过期都是失败：说"已过期"会让用户以为本来是成功的。
	if got := EffectiveExportStatus(ExportFailed, past, now); got != ExportFailed {
		t.Fatalf("失败任务不该变成 expired，实际 %s", got)
	}
	// 还在排队的任务即使 expires_at 已过也不是 expired —— 它还没有包可言。
	if got := EffectiveExportStatus(ExportQueued, past, now); got != ExportQueued {
		t.Fatalf("排队中不该变成 expired，实际 %s", got)
	}
}
