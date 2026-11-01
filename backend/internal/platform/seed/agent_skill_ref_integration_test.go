//go:build integration

package seed

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	"github.com/li904338203/tabtv/backend/migrations"
)

// 迁移 00066（DRT-29）在已有数据上的行为：仍是旧版原文的「节点与连线」改成新文本，运营改过的不动，
// 别的技能的同名文档不动；Down 把新文本改回 DRT-05 之后的原文。goose 对这份文件的解析（Up / Down / Up）
// 由 migrate 的集成测试覆盖，这里直接执行两段 SQL。
func TestAgentSkillMigration_只覆盖旧版原文(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Fresh(t)
	raw, err := fs.ReadFile(migrations.FS, directorSkillRefMigration)
	if err != nil {
		t.Fatal(err)
	}
	upPart, downPart, _ := strings.Cut(string(raw), "-- +goose Down")
	stmt := func(part string) string {
		_, s, _ := strings.Cut(part, "-- +goose StatementBegin")
		s, _, _ = strings.Cut(s, "-- +goose StatementEnd")
		return s
	}
	insert := func(id int64, code string) {
		if _, err := pool.Exec(ctx, `insert into agent_skill (id, code, name, category, body)
			values ($1, $2, $2, 'canvas', '正文')`, id, code); err != nil {
			t.Fatal(err)
		}
	}
	insert(1, "canvas_basics")
	insert(2, "other_skill")
	insert(3, "edited_skill")
	refs := map[int64]struct {
		skill int64
		body  string
	}{
		11: {1, legacyRefV1()},       // DRT-05 之前 seed 的原文 → 改
		12: {2, legacyRefV2()},       // 别的技能的同名文档 → 不动
		13: {3, legacyRefV2() + "改"}, // （另一个技能）运营改过 → 不动
	}
	for id, r := range refs {
		if _, err := pool.Exec(ctx, `insert into agent_skill_reference (id, skill_id, name, body)
			values ($1, $2, '节点与连线', $3)`, id, r.skill, r.body); err != nil {
			t.Fatal(err)
		}
	}
	body := func(id int64) string {
		var b string
		if err := pool.QueryRow(ctx, `select body from agent_skill_reference where id = $1`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}

	if _, err := pool.Exec(ctx, stmt(upPart)); err != nil {
		t.Fatalf("Up：%v", err)
	}
	// 00066 写进去的是 DRT-29 那一版（之后 00068 / 00072 / 00073 依次接着改），不是当前生成器的输出
	if body(11) != legacyRefV3() || body(12) != legacyRefV2() || body(13) != legacyRefV2()+"改" {
		t.Fatal("Up 应只改 canvas_basics 里仍是旧原文的那一行")
	}
	// 运营改过的 canvas_basics 不动。
	if _, err := pool.Exec(ctx, `update agent_skill_reference set body = $1 where id = 11`, "运营改过的正文"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, stmt(upPart)); err != nil || body(11) != "运营改过的正文" {
		t.Fatalf("运营改过的不应被覆盖：%v %q", err, body(11))
	}
	// Down：新文本 → DRT-05 之后的原文；其余不动。
	if _, err := pool.Exec(ctx, `update agent_skill_reference set body = $1 where id = 11`, legacyRefV3()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, stmt(downPart)); err != nil {
		t.Fatalf("Down：%v", err)
	}
	if body(11) != legacyRefV2() || body(13) != legacyRefV2()+"改" {
		t.Fatal("Down 应把新文本改回 DRT-05 之后的原文")
	}
}
