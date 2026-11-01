//go:build integration

// 商品目录后台保存的校验（P15-05 补编辑表单时加）。
//
// ⚠️ 用真库：排序冲突要和**库里已有的等级**比、套餐挂的等级要**真的存在** ——
// 这两条恰恰是「只判不能为空」时放过去的，放过去的后果都不报错：
// 同排序的两个等级之间可以 0 元来回「升级」；挂在不存在等级上的套餐用户端永远列不出来。
package integration

import (
	"context"
	"errors"
	"testing"

	billingapp "github.com/li904338203/tabtv/backend/internal/billing/app"
	"github.com/li904338203/tabtv/backend/internal/billing/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

func bizCode(err error) errcode.Code {
	var e *httpx.BizError
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestCatalogAdmin_保存前的校验(t *testing.T) {
	r, _ := setup(t)
	svc := billingapp.New(r)
	ctx := context.Background()

	if err := svc.SaveLevel(ctx, 1, repo.Level{Code: "basic", Name: "基础会员", Sort: 10, IsEnabled: true}); err != nil {
		t.Fatal(err)
	}
	// 与别的等级同排序：拒，且一行都不写
	err := svc.SaveLevel(ctx, 1, repo.Level{Code: "pro", Name: "专业会员", Sort: 10, IsEnabled: true})
	if bizCode(err) != errcode.InvalidArgument {
		t.Fatalf("同排序应被拒，得到 %v", err)
	}
	levels, _ := r.ListLevels(ctx, false)
	if len(levels) != 1 {
		t.Fatalf("被拒的等级不该落库，库里有 %d 个", len(levels))
	}
	// 编辑自己（排序不变、改名）不算冲突
	if err := svc.SaveLevel(ctx, 1, repo.Level{Code: "basic", Name: "基础会员 改", Sort: 10, IsEnabled: true}); err != nil {
		t.Fatalf("编辑自己不该撞自己的排序：%v", err)
	}
	// 编码是业务键：大写 / 空格 / 连字符都拒（否则「basic」与「Basic 」是两个等级）
	for _, code := range []string{"Basic", "basic ", "pro-plus"} {
		if err := svc.SaveLevel(ctx, 1, repo.Level{Code: code, Name: "x", Sort: 99}); bizCode(err) != errcode.InvalidArgument {
			t.Errorf("编码 %q 应被拒，得到 %v", code, err)
		}
	}

	// 套餐挂的等级必须存在
	if err := svc.SavePlan(ctx, 1, repo.Plan{LevelID: 987654321, Period: "month", PriceCents: 100}); bizCode(err) != errcode.InvalidArgument {
		t.Fatalf("挂在不存在等级上的套餐应被拒，得到 %v", err)
	}
	if err := svc.SavePlan(ctx, 1, repo.Plan{LevelID: levels[0].ID, Period: "month", PriceCents: 2900, GiftCredits: 300, IsEnabled: true}); err != nil {
		t.Fatalf("正常的套餐应能保存：%v", err)
	}

	// 积分包：积分上限、名称按字符数
	if err := svc.SavePackage(ctx, 1, repo.Package{Code: "pack_x", Name: "x", Credits: 200_000_000, PriceCents: 1}); bizCode(err) != errcode.InvalidArgument {
		t.Fatalf("积分超上限应被拒，得到 %v", err)
	}
	if err := svc.SavePackage(ctx, 1, repo.Package{Code: "pack_x", Name: "  两千积分  ", Credits: 2000, PriceCents: 2000, IsEnabled: true}); err != nil {
		t.Fatalf("正常的积分包应能保存：%v", err)
	}
	p, err := r.Package(ctx, packageIDByCode(t, r, "pack_x"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "两千积分" {
		t.Fatalf("名称应去掉首尾空白后落库，得到 %q", p.Name)
	}
}

func packageIDByCode(t *testing.T, r *repo.Repo, code string) int64 {
	t.Helper()
	list, err := r.ListPackages(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.Code == code {
			return p.ID
		}
	}
	t.Fatalf("没有编码为 %s 的积分包", code)
	return 0
}
