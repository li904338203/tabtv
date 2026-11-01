package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 计划项的生成方式与输入对齐（04 文档「技能和模型参数」）：
// 模型给的生成方式收不下这批输入（带了参考图 / 商品图却写成文生视频，或者干脆没写）时，
// 在报价之前换成该模型收得下的那一种。换了要说出来：卡片上给用户看，结果里给模型看，
// 执行时 generate 照着用。

// operationLabels 是生成方式的中文名（说明用）。
var operationLabels = map[string]string{
	"text2video": "文生视频", "img2video": "图生视频", "ref2video": "参考生视频",
	"first_last_frame": "首尾帧生视频", "video2video": "视频生视频",
	"text2img": "文生图", "img2img": "图生图",
}

func opLabel(op string) string {
	if l := operationLabels[op]; l != "" {
		return l + "（" + op + "）"
	}
	return op
}

// opMatcher 给计划项挑生成方式；同一次工具调用里按模型缓存目录结果。
type opMatcher struct {
	ops   app.OperationCatalog
	cache map[string][]domain.OperationFit
}

func newOpMatcher(ops app.OperationCatalog) *opMatcher {
	return &opMatcher{ops: ops, cache: map[string][]domain.OperationFit{}}
}

// fits 取模型的生成方式。取不到（未接入、模型不存在、查库失败）返回 nil：不改生成方式，让报价说明原因。
func (m *opMatcher) fits(ctx context.Context, model string) []domain.OperationFit {
	if m == nil || m.ops == nil || strings.TrimSpace(model) == "" {
		return nil
	}
	if f, ok := m.cache[model]; ok {
		return f
	}
	f, err := m.ops.ModelOperations(ctx, model)
	if err != nil {
		f = nil
	}
	m.cache[model] = f
	return f
}

// match 让第 n 项的生成方式收得下 counts 这批输入。返回改好的条目与说明（换了一个模型自己写的方式时才有说明）。
//
// 只处理视频与图片；模型没写生成方式时按输入挑（原先一律按产出类型取文生）。
// 该模型没有收得下的方式时原样返回，由报价给出原因。
func (m *opMatcher) match(ctx context.Context, n int, it domain.PlanItem, counts map[string]int) (domain.PlanItem, string) {
	if it.Kind != "video" && it.Kind != "image" {
		return it, ""
	}
	fits := m.fits(ctx, it.ModelCode)
	if len(fits) == 0 {
		return it, ""
	}
	op, switched, ok := domain.MatchOperation(it.Kind, it.OperationCode, counts, fits)
	if !ok || op == it.OperationCode {
		return it, ""
	}
	old := it.OperationCode
	it.OperationCode = op
	if !switched {
		return it, ""
	}
	label := it.Label
	if label == "" {
		label = planKindLabels[it.Kind]
	}
	return it, fmt.Sprintf("第 %d 项「%s」用到了%s，%s收不下这些输入，已改为%s。",
		n, label, describeCounts(counts), opLabel(old), opLabel(op))
}

// hint 是该模型在这类产出下能用的生成方式（参数错误退回时附上，让模型照着改，而不是去掉参考图）。
func (m *opMatcher) hint(ctx context.Context, it domain.PlanItem) string {
	fits := m.fits(ctx, it.ModelCode)
	if len(fits) == 0 {
		return ""
	}
	list := domain.DescribeFits(it.Kind, fits)
	if list == "" {
		return ""
	}
	return "\n  该模型能用的生成方式：\n" + indent(list, "  ")
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

var countLabels = map[string]string{"image": "图片", "video": "视频", "audio": "音频"}

// describeCounts 把输入计数写成「1 张图片、1 段视频」（不含文字）。
func describeCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k, v := range counts {
		if v > 0 && k != "text" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := countLabels[k]
		if label == "" {
			label = k
		}
		parts = append(parts, fmt.Sprintf(" %d 个%s", counts[k], label))
	}
	if len(parts) == 0 {
		return "纯文字"
	}
	return strings.Join(parts, "、")
}
