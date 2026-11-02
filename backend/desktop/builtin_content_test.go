package desktop

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/li904338203/tabtv/backend/internal/platform/resources"
	seeddata "github.com/li904338203/tabtv/backend/internal/platform/seed/data"
)

// 内置营销内容（TT-T-24）：首次启动全部装上并启用、有封面；第二次启动不重复插入。
func TestBoot_内置营销内容装上且幂等(t *testing.T) {
	if _, err := resources.Postgres(); err != nil {
		t.Skipf("没有 PG 二进制：%v", err)
	}
	t.Setenv("TABTV_DATA_DIR", t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	var opts struct {
		Categories []json.RawMessage `json:"categories"`
		Skills     []json.RawMessage `json:"skills"`
		Options    []json.RawMessage `json:"options"`
	}
	var drafts struct {
		Skills []json.RawMessage `json:"skills"`
	}
	var presets struct {
		PresetCharacters []json.RawMessage `json:"presetCharacters"`
	}
	for name, dst := range map[string]any{
		"marketing/options-2026-10-21.json":      &opts,
		"marketing/skills-draft-2026-11-01.json": &drafts,
		"marketing/preset-characters-draft.json": &presets,
	} {
		b, _ := fs.ReadFile(seeddata.FS, name)
		if err := json.Unmarshal(b, dst); err != nil {
			t.Fatal(err)
		}
	}

	rt, err := Boot(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Builtin.Failures) > 0 {
		t.Errorf("内置内容有失败项：%v", rt.Builtin.Failures)
	}
	db, err := pgx.Connect(ctx, rt.DB.DSN())
	if err != nil {
		t.Fatal(err)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func(label string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s：%d，期望 %d", label, got, want)
		}
	}
	check("分类", count(`select count(*) from marketing_category where is_enabled`), len(opts.Categories))
	check("选项（已启用、有封面）",
		count(`select count(*) from marketing_option where is_enabled and jsonb_array_length(cover_media_ids) > 0`),
		len(opts.Options))
	check("营销技能（有封面）",
		count(`select count(*) from agent_skill where code like 'mk\_%' and cover_media_id is not null`),
		len(opts.Skills)+len(drafts.Skills))
	// 定稿技能启用、建议稿技能装上但停用（2026-11-02 定）。
	check("营销技能（已启用）", count(`select count(*) from agent_skill where code like 'mk\_%' and is_enabled`), len(opts.Skills))
	check("预置营销角色（已启用、有图）",
		count(`select count(*) from marketing_preset_character where is_enabled and jsonb_array_length(image_media_ids) > 0`),
		len(presets.PresetCharacters))
	if n := count(`select count(*) from agent_skill where code not like 'mk\_%' and cover_media_id is null`); n != 0 {
		t.Errorf("还有 %d 个通用技能没有封面", n)
	}
	media := count(`select count(*) from media_object where workspace_id = 0 and status = 'active'`)
	db.Close(ctx)
	rt.Close(context.Background())

	// 第二次启动：不重复插入、不重复上传。
	rt2, err := Boot(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close(context.Background())
	r := rt2.Builtin
	if r.Categories+r.Skills+r.Refs+r.Options+r.Presets+r.SkillCovers != 0 || len(r.Failures) > 0 {
		t.Errorf("第二次启动不该再装：%+v", r)
	}
	db2, _ := pgx.Connect(ctx, rt2.DB.DSN())
	defer db2.Close(ctx)
	var media2 int
	_ = db2.QueryRow(ctx, `select count(*) from media_object where workspace_id = 0 and status = 'active'`).Scan(&media2)
	if media2 != media {
		t.Errorf("第二次启动多出了平台素材：%d → %d", media, media2)
	}
}
