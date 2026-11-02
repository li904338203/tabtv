//go:build integration

package seed

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	agentrepo "github.com/li904338203/tabtv/backend/internal/agent/repo"
	catalogrepo "github.com/li904338203/tabtv/backend/internal/catalog/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

// 「恢复内置版本」（TT-T-19/20）取的内置内容与哈希必须与 seed 写库时逐字一致：
// 不一致的话恢复之后下一次启动 seed 认不出它是最新内置版本，又覆盖一遍（版本白白 +1），
// 而设置页上的「已修改」标识也会对不上。
func TestBuiltinLookup_哈希与seed写库一致(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Fresh(t)
	db, err := sql.Open("pgx", testsupport.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n := int64(1)
	if err := Apply(ctx, db, func() int64 { n++; return 800000 + n }); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `select code, builtin_hash from image_preset where builtin_hash is not null`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var code, hash string
		if err := rows.Scan(&code, &hash); err != nil {
			t.Fatal(err)
		}
		p, ok := LookupBuiltinPreset(code)
		if !ok || p.Hash != hash {
			t.Errorf("预设 %s：查得到=%v 哈希一致=%v", code, ok, p.Hash == hash)
		}
		count++
	}
	rows.Close()
	if count == 0 {
		t.Fatal("seed 没写任何内置预设")
	}
	rows, err = pool.Query(ctx, `select code, builtin_hash from agent_skill where builtin_hash is not null`)
	if err != nil {
		t.Fatal(err)
	}
	count = 0
	for rows.Next() {
		var code, hash string
		if err := rows.Scan(&code, &hash); err != nil {
			t.Fatal(err)
		}
		s, ok := LookupBuiltinSkill(code)
		if !ok || s.Hash != hash {
			t.Errorf("技能 %s：查得到=%v 哈希一致=%v", code, ok, s.Hash == hash)
		}
		count++
	}
	rows.Close()
	if count == 0 {
		t.Fatal("seed 没写任何内置技能")
	}
	if _, ok := LookupBuiltinSkill("no_such_skill"); ok {
		t.Fatal("不存在的技能不该查得到")
	}
}

// 技能「恢复内置版本」端到端：用户改过（含加了一份附属文档）→ 恢复 → 回到内置内容、清已修改、version +1，
// 后加的附属文档删掉；随后再跑一次 seed 不再动它（认得出是最新内置版本）。
func TestSkillRestore_回到内置且seed认得(t *testing.T) {
	ctx := context.Background()
	pool := testsupport.Fresh(t)
	db, err := sql.Open("pgx", testsupport.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n := int64(1)
	next := func() int64 { n++; return 700000 + n }
	if err := Apply(ctx, db, next); err != nil {
		t.Fatal(err)
	}
	r := agentrepo.New(pool)
	var code string
	for _, s := range agentSkillSeeds() {
		if len(s.refs) > 0 {
			code = s.code
			break
		}
	}
	if code == "" {
		code = agentSkillSeeds()[0].code
	}
	cur, err := r.GetSkillByCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if !cur.IsBuiltin || cur.IsUserModified {
		t.Fatalf("seed 刚装好应为「内置、未修改」：%+v", cur)
	}
	cur.Body = "用户改过的正文"
	cur, err = r.UpdateSkill(ctx, cur, cur.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveSkillReference(ctx, agentrepo.SkillReference{ID: next(), SkillID: cur.ID, Name: "用户加的",
		Body: "x"}, true, cur.Version); err != nil {
		t.Fatal(err)
	}
	cur, _ = r.GetSkillByID(ctx, cur.ID)
	if !cur.IsUserModified {
		t.Fatal("修改后应标「已修改」")
	}

	b, _ := LookupBuiltinSkill(code)
	c := agentrepo.SkillRestore{Name: b.Name, Category: b.Category, Description: b.Description,
		Tags: json.RawMessage(b.Tags), Steps: json.RawMessage(b.Steps), Tools: json.RawMessage(b.Tools),
		Body: b.Body, QuickEntry: b.QuickEntry, Sort: b.Sort, Hash: b.Hash}
	for _, f := range b.Refs {
		c.Refs = append(c.Refs, agentrepo.SkillReference{ID: next(), Name: f.Name, Body: f.Body, Sort: f.Sort})
	}
	out, err := r.RestoreSkill(ctx, cur.ID, c)
	if err != nil {
		t.Fatal(err)
	}
	if out.Body != b.Body || out.IsUserModified || out.Version != cur.Version+1 {
		t.Fatalf("恢复结果不对：modified=%v version=%d→%d", out.IsUserModified, cur.Version, out.Version)
	}
	refs, _ := r.ListSkillReferences(ctx, cur.ID)
	if len(refs) != len(b.Refs) {
		t.Fatalf("附属文档应同步成内置那一套（%d 份），得到 %d 份", len(b.Refs), len(refs))
	}
	if _, err := r.RestoreSkill(ctx, 424242, c); err == nil {
		t.Fatal("不存在的技能应报错")
	}

	if err := Apply(ctx, db, next); err != nil {
		t.Fatal(err)
	}
	again, _ := r.GetSkillByID(ctx, cur.ID)
	if again.Version != out.Version {
		t.Fatalf("恢复后 seed 又覆盖了一遍（version %d→%d）：哈希没对上", out.Version, again.Version)
	}

	// 预设同理：恢复后 seed 不再动它。
	cr := catalogrepo.New(pool)
	pcode := presets[0].code
	p, _ := LookupBuiltinPreset(pcode)
	cp := catalogrepo.AdminImagePreset{Code: p.Code, Name: p.Name, PresetGroup: p.Group, Spec: p.Spec,
		InputMode: p.InputMode, Icon: p.Icon, Prompt: p.Prompt, Params: json.RawMessage(p.Params),
		Operation: p.Operation, Sort: p.Sort, Description: p.Description, IsNew: p.IsNew}
	if p.Dimensions != "" {
		cp.Dimensions = json.RawMessage(p.Dimensions)
	}
	edited := cp
	edited.Prompt = "改过"
	if err := cr.SaveImagePreset(ctx, next(), edited, false); err != nil {
		t.Fatal(err)
	}
	if err := cr.RestoreImagePreset(ctx, cp, p.Hash); err != nil {
		t.Fatal(err)
	}
	got, err := cr.GetAdminImagePreset(ctx, pcode)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != p.Prompt || got.IsUserModified || !got.IsBuiltin {
		t.Fatalf("预设恢复结果不对：%+v", got)
	}
}
