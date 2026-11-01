package domain

import (
	"fmt"
	"strings"
)

// 后台保存连线规则时的校验（P15-05 补编辑表单时加）。
//
// ⚠️ 起因：此前只判「源与目标非空」—— 存得进一条指向不存在节点的规则、一条连向素材节点的规则、
// 或一个与连线对不上的默认操作。三种都**不报错**，而是在用户画布上变成「连得上却生成不了」：
// 连线校验说可以连，生成时按一个产出不是目标类型的操作去跑，参数对不上被受理拒掉。

// RuleNode 是校验要的节点事实。
type RuleNode struct {
	Code string
	// Kind 是 generator / material / composite。
	Kind string
}

// RuleOperation 是校验要的操作事实。
type RuleOperation struct {
	Code string
	// InNodeTypes 是逗号分隔的可接收输入类型。
	InNodeTypes string
	OutNodeType string
}

// CompatibleOperations 能用作这条连线默认操作的操作码：产出是目标类型、且收源类型的输入。
func CompatibleOperations(source, target string, ops []RuleOperation) []string {
	var out []string
	for _, o := range ops {
		if o.OutNodeType != target {
			continue
		}
		for _, in := range strings.Split(o.InNodeTypes, ",") {
			if strings.TrimSpace(in) == source {
				out = append(out, o.Code)
				break
			}
		}
	}
	return out
}

// ValidateConnectionRuleConfig 校验一条连线规则讲不讲得通。
func ValidateConnectionRuleConfig(source, target string, allowed bool, defaultOp string,
	nodes []RuleNode, ops []RuleOperation) error {
	find := func(code string) (RuleNode, bool) {
		for _, n := range nodes {
			if n.Code == code {
				return n, true
			}
		}
		return RuleNode{}, false
	}
	src, ok := find(source)
	if !ok {
		return fmt.Errorf("源节点类型 %q 不存在", source)
	}
	dst, ok := find(target)
	if !ok {
		return fmt.Errorf("目标节点类型 %q 不存在", target)
	}
	if !allowed {
		// 禁止连接的规则不带默认操作：带了也不会被用到，留着只会让人误以为它在起作用。
		if defaultOp != "" {
			return fmt.Errorf("禁止连接的规则不需要默认操作")
		}
		return nil
	}
	// 素材节点（风格 / 特效）没有生成能力，只为下游贡献提示词 —— 连进它的线什么都不会发生。
	if dst.Kind == "material" {
		return fmt.Errorf("素材节点不接收输入：%s 只能作为修饰输入连到生成节点上", target)
	}
	if src.Kind == "material" {
		if defaultOp != "" {
			return fmt.Errorf("素材节点没有独立的生成能力，只作为目标节点的修饰输入，不填默认操作")
		}
		return nil
	}
	compatible := CompatibleOperations(source, target, ops)
	if defaultOp == "" {
		if dst.Kind == "generator" {
			return fmt.Errorf("连到生成节点的规则必须有默认操作（可选：%s）", orNone(compatible))
		}
		return nil
	}
	for _, c := range compatible {
		if c == defaultOp {
			return nil
		}
	}
	return fmt.Errorf("操作 %s 与 %s → %s 对不上（产出要是 %s、且收 %s 的输入；可选：%s）",
		defaultOp, source, target, target, source, orNone(compatible))
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "没有兼容的操作"
	}
	return strings.Join(list, "、")
}
