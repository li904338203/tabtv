//go:build integration

// 画布配置后台保存（P15-05 补编辑表单时发现）。
//
// ⚠️ 参数定义的「停用」原本是单向门：保存的 upsert 不碰 is_enabled，而停用走另一条路径 ——
// 停用之后没有任何接口能重新启用，运营点错一次，这个参数就从所有节点上永久消失了。
package integration

import (
	"context"
	"testing"

	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
)

func TestParamDef_停用之后能重新启用且不传则保留(t *testing.T) {
	r, pool, next := setupCatalog(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	enabled := func() bool {
		t.Helper()
		var on bool
		if err := pool.QueryRow(ctx, `select is_enabled from param_def where code = 'quality_level'`).Scan(&on); err != nil {
			t.Fatal(err)
		}
		return on
	}
	save := func(on *bool) {
		t.Helper()
		if err := svc.SaveParamDef(ctx, 0, catalogapp.ParamDefInput{Code: "quality_level", Name: "质量档位",
			Control: "switch", Config: []byte(`{}`), IsEnabled: on}); err != nil {
			t.Fatal(err)
		}
	}
	yes, no := true, false
	save(nil)
	if !enabled() {
		t.Fatal("新建的参数默认启用")
	}
	if err := svc.DisableParamDef(ctx, 0, "quality_level"); err != nil {
		t.Fatal(err)
	}
	// 不带 isEnabled 的保存（只改名字）不能顺手把它启用
	save(nil)
	if enabled() {
		t.Fatal("不传 isEnabled 的保存把停用的参数启用了")
	}
	save(&yes)
	if !enabled() {
		t.Fatal("显式启用没生效 —— 停用成了单向门")
	}
	save(&no)
	if enabled() {
		t.Fatal("显式停用没生效")
	}
}

// 连线规则保存要过可行性校验（与 domain.ValidateConnectionRuleConfig 同一条）：
// 此前只判非空，连向素材节点、默认操作与连线对不上的规则都存得进去，在用户画布上变成「连得上却生成不了」。
func TestConnectionRule_讲不通的规则存不进去(t *testing.T) {
	r, pool, next := setupCatalog(t)
	svc := catalogapp.New(r, next)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into node_type (id, code, name, kind, sort, is_enabled) values
		  ($1, 'text', '文本', 'generator', 1, true), ($2, 'image', '图片', 'generator', 2, true),
		  ($3, 'style', '风格', 'material', 3, true)`, next(), next(), next()); err != nil {
		t.Fatal(err)
	}
	seedOp(t, pool, next, "text2img", "image", "text,style", "image", "enabled")
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from connection_rule`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, in := range map[string]catalogapp.ConnectionRuleInput{
		"连向素材节点":   {SourceType: "text", TargetType: "style", IsAllowed: true},
		"缺默认操作":    {SourceType: "text", TargetType: "image", IsAllowed: true},
		"源节点不存在":   {SourceType: "3d", TargetType: "image", IsAllowed: true, DefaultOperation: "text2img"},
		"素材源带默认操作": {SourceType: "style", TargetType: "image", IsAllowed: true, DefaultOperation: "text2img"},
	} {
		if err := svc.SaveConnectionRule(ctx, 0, in); err == nil {
			t.Errorf("%s 应被拒", name)
		}
	}
	if n := count(); n != 0 {
		t.Fatalf("被拒的规则不该落库，库里有 %d 条", n)
	}
	if err := svc.SaveConnectionRule(ctx, 0, catalogapp.ConnectionRuleInput{
		SourceType: "text", TargetType: "image", IsAllowed: true, DefaultOperation: "text2img"}); err != nil {
		t.Fatalf("讲得通的规则应能保存：%v", err)
	}
	if err := svc.SaveConnectionRule(ctx, 0, catalogapp.ConnectionRuleInput{
		SourceType: "style", TargetType: "image", IsAllowed: true}); err != nil {
		t.Fatalf("素材源不带默认操作应能保存：%v", err)
	}
}
