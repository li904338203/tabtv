package domain

import (
	"fmt"
	"image"
)

// 宫格切分（NEW-14）：把一张图切成若干张**独立素材**，编号与位置明确。
//
// ⚠️ 切分**不走生成链路**：没有模型、没有上游、没有费用 ——
// 它就是一次本地像素搬运。塞进 generation 的话会凭空多出一条
// 永远 0 积分、永远同步完成的"任务"，把任务列表和对账都搅乱。

// MaxGrid 单边最大格数。
//
// ⚠️ 上限不是为了防滥用，是为了**别把一次点击变成几千个文件**：
// 16×16 就是 256 张图，每张都要上传对象存储、写一行库、建一条引用。
const MaxGrid = 8

// GridSpec 是一次切分的规格。
type GridSpec struct {
	Rows int
	Cols int
}

func (g GridSpec) Validate() error {
	if g.Rows < 1 || g.Cols < 1 {
		return fmt.Errorf("行列数至少为 1")
	}
	if g.Rows > MaxGrid || g.Cols > MaxGrid {
		return fmt.Errorf("行列数最多 %d，当前 %d×%d", MaxGrid, g.Rows, g.Cols)
	}
	if g.Rows == 1 && g.Cols == 1 {
		// 切成一张等于没切，却要多存一份文件、多占一份空间。
		return fmt.Errorf("1×1 不需要切分")
	}
	return nil
}

// Tile 是切出来的一块。
type Tile struct {
	// Index 从 1 起，**按行优先**编号。
	//
	// ⚠️ 编号从 1 起而不是 0：它会直接显示给用户（"第 3 格"），
	// 而用户不数 0。
	Index int
	Row   int // 从 0 起，供定位
	Col   int
	Rect  image.Rectangle
}

// SplitGrid 算出每一块的像素范围。
//
// ⚠️ 用**累积边界**而不是"每块宽 = 总宽/列数"：整除不尽时后者会在右边
// 和下边留下一条没被任何块覆盖的细缝（1000÷3 每块 333，最后 1 像素丢了）。
// 按边界算则最后一块自动吃掉余数，拼回去与原图**逐像素一致**。
func SplitGrid(width, height int, g GridSpec) ([]Tile, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("原图尺寸未知，无法切分")
	}
	if width < g.Cols || height < g.Rows {
		// 比格数还小的图切出来会有 0 宽的块 —— 那不是图片。
		return nil, fmt.Errorf("原图 %d×%d 太小，切不出 %d×%d 格",
			width, height, g.Rows, g.Cols)
	}

	out := make([]Tile, 0, g.Rows*g.Cols)
	idx := 0
	for r := 0; r < g.Rows; r++ {
		y0 := height * r / g.Rows
		y1 := height * (r + 1) / g.Rows
		for c := 0; c < g.Cols; c++ {
			x0 := width * c / g.Cols
			x1 := width * (c + 1) / g.Cols
			idx++
			out = append(out, Tile{
				Index: idx, Row: r, Col: c,
				Rect: image.Rect(x0, y0, x1, y1),
			})
		}
	}
	return out, nil
}

// TileLabel 给用户看的编号。
//
// ⚠️ 带上行列而不只是序号：用户在 3×3 里说"第 6 格"要数半天，
// 说"第 2 行第 3 格"一眼就知道是哪块。
func TileLabel(t Tile, g GridSpec) string {
	if g.Rows == 1 || g.Cols == 1 {
		return fmt.Sprintf("第 %d 格", t.Index)
	}
	return fmt.Sprintf("第 %d 行第 %d 格", t.Row+1, t.Col+1)
}
