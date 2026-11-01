//go:build integration

// 提示词版本链的真库测试。
//
// ⚠️ 必须用真库：这里要守的两条不变量都由**库的默认值与唯一索引**决定，
// 用 mock 仓储一条都测不出来。P03-05 端到端验证时真实踩到过：
// prompt_template.version 的列默认值是 1，新建草稿行于是凭空带一个
// 没有快照、内容为空的「版本 1」，管理端点回滚必得 40401（迁移 00015 修）。
package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

// 首次发布必须产出 v1。
// 若从 v2 起算，v1 就是个查不到快照的幽灵版本 —— 版本历史里列得出来、点了必 404。
func TestPublish_首发即v1且每个版本都有快照(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	if err := r.SaveDraft(ctx, next(), "storyboard_gen.system", "", "初版 {{story}}", ""); err != nil {
		t.Fatal(err)
	}
	tpl, err := r.GetPrompt(ctx, "storyboard_gen.system", "")
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Version != 0 {
		t.Fatalf("未发布过的模板应为 version 0，实际 %d", tpl.Version)
	}

	v, err := r.Publish(ctx, next(), tpl.ID, 1, "初版 {{story}}", "上线", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("首次发布应产出 v1，实际 v%d", v)
	}

	// 不变量：线上正在生效的版本号，一定能在快照表里找到。
	var missing int
	if err := pool.QueryRow(ctx, `
		select count(*) from prompt_template t
		where not exists (
			select 1 from prompt_template_version v
			where v.template_id = t.id and v.version = t.version)
		  and t.version > 0`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("有 %d 个模板的当前版本没有快照，回滚不到", missing)
	}
}

// 回滚产出的是新版本号，不是把号码改回去 ——
// 否则「线上这版是怎么来的」无从回答，同一个号码还会对应两份不同内容。
func TestRollback_版本号单调递增且记录来源(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()

	if err := r.SaveDraft(ctx, next(), "prompt_compose.system", "", "V1 {{shotDescription}}", ""); err != nil {
		t.Fatal(err)
	}
	tpl, _ := r.GetPrompt(ctx, "prompt_compose.system", "")
	if _, err := r.Publish(ctx, next(), tpl.ID, 1, "V1 {{shotDescription}}", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(ctx, next(), tpl.ID, 1, "V2 {{shotDescription}}", "", nil); err != nil {
		t.Fatal(err)
	}

	old, err := r.GetPromptVersion(ctx, tpl.ID, 1)
	if err != nil {
		t.Fatalf("v1 快照应存在: %v", err)
	}
	target := 1
	v3, err := r.Publish(ctx, next(), tpl.ID, 1, old.Content, "回滚", &target)
	if err != nil {
		t.Fatal(err)
	}
	if v3 != 3 {
		t.Fatalf("回滚应产出 v3，实际 v%d", v3)
	}

	cur, _ := r.GetPrompt(ctx, "prompt_compose.system", "")
	if cur.Content != old.Content {
		t.Fatalf("回滚后线上内容应为 v1 的内容，实际 %q", cur.Content)
	}
	list, err := r.ListPromptVersions(ctx, tpl.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("应有 3 个版本快照，实际 %d", len(list))
	}
	if list[0].RollbackOfVersion == nil || *list[0].RollbackOfVersion != 1 {
		t.Fatalf("最新版本应记下 rollback_of_version=1，实际 %v", list[0].RollbackOfVersion)
	}
}

// 模型定制优先于通用；定制下线后**立刻**回落通用，
// 而不是留下一个既不生效也删不掉的中间态。
func TestResolvePrompt_模型定制优先且可回落(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()

	for _, c := range []struct{ model, content string }{
		{"", "通用 {{shotDescription}}"},
		{"gpt_text", "定制 {{shotDescription}}"},
	} {
		if err := r.SaveDraft(ctx, next(), "prompt_compose.system", c.model, c.content, ""); err != nil {
			t.Fatal(err)
		}
		tpl, err := r.GetPrompt(ctx, "prompt_compose.system", c.model)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Publish(ctx, next(), tpl.ID, 1, c.content, "", nil); err != nil {
			t.Fatal(err)
		}
	}

	got, ok, err := r.ResolvePrompt(ctx, "prompt_compose.system", "gpt_text")
	if err != nil || !ok {
		t.Fatalf("应解析到内容: ok=%v err=%v", ok, err)
	}
	if got != "定制 {{shotDescription}}" {
		t.Fatalf("该模型应命中定制模板，实际 %q", got)
	}
	// 没有定制的模型走通用。
	got, _, _ = r.ResolvePrompt(ctx, "prompt_compose.system", "seedance_v1")
	if got != "通用 {{shotDescription}}" {
		t.Fatalf("无定制的模型应回落通用，实际 %q", got)
	}

	custom, _ := r.GetPrompt(ctx, "prompt_compose.system", "gpt_text")
	if err := r.ArchivePrompt(ctx, custom.ID); err != nil {
		t.Fatal(err)
	}
	got, _, _ = r.ResolvePrompt(ctx, "prompt_compose.system", "gpt_text")
	if got != "通用 {{shotDescription}}" {
		t.Fatalf("定制下线后应回落通用，实际 %q", got)
	}
}

// 草稿不参与运行时解析：编辑到一半的内容不能影响线上生成。
func TestResolvePrompt_草稿不生效(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()

	if err := r.SaveDraft(ctx, next(), "storyboard_gen.system", "", "草稿内容 {{story}}", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.ResolvePrompt(ctx, "storyboard_gen.system", ""); err != nil || ok {
		t.Fatalf("只有草稿时不应解析到内容: ok=%v err=%v", ok, err)
	}
}
