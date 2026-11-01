package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// 媒体引用检查器的装配绊线（MKT-T-23，同 CD-68 的守法）。
//
// ⚠️ 两个进程都要注册同一批检查器：只装 API 的话「API 拒绝删除而 Worker 的回收器照样收走」，
// 两边各自看起来都没错，任何业务测试都是绿的。这里直接读两个 main 的源码，
// 把 `AddReferenceChecker(mediaRefChecker{mediaapp.UsageXxx, ...})` 的 Kind 抽出来比对。
func registeredRefKinds(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s：%v", path, err)
	}
	var kinds []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddReferenceChecker" || len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.CompositeLit)
		if !ok || len(lit.Elts) == 0 {
			t.Errorf("%s：AddReferenceChecker 的参数不是 mediaRefChecker 字面量，绊线读不懂", path)
			return true
		}
		kind, ok := lit.Elts[0].(*ast.SelectorExpr)
		if !ok {
			t.Errorf("%s：检查器的 Kind 不是 mediaapp.UsageXxx 常量", path)
			return true
		}
		kinds = append(kinds, kind.Sel.Name)
		return true
	})
	sort.Strings(kinds)
	return kinds
}

func TestReferenceCheckers_两个main都注册agent(t *testing.T) {
	api := registeredRefKinds(t, "main.go")
	worker := registeredRefKinds(t, "../worker/main.go")
	if strings.Join(api, ",") != strings.Join(worker, ",") {
		t.Fatalf("API 与 Worker 注册的媒体引用检查器不一致：\n api=%v\n worker=%v", api, worker)
	}
	want := []string{"UsageAgentMarketing", "UsageAgentSession", "UsageDirectorScene",
		"UsageGenerationOut", "UsageMarketingCharacter", "UsageMarketingProduct", "UsageProjectCover", "UsageScriptAsset", "UsageTimelineClip"}
	have := map[string]bool{}
	for _, k := range api {
		have[k] = true
	}
	for _, k := range want {
		if !have[k] {
			t.Errorf("两个 main 都缺 %s 的检查器", k)
		}
	}
}
