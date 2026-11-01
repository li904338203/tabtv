package seed

import (
	"testing"

	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	smartcutdomain "github.com/li904338203/tabtv/backend/internal/smartcut/domain"
)

// seed 不走后台的连线规则校验（直接写库），所以要由测试保证：每一条 seed 规则都讲得通，
// 运营在后台对它点一次「保存」不会报「对不上」（3D 导演台 DRT-05，DRD-20）。
//
// ⚠️ 导演台的三条规则正是靠这一条钉住的：默认操作的 in_node_types 里不含 director 的话，
// seed 照样写得进去，而后台一保存就红 —— 那时运营看到的是一条「改了什么都存不进去」的规则。
func TestValidateConnectionRuleConfig_导演台规则与seed操作一致(t *testing.T) {
	nodes := make([]catalogdomain.RuleNode, 0, len(nodeTypeRows))
	for _, n := range nodeTypeRows {
		nodes = append(nodes, catalogdomain.RuleNode{Code: n.code, Kind: n.kind})
	}
	ops := make([]catalogdomain.RuleOperation, 0, len(operationRows))
	for _, o := range operationRows {
		ops = append(ops, catalogdomain.RuleOperation{Code: o.code, InNodeTypes: o.inTypes, OutNodeType: o.outType})
	}
	seen := map[string]bool{}
	for _, r := range connectionRuleRows {
		seen[r.src+"→"+r.dst] = true
		if err := catalogdomain.ValidateConnectionRuleConfig(r.src, r.dst, true, r.defaultOp, nodes, ops); err != nil {
			t.Errorf("seed 规则 %s→%s（%s）过不了后台校验：%v", r.src, r.dst, r.defaultOp, err)
		}
	}
	for _, want := range []string{"image→director", "director→image", "director→video"} {
		if !seen[want] {
			t.Errorf("缺少导演台连线规则 %s", want)
		}
	}
	// 智能剪辑（SCT-08）：六条入边不带默认操作，两条出边的默认操作要在 in_node_types 里含 smartcut。
	for _, want := range []string{"video→smartcut", "image→smartcut", "audio→smartcut", "text→smartcut",
		"script→smartcut", "group→smartcut", "smartcut→video", "smartcut→text"} {
		if !seen[want] {
			t.Errorf("缺少智能剪辑连线规则 %s", want)
		}
	}
}

// Agent / MCP 替用户连进智能剪辑时按 smartcut/domain.InboundNodeTypes 判（SCT-41）：它必须与 seed 的入边逐一对应 ——
// 多一种的话 Agent 会连出一条后台不认的线，少一种的话用户能连的 Agent 却说「不能连」。
func TestSmartcutInboundTypes_与seed入边一致(t *testing.T) {
	seed := map[string]bool{}
	for _, r := range connectionRuleRows {
		if r.dst == smartcutdomain.NodeType {
			seed[r.src] = true
		}
	}
	if len(seed) != len(smartcutdomain.InboundNodeTypes) {
		t.Fatalf("seed 入边 %v 与 InboundNodeTypes %v 数量不同", seed, smartcutdomain.InboundNodeTypes)
	}
	for _, typ := range smartcutdomain.InboundNodeTypes {
		if !seed[typ] {
			t.Errorf("InboundNodeTypes 里的 %s 在 seed 里没有入边", typ)
		}
	}
}
