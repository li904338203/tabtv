package domain

import "fmt"

// 选图错误分类。工具层（agent）据此把错误回给模型重选，**不静默截断**（MKT-F-48 ④）。
const (
	ImageOutOfRange = "out_of_range"
	ImageDuplicate  = "duplicate"
	ImageTooMany    = "too_many"
)

// ImageSelectError 是选图失败。Max / Total 原样带出：模型要据此重选，
// 只说"选多了"的话它不知道该减到几张。
type ImageSelectError struct {
	Code  string
	Index int // 出问题的那个序号（too_many 时为 0）
	Total int
	Max   int
}

func (e *ImageSelectError) Error() string {
	switch e.Code {
	case ImageOutOfRange:
		return fmt.Sprintf("商品图 #%d 不存在：共 %d 张，序号范围 #1~#%d", e.Index, e.Total, e.Total)
	case ImageDuplicate:
		return fmt.Sprintf("商品图 #%d 重复选取", e.Index)
	default:
		return fmt.Sprintf("选取的商品图超过该模型的参考图上限：最多 %d 张", e.Max)
	}
}

// SelectImages 校验一个生成项选用的商品图序号（**从 1 起**，与「商品图 #n」一致）。
//
//   - 越界、重复、超过上限 max 各给明确错误（带 max，MKT-AC-26 ④）；
//     超上限是**拒绝**不是截断 —— 截掉哪几张模型不知道，确认卡上写的就和实际发的不一样。
//   - indexes 为空 = 未指定，按商品槽顺序取前 max 张（DefaultIndexes，MKT-F-48 ③）。
//   - max ≤ 0 表示该模型 × 操作不收图：任何非空选择都超限。
//
// 返回值是入参的副本，顺序即发给上游的参考图顺序（不重排）。
func SelectImages(total int, indexes []int, max int) ([]int, error) {
	if len(indexes) == 0 {
		return DefaultIndexes(total, max), nil
	}
	seen := make(map[int]bool, len(indexes))
	for _, i := range indexes {
		if i < 1 || i > total {
			return nil, &ImageSelectError{Code: ImageOutOfRange, Index: i, Total: total, Max: max}
		}
		if seen[i] {
			return nil, &ImageSelectError{Code: ImageDuplicate, Index: i, Total: total, Max: max}
		}
		seen[i] = true
	}
	if len(indexes) > max {
		return nil, &ImageSelectError{Code: ImageTooMany, Total: total, Max: max}
	}
	out := make([]int, len(indexes))
	copy(out, indexes)
	return out, nil
}

// DefaultIndexes 按商品槽顺序取前 min(total, max) 张：#1 是主图，排在最前。
func DefaultIndexes(total, max int) []int {
	n := total
	if max < n {
		n = max
	}
	if n <= 0 {
		return []int{}
	}
	out := make([]int, n)
	for i := range out {
		out[i] = i + 1
	}
	return out
}
