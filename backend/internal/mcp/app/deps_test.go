package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// 不做项绊线的依赖一侧（MCPT-07，MCP-F-90）：工具名之外，**能力的入口**也不能出现。
// 只查工具名的话，有人可以先在端口上加一个 Refund，再起一个名字无害的工具去调它。

// TestForbidden_mcp不导入payment：internal/mcp/**（含测试）不得 import 支付渠道。
// MCP 没有任何一条路需要碰支付：报价只读账本余额，花钱走积分预扣（经 generation 受理）。
func TestForbidden_mcp不导入payment(t *testing.T) {
	const payment = "github.com/li904338203/tabtv/backend/internal/payment"
	forbidden := func(p string) bool { return p == payment || strings.HasPrefix(p, payment+"/") }
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir("..", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		af, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, im := range af.Imports {
			ip, _ := strconv.Unquote(im.Path.Value)
			if forbidden(ip) {
				t.Errorf("%s import 了 %s：MCP 不提供充值 / 购买 / 退款（MCP-F-90）", p, ip)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 30 {
		t.Fatalf("只读到 %d 个源文件：绊线形同虚设", checked)
	}
	for _, p := range []string{payment, payment + "/app", payment + "/domain"} {
		if !forbidden(p) {
			t.Errorf("判定应拦下 %s", p)
		}
	}
	for _, p := range []string{"github.com/li904338203/tabtv/backend/internal/paymentx", "github.com/li904338203/tabtv/backend/internal/billing/app"} {
		if forbidden(p) {
			t.Errorf("判定不该拦 %s", p)
		}
	}
}

// camelWords 把 MemberLevelID 切成 [member level id]。
func camelWords(s string) []string {
	var out []string
	var cur []rune
	rs := []rune(s)
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) ||
			(i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
			out = append(out, strings.ToLower(string(cur)))
			cur = nil
		}
		cur = append(cur, r)
	}
	if len(cur) > 0 {
		out = append(out, strings.ToLower(string(cur)))
	}
	return out
}

var (
	// portForbiddenWords 是方法名里整词出现即拒的词：订单、退款、调账、充值购买、角色变更、删除清理。
	portForbiddenWords = []string{"order", "orders", "refund", "refunds", "adjust", "adjustment", "recharge",
		"topup", "purchase", "checkout", "pay", "payment", "withdraw", "role", "roles", "purge", "transfer", "invite", "kick"}
	// 「写动词 + 对象」组合：成员变更、模型 / 价格写入、删项目。
	portWriteVerbs   = []string{"set", "save", "update", "upsert", "create", "delete", "remove", "put", "publish", "change", "disable", "enable"}
	portWriteObjects = []string{"member", "members", "price", "prices", "pricing", "model", "models", "project", "projects"}
)

// forbiddenPortMethod 返回命中的原因（不命中返回空串）。
func forbiddenPortMethod(name string) string {
	ws := camelWords(name)
	for _, w := range ws {
		for _, f := range portForbiddenWords {
			if w == f {
				return f
			}
		}
	}
	hasVerb, obj := false, ""
	for _, w := range ws {
		for _, v := range portWriteVerbs {
			hasVerb = hasVerb || w == v
		}
		for _, o := range portWriteObjects {
			if w == o {
				obj = o
			}
		}
	}
	// 「建项目」是 MCP 要做的写操作（projects.create，M2）；只拦删 / 改 / 禁用项目。
	if hasVerb && obj != "" && (obj != "project" && obj != "projects" || !slices.Contains(ws, "create")) {
		return "写 " + obj
	}
	return ""
}

// TestForbidden_端口无订单退款与成员权限方法 扫 mcp/app 全部非测试文件里的**接口方法名**
// （端口散在 ports.go / read.go / quote.go / audit.go）。
func TestForbidden_端口无订单退款与成员权限方法(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			for _, m := range it.Methods.List {
				for _, nm := range m.Names {
					methods = append(methods, ts.Name.Name+"."+nm.Name)
				}
			}
			return true
		})
	}
	if len(methods) < 10 {
		t.Fatalf("只扫到 %d 个端口方法：绊线形同虚设", len(methods))
	}
	for _, m := range methods {
		name := m[strings.LastIndex(m, ".")+1:]
		if why := forbiddenPortMethod(name); why != "" {
			t.Errorf("端口方法 %s 命中不做项（%s，MCP-F-90）：订单 / 退款 / 调账 / 成员角色变更 / 模型与价格写入 / 删项目不经 MCP", m, why)
		}
	}
	// 自检。
	for _, bad := range []string{"CreateOrder", "Refund", "RefundOrder", "AdjustCredits", "SetRole", "UpdateMemberRole",
		"RemoveMember", "SavePrice", "DeletePrice", "UpdateModel", "DisableModel", "DeleteProject", "PurgeProject",
		"Recharge", "TransferOwnership", "Pay"} {
		if forbiddenPortMethod(bad) == "" {
			t.Errorf("判定应拦下 %s", bad)
		}
	}
	for _, ok := range []string{"MemberLevelID", "Resolve", "Models", "ImageTools", "QuotePlanItem", "Balance",
		"CreateAccessKey", "RevokeGrant", "List", "Get", "SignedURL", "CreateWithIdemKey", "CreateProject", "Payer"} {
		if why := forbiddenPortMethod(ok); why != "" {
			t.Errorf("判定不该拦 %s（%s）", ok, why)
		}
	}
}
