//go:build integration

package seed

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// 内置版本规则（TT-T-24）：没被用户改过的内置条目随内置内容更新；改过的不动。
func TestBuiltinRule_未修改的跟随内置_改过的不动(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Fresh(t)
	db, err := sql.Open("pgx", testsupport.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n := int64(1)
	next := func() int64 { n++; return 900000 + n }
	if err := Apply(ctx, db, next); err != nil {
		t.Fatal(err)
	}
	skill, preset := agentSkillSeeds()[0].code, presets[0].code

	// 模拟「内置内容已经更新过一版」：把两行改成旧内容、旧哈希（没被用户改过）。
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`update agent_skill set body = '旧版正文', builtin_hash = 'old' where code = $1`, skill)
	mustExec(`update image_preset set prompt = '旧版提示词', builtin_hash = 'old' where code = $1`, preset)
	// 另一条被用户改过。
	other := agentSkillSeeds()[1].code
	mustExec(`update agent_skill set body = '用户的正文', is_user_modified = true where code = $1`, other)

	if err := Apply(ctx, db, next); err != nil {
		t.Fatal(err)
	}
	var body, prompt, otherBody string
	_ = pool.QueryRow(ctx, `select body from agent_skill where code = $1`, skill).Scan(&body)
	_ = pool.QueryRow(ctx, `select prompt from image_preset where code = $1`, preset).Scan(&prompt)
	_ = pool.QueryRow(ctx, `select body from agent_skill where code = $1`, other).Scan(&otherBody)
	if body == "旧版正文" || prompt == "旧版提示词" {
		t.Fatalf("没被改过的内置条目应跟随内置内容：body=%q prompt=%q", body, prompt)
	}
	if otherBody != "用户的正文" {
		t.Fatalf("用户改过的不应被覆盖：%q", otherBody)
	}
}
