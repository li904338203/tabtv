package domain

import (
	"strings"
	"testing"
)

func TestSplitGrid_按行优先从1开始编号(t *testing.T) {
	tiles, err := SplitGrid(300, 200, GridSpec{Rows: 2, Cols: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(tiles) != 6 {
		t.Fatalf("2×3 应切出 6 块，实际 %d", len(tiles))
	}
	// ⚠️ 编号从 1 起：它会直接显示给用户（"第 3 格"），而用户不数 0。
	if tiles[0].Index != 1 || tiles[5].Index != 6 {
		t.Fatalf("编号应从 1 起: %d..%d", tiles[0].Index, tiles[5].Index)
	}
	// 行优先：第 2 块应在第 1 行第 2 列。
	if tiles[1].Row != 0 || tiles[1].Col != 1 {
		t.Fatalf("应按行优先编号: %+v", tiles[1])
	}
	if tiles[3].Row != 1 || tiles[3].Col != 0 {
		t.Fatalf("第 4 块应是第 2 行第 1 列: %+v", tiles[3])
	}
}

// ⚠️ 用"每块宽 = 总宽/列数"的话，整除不尽时右边和下边会留下一条
// 没被任何块覆盖的细缝（1000÷3 每块 333，最后 1 像素丢了）。
func TestSplitGrid_整除不尽也要拼回原图(t *testing.T) {
	const w, h = 1000, 999
	tiles, err := SplitGrid(w, h, GridSpec{Rows: 3, Cols: 3})
	if err != nil {
		t.Fatal(err)
	}
	// 最右一列的右边界 == 原图宽；最下一行的下边界 == 原图高。
	var maxX, maxY int
	area := 0
	for _, x := range tiles {
		if x.Rect.Max.X > maxX {
			maxX = x.Rect.Max.X
		}
		if x.Rect.Max.Y > maxY {
			maxY = x.Rect.Max.Y
		}
		area += x.Rect.Dx() * x.Rect.Dy()
	}
	if maxX != w || maxY != h {
		t.Fatalf("右下角应正好盖住原图: %d×%d", maxX, maxY)
	}
	// 面积之和 == 原图面积：既没漏也没重叠。
	if area != w*h {
		t.Fatalf("各块面积之和应等于原图，实际 %d ≠ %d", area, w*h)
	}
}

func TestSplitGrid_块之间不重叠且相邻(t *testing.T) {
	tiles, err := SplitGrid(100, 100, GridSpec{Rows: 2, Cols: 2})
	if err != nil {
		t.Fatal(err)
	}
	// 第 1 块的右边界 == 第 2 块的左边界。
	if tiles[0].Rect.Max.X != tiles[1].Rect.Min.X {
		t.Fatalf("横向应严丝合缝: %d vs %d",
			tiles[0].Rect.Max.X, tiles[1].Rect.Min.X)
	}
	if tiles[0].Rect.Max.Y != tiles[2].Rect.Min.Y {
		t.Fatalf("纵向应严丝合缝: %d vs %d",
			tiles[0].Rect.Max.Y, tiles[2].Rect.Min.Y)
	}
}

func TestSplitGrid_拒绝不合理的规格(t *testing.T) {
	cases := []struct {
		name string
		spec GridSpec
		want string
	}{
		// ⚠️ 切成一张等于没切，却要多存一份文件、多占一份空间。
		{"1×1", GridSpec{1, 1}, "不需要切分"},
		{"零行", GridSpec{0, 3}, "至少为 1"},
		// ⚠️ 上限不是防滥用，是别把一次点击变成几千个文件：
		// 16×16 就是 256 张图，每张都要上传、写库、建引用。
		{"超上限", GridSpec{9, 9}, "最多"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := SplitGrid(100, 100, c.spec); err == nil {
				t.Fatal("应被拒绝")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误应说明原因，实际 %v", err)
			}
		})
	}
}

// ⚠️ 比格数还小的图切出来会有 0 宽的块 —— 那不是图片。
func TestSplitGrid_图太小时拒绝(t *testing.T) {
	_, err := SplitGrid(2, 2, GridSpec{Rows: 3, Cols: 3})
	if err == nil || !strings.Contains(err.Error(), "太小") {
		t.Fatalf("应拒绝并说明原因，实际 %v", err)
	}
}

func TestSplitGrid_尺寸未知时拒绝(t *testing.T) {
	// 尺寸没记下来（格式不认识）就切不了 —— 这时候拒绝是对的，
	// 因为切分完全依赖像素坐标，猜不得。
	if _, err := SplitGrid(0, 0, GridSpec{Rows: 2, Cols: 2}); err == nil {
		t.Fatal("尺寸未知应被拒绝")
	}
}

// ⚠️ 用户在 3×3 里说"第 6 格"要数半天，说"第 2 行第 3 格"一眼就知道。
func TestTileLabel_多行时带上行列(t *testing.T) {
	tiles, _ := SplitGrid(300, 300, GridSpec{Rows: 3, Cols: 3})
	if got := TileLabel(tiles[5], GridSpec{3, 3}); got != "第 2 行第 3 格" {
		t.Fatalf("应带上行列: %s", got)
	}
}

func TestTileLabel_单行单列时只给序号(t *testing.T) {
	tiles, _ := SplitGrid(300, 100, GridSpec{Rows: 1, Cols: 3})
	if got := TileLabel(tiles[1], GridSpec{1, 3}); got != "第 2 格" {
		t.Fatalf("单行时说行列反而啰嗦: %s", got)
	}
}
