package canvasgen

import (
	"context"
	"fmt"
	"strings"
)

// 新建节点的类型推导（Agent 与 MCP 的 generate 共用，不各写一份）。
//
// 起因（会话 105692702233853952）：模型调用 generate 只给了 operationCode=text2video、没给 nodeId
// 也没给 nodeType，参数校验直接报「nodeType 必须是 image / video / audio / text，实际 ""」——
// 前端多出一条红色失败步骤，而操作码本来就已经决定了产出什么。
//
// ⚠️ 产出类型查目录的 `operation.out_node_type`，**不靠操作码字符串猜**（CLAUDE.md §2.8 / §5.5）：
// 新增一个操作只改目录，不该还要改这里。

// NewNodeTypes 是 generate 可新建的节点类型（与画布节点类型逐字一致）。
var NewNodeTypes = []string{"image", "video", "audio", "text"}

// IsNewNodeType 报告 t 是不是 generate 可新建的节点类型。
func IsNewNodeType(t string) bool {
	for _, n := range NewNodeTypes {
		if n == t {
			return true
		}
	}
	return false
}

// NodeTypeError 是「节点类型定不下来 / 与操作对不上」这类参数错误（调用方翻成参数非法）；
// 查目录失败不是它（那是内部错误）。
type NodeTypeError struct{ Msg string }

func (e *NodeTypeError) Error() string { return e.Msg }

func nodeTypeErrf(format string, a ...any) error {
	return &NodeTypeError{Msg: fmt.Sprintf(format, a...)}
}

// OperationOutputs 给出操作的产出节点类型（operation.out_node_type）。
// 操作不存在或没登记时返回空串、不报错；只有查询失败才返回 error。
type OperationOutputs interface {
	OperationOutNodeType(ctx context.Context, operationCode string) (string, error)
}

// ResolveNewNodeType 定下新建节点的类型（只用于不给 nodeId 的情形）：
//
//   - 没给 nodeType：取操作的 out_node_type；它不是可新建的节点类型时报错并说清是什么；
//   - 给了 nodeType：必须与 out_node_type 一致（否则会建出一个类型不对的节点，任务挂上去也对不上）；
//   - 目录查不到产出类型（src 为 nil / 操作没登记）：退回旧规则，nodeType 必须显式给且合法。
func ResolveNewNodeType(ctx context.Context, src OperationOutputs, operationCode, declared string) (string, error) {
	declared = strings.TrimSpace(declared)
	out := ""
	if src != nil {
		o, err := src.OperationOutNodeType(ctx, operationCode)
		if err != nil {
			return "", fmt.Errorf("查询操作 %s 的产出类型失败：%w", operationCode, err)
		}
		out = strings.TrimSpace(o)
	}
	if out == "" {
		if IsNewNodeType(declared) {
			return declared, nil
		}
		if declared == "" && src != nil {
			return "", nodeTypeErrf("操作 %s 不在目录里或没有登记产出类型，无法确定要新建的节点类型；"+
				"请确认 operationCode，或显式给 nodeType（%s）", operationCode, strings.Join(NewNodeTypes, " / "))
		}
		return "", nodeTypeErrf("不给 nodeId 时 nodeType 必须是 %s，实际 %q", strings.Join(NewNodeTypes, " / "), declared)
	}
	if declared != "" && declared != out {
		return "", nodeTypeErrf("nodeType=%q 与操作 %s 的产出类型 %q 不一致；不给 nodeType 时会按操作自动确定",
			declared, operationCode, out)
	}
	if !IsNewNodeType(out) {
		return "", nodeTypeErrf("操作 %s 的产出类型是 %q，不能直接建节点（可新建的类型：%s）",
			operationCode, out, strings.Join(NewNodeTypes, " / "))
	}
	return out, nil
}

// NewNodeType 用编排上配置的目录（Outputs）推导新建节点的类型，见 ResolveNewNodeType。
func (o Orchestrator) NewNodeType(ctx context.Context, operationCode, declared string) (string, error) {
	return ResolveNewNodeType(ctx, o.Outputs, operationCode, declared)
}
