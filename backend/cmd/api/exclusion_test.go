package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 排除项断言（MKT-T-32 / MKT-AC-23）：营销 Agent 明确不做「每日热点趋势」（MK-4）与
// 数字人 / 口型驱动（MK-5）—— 不建对应的路由、权限、队列。
//
// ⚠️ 读源码而不是起一个真路由器：router() 要全套 bootstrap 依赖（库、Redis、MQ），
// 为一条关键词断言把整个进程拉起来不值；而路由的注册点就是这些 r.Get/Post/Route/Mount
// 调用的字面量 —— 新增一条带关键词的路由在这里当场红，这正是绊线要的。
// 动态拼出来的路径（非字面量）扫不到，本仓库没有那种写法；出现时这条绊线需要跟着升级。
//
// MKT-T-43 扩到 M-E（MK-23~25、MKT-AC-37 / 45）：不做真人角色与扫码认证、角色不做可见范围、不做声音克隆；
// 并加一条契约扫描（`api/openapi.yaml` 与 `api/events/*.json` 的**键**：路径、schema 名、字段名）。
// 只扫键不扫值：描述文字里「真人整份保存」「只改写非真人角色的外观描述」是合法说明，支付渠道的 `qrcode` 是枚举值。
// 前端的同类断言在 `frontend/utils/marketingExclusion.test.ts`，管理端在 `apps/admin/src/auth/permissions.test.ts`
// （三份关键词表各自注明来源；语言不同无法共用）。
//
// 关键词表只放排除项**特有**的词：「hook」是营销合法词（Hook 选项）、「clip」是时间线片段、
// 「avatar」可能是头像 —— 都不放。英文按**整词**比（`photo` 里有 hot、`clips` 里有 lip，子串匹配会误伤），
// 中文按子串比。
var (
	// 整词（按 / - _ : . { } 空白 切开之后逐段比；全部小写）。来源：MK-4 每日热点、MK-5 数字人 / 口型驱动。
	excludedTokens = []string{"hot", "hotspot", "hotspots", "trend", "trends", "trending",
		"lip", "lipsync", "lips"}
	// 子串（中文与带分隔符的英文复合词）。
	excludedSubstrings = []string{"热点", "热榜", "数字人", "口型", "对口型",
		"digital_human", "digitalhuman", "digital-human", "lip_sync", "lip-sync",
		// MKT-T-43（M-E，MK-23~25 / MKT-AC-37、45）：不做真人角色与扫码认证、角色不做可见范围、不做声音克隆。
		// 用子串而不是整词：契约字段是 camelCase（`characterVisibility`、`isRealPerson`、`voiceClone`），
		// 小写之后不再有分隔符，整词比较会漏掉。这几个子串在现有路由 / 权限 / 队列 / 契约键里一个都没有。
		"visibility", "realperson", "real_person", "real-person",
		"voiceclone", "voice_clone", "voice-clone", "真人认证", "真人角色", "扫码认证", "实名认证", "声音克隆"}
	// 同一路径里同时出现这两个整词才算（`/voices/clone`）。单独的「clone」不进表：
	// 「复制项目」将来很可能就叫 `/projects/{id}/clone`，那是合法功能。
	excludedTokenPairs = [][2]string{{"voice", "clone"}, {"voices", "clone"}}
)

func excludedHit(s string) string {
	low := strings.ToLower(s)
	for _, sub := range excludedSubstrings {
		if strings.Contains(low, sub) {
			return sub
		}
	}
	toks := strings.FieldsFunc(low, func(r rune) bool {
		return strings.ContainsRune("/-_:.{}* \t", r)
	})
	seen := make(map[string]bool, len(toks))
	for _, tok := range toks {
		seen[tok] = true
		for _, bad := range excludedTokens {
			if tok == bad {
				return bad
			}
		}
	}
	for _, p := range excludedTokenPairs {
		if seen[p[0]] && seen[p[1]] {
			return p[0] + "+" + p[1]
		}
	}
	return ""
}

func TestExcludedHit_不误伤营销合法词(t *testing.T) {
	for _, ok := range []string{"/marketing/options", "/media/{id}/photo", "/timelines/{id}/clips",
		"hk_stop_scroll", "/me/avatar", "/agent/skills", "shot", "hotel",
		// M-E 合法词：角色库、营销角色的参考音频、音色目录、OAuth 客户端的 verified、支付的 qrcode。
		"/marketing/characters", "/marketing/preset-characters/{presetId}/voice", "/admin/voices",
		"verified", "qrcode", "/projects/{id}/clone", "isVisible", "voiceText", "voiceMediaId"} {
		if hit := excludedHit(ok); hit != "" {
			t.Errorf("%q 被误判为排除项（命中 %q）", ok, hit)
		}
	}
	for _, bad := range []string{"/marketing/hot-topics", "/trends/daily", "/digital_human/render",
		"lip-sync.jobs", "数字人管理", "每日热点", "/avatar/{id}/lipsync",
		"/marketing/characters/{id}/visibility", "characterVisibility", "isRealPerson", "real_person",
		"voiceClone", "voice_clone.jobs", "/voices/clone", "真人认证", "扫码认证"} {
		if excludedHit(bad) == "" {
			t.Errorf("%q 应命中排除项关键词", bad)
		}
	}
}

// goSources 列出 dir 下全部非测试 Go 源文件（跳过生成代码 apigen：那是契约的镜像，契约另有前端断言）。
func goSources(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "apigen" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				out = append(out, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func parseGo(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s：%v", path, err)
	}
	return f
}

func strLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

var routeMethods = map[string]bool{"Get": true, "Post": true, "Put": true, "Patch": true, "Delete": true,
	"Head": true, "Options": true, "Handle": true, "HandleFunc": true, "Route": true, "Mount": true,
	"Method": true, "MethodFunc": true, "Connect": true, "Trace": true}

func TestRoutes_不含排除项关键词(t *testing.T) {
	n := 0
	for _, path := range goSources(t, "../../internal", "../../cmd") {
		ast.Inspect(parseGo(t, path), func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !routeMethods[sel.Sel.Name] {
				return true
			}
			// Method(method, pattern, h)：路径在第二个参数。
			args := call.Args
			if (sel.Sel.Name == "Method" || sel.Sel.Name == "MethodFunc") && len(args) > 1 {
				args = args[1:]
			}
			p, ok := strLit(args[0])
			if !ok || !strings.HasPrefix(p, "/") {
				return true
			}
			n++
			if hit := excludedHit(p); hit != "" {
				t.Errorf("%s：路由 %q 含排除项关键词 %q（MK-4 / MK-5，MKT-AC-23）", path, p, hit)
			}
			return true
		})
	}
	// 绊线自检：一条都没扫到说明扫描方式失效了（例如路由改成了别的注册写法），而那时断言恒绿。
	if n < 100 {
		t.Fatalf("只扫到 %d 条路由字面量，扫描方式可能已失效", n)
	}
}

func TestPermissions_不含排除项关键词(t *testing.T) {
	f := parseGo(t, "../../internal/platform/seed/seed.go")
	found, n := false, 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if len(vs.Names) != 1 || vs.Names[0].Name != "permissionSeeds" {
				continue
			}
			found = true
			ast.Inspect(vs, func(node ast.Node) bool {
				if s, ok := strLit(asExpr(node)); ok {
					n++
					if hit := excludedHit(s); hit != "" {
						t.Errorf("权限位 %q 含排除项关键词 %q（MK-4 / MK-5，MKT-AC-23）", s, hit)
					}
				}
				return true
			})
		}
	}
	if !found || n < 30 {
		t.Fatalf("没在 seed.go 里找到 permissionSeeds 或字面量过少（%d），绊线失效", n)
	}
}

func asExpr(n ast.Node) ast.Expr {
	e, _ := n.(ast.Expr)
	return e
}

// 队列：NewConsumer 的队列名字面量 + 全部 *Queue / *Topic 常量（主题决定路由键，队列名由它派生）。
func TestQueues_不含排除项关键词(t *testing.T) {
	n := 0
	check := func(where, s string) {
		n++
		if hit := excludedHit(s); hit != "" {
			t.Errorf("%s：队列 / 主题 %q 含排除项关键词 %q（MK-4 / MK-5，MKT-AC-23）", where, s, hit)
		}
	}
	for _, path := range goSources(t, "../../internal", "../../cmd") {
		ast.Inspect(parseGo(t, path), func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewConsumer" {
					for _, a := range x.Args {
						if s, ok := strLit(a); ok {
							check(path, s)
						}
					}
				}
			case *ast.ValueSpec:
				for i, name := range x.Names {
					if (strings.HasSuffix(name.Name, "Queue") || strings.HasSuffix(name.Name, "Topic")) &&
						i < len(x.Values) {
						if s, ok := strLit(x.Values[i]); ok {
							check(path, s)
						}
					}
				}
			}
			return true
		})
	}
	if n < 5 {
		t.Fatalf("只扫到 %d 个队列 / 主题，扫描方式可能已失效", n)
	}
}

// 契约：openapi 与事件 schema 的全部映射键（路径、schema 名、字段名）。MKT-AC-37「接口与契约中不存在
// `visibility` / 真人认证相关字段与路由」—— 字段只出现在契约与 DTO 里，扫路由字面量抓不到它。
func TestContract_不含排除项关键词(t *testing.T) {
	files := []string{"../../../api/openapi.yaml"}
	events, err := filepath.Glob("../../../api/events/*.json")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, events...)
	n := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var root yaml.Node
		if err := yaml.Unmarshal(raw, &root); err != nil {
			t.Fatalf("解析 %s：%v", path, err)
		}
		var walk func(*yaml.Node)
		walk = func(node *yaml.Node) {
			if node.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(node.Content); i += 2 {
					key := node.Content[i].Value
					n++
					if hit := excludedHit(key); hit != "" {
						t.Errorf("%s 第 %d 行：契约键 %q 含排除项关键词 %q（MK-4 / MK-5 / MK-23~25，MKT-AC-23 / 37）",
							path, node.Content[i].Line, key, hit)
					}
					walk(node.Content[i+1])
				}
				return
			}
			for _, c := range node.Content {
				walk(c)
			}
		}
		walk(&root)
	}
	// 绊线自检：契约挪了位置或解析方式失效时，这里先红而不是恒绿。
	if len(files) < 2 || n < 5000 {
		t.Fatalf("只扫到 %d 个文件、%d 个键，扫描方式可能已失效", len(files), n)
	}
}
