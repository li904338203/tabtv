//go:build integration

// 角色库数据层（MKT-T-34）的真库测试：名称唯一（lower、软删后可重名）、工作区隔离、
// 预置副本唯一、媒体引用检查器（图片 GIN + 音频）、营销角色用户端只读查询。
package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/marketing/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func setupChar(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

func newCharacter(t *testing.T, r *repo.Repo, next func() int64, ws int64, name string) repo.Character {
	t.Helper()
	c, err := r.InsertCharacter(context.Background(), repo.Character{ID: next(), WorkspaceID: ws, Name: name,
		ImageMediaIDs: []int64{next()}, CreatedBy: next()}, 200)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCharacter_重名与软删后可重名(t *testing.T) {
	r, _, next := setupChar(t)
	ctx := context.Background()
	const ws = 3001
	c := newCharacter(t, r, next, ws, "Yuzu")
	if c.Source != repo.CharacterSourceUser || c.Version != 1 {
		t.Fatalf("默认来源 user、版本 1：%+v", c)
	}
	// lower(name) 唯一：大小写不同也算重名。
	if _, err := r.InsertCharacter(ctx, repo.Character{ID: next(), WorkspaceID: ws, Name: "yuzu",
		ImageMediaIDs: []int64{1}}, 200); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("同工作区重名应 ErrDuplicate：%v", err)
	}
	// 别的工作区同名不冲突。
	newCharacter(t, r, next, 3002, "Yuzu")
	// 改名撞名同样 ErrDuplicate。
	other := newCharacter(t, r, next, ws, "小美")
	if _, err := r.UpdateCharacter(ctx, repo.Character{ID: other.ID, WorkspaceID: ws, Name: "YUZU",
		ImageMediaIDs: []int64{1}, UpdatedBy: next()}, 1); !errors.Is(err, repo.ErrDuplicate) {
		t.Fatalf("改名撞名应 ErrDuplicate：%v", err)
	}
	// 软删后可重名。
	if _, err := r.SoftDeleteCharacter(ctx, ws, c.ID, next()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.InsertCharacter(ctx, repo.Character{ID: next(), WorkspaceID: ws, Name: "yuzu",
		ImageMediaIDs: []int64{1}}, 200); err != nil {
		t.Fatalf("软删后应可重名：%v", err)
	}
	// 更新：版本进位、来源不动、乐观锁。
	u, err := r.UpdateCharacter(ctx, repo.Character{ID: other.ID, WorkspaceID: ws, Name: "小美2",
		ImageMediaIDs: []int64{7, 8}, Appearance: "圆脸", StyleCode: "anime", VoiceText: "甜", VoiceMediaID: 9,
		UpdatedBy: next()}, 1)
	if err != nil || u.Version != 2 || u.Source != repo.CharacterSourceUser || u.VoiceMediaID != 9 ||
		u.StyleCode != "anime" || len(u.ImageMediaIDs) != 2 {
		t.Fatalf("更新结果不对：%v %+v", err, u)
	}
	if _, err := r.UpdateCharacter(ctx, repo.Character{ID: other.ID, WorkspaceID: ws, Name: "x",
		ImageMediaIDs: []int64{1}}, 1); !errors.Is(err, repo.ErrStale) {
		t.Fatalf("旧版本应 ErrStale：%v", err)
	}
}

func TestCharacter_工作区隔离(t *testing.T) {
	r, _, next := setupChar(t)
	ctx := context.Background()
	const a, b = 3101, 3102
	c := newCharacter(t, r, next, a, "A 的角色")
	if _, err := r.GetCharacter(ctx, b, c.ID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("B 取 A 的角色应 ErrNotFound：%v", err)
	}
	if _, err := r.UpdateCharacter(ctx, repo.Character{ID: c.ID, WorkspaceID: b, Name: "改",
		ImageMediaIDs: []int64{1}}, 1); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("B 改 A 的角色应 ErrNotFound：%v", err)
	}
	if _, err := r.SoftDeleteCharacter(ctx, b, c.ID, 0); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("B 删 A 的角色应 ErrNotFound：%v", err)
	}
	if list, _, err := r.ListCharacters(ctx, repo.CharacterFilter{WorkspaceID: b}); err != nil || len(list) != 0 {
		t.Fatalf("B 的列表应为空：%v %d", err, len(list))
	}
	// 列表：搜索转义 + 游标翻页（name 排序）。
	newCharacter(t, r, next, a, "B_100%")
	newCharacter(t, r, next, a, "C 角色")
	if list, _, _ := r.ListCharacters(ctx, repo.CharacterFilter{WorkspaceID: a, Q: "100%"}); len(list) != 1 {
		t.Fatalf("搜索 100%% 应只命中 1 个：%d", len(list))
	}
	var names []string
	cursor := ""
	for i := 0; i < 5; i++ {
		page, more, err := r.ListCharacters(ctx, repo.CharacterFilter{WorkspaceID: a, Sort: repo.ProductSortName,
			Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range page {
			names = append(names, x.Name)
		}
		if !more {
			break
		}
		cursor = repo.EncodeCharacterCursor(repo.ProductSortName, page[len(page)-1])
	}
	if len(names) != 3 || names[0] != "A 的角色" || names[2] != "C 角色" {
		t.Fatalf("游标翻页按名称不漏不重：%v", names)
	}
}

func TestCharacter_预置副本唯一与配额(t *testing.T) {
	r, _, next := setupChar(t)
	ctx := context.Background()
	const ws = 3201
	row := repo.Character{WorkspaceID: ws, Name: "柚子君", ImageMediaIDs: []int64{1}, Source: repo.CharacterSourcePreset,
		SourcePresetID: 77, SourcePresetVersion: 2}
	row.ID = next()
	if _, err := r.InsertCharacter(ctx, row, 200); err != nil {
		t.Fatal(err)
	}
	row.ID, row.Name = next(), "柚子君2"
	if _, err := r.InsertCharacter(ctx, row, 200); !errors.Is(err, repo.ErrPresetCopyExists) {
		t.Fatalf("同 (工作区, 预置, 版本) 第二份应 ErrPresetCopyExists：%v", err)
	}
	got, err := r.GetCharacterByPreset(ctx, ws, 77, 2)
	if err != nil || got.Name != "柚子君" || got.SourcePresetVersion != 2 {
		t.Fatalf("按预置版本取副本：%v %+v", err, got)
	}
	// 新版本可以再建一份。
	row.ID, row.SourcePresetVersion = next(), 3
	if _, err := r.InsertCharacter(ctx, row, 200); err != nil {
		t.Fatalf("改版后应能再建：%v", err)
	}
	// 配额：已有 2 个，上限 2 → ErrQuota。
	if _, err := r.InsertCharacter(ctx, repo.Character{ID: next(), WorkspaceID: ws, Name: "满了",
		ImageMediaIDs: []int64{1}}, 2); !errors.Is(err, repo.ErrQuota) {
		t.Fatalf("配额满应 ErrQuota：%v", err)
	}
}

func TestCharacterMediaReferences_图片与音频(t *testing.T) {
	r, _, next := setupChar(t)
	ctx := context.Background()
	img, voice := next(), next()
	c, err := r.InsertCharacter(ctx, repo.Character{ID: next(), WorkspaceID: 3301, Name: "有图有声",
		ImageMediaIDs: []int64{next(), img}, VoiceMediaID: voice}, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, mid := range []int64{img, voice} {
		refs, err := r.CharacterMediaReferences(ctx, mid)
		if err != nil || len(refs) != 1 || refs[0] != c.ID {
			t.Fatalf("媒体 %d 应命中角色 %d：%v %v", mid, c.ID, refs, err)
		}
	}
	if refs, _ := r.CharacterMediaReferences(ctx, next()); len(refs) != 0 {
		t.Fatalf("无关媒体不应命中：%v", refs)
	}
	if _, err := r.SoftDeleteCharacter(ctx, 3301, c.ID, 0); err != nil {
		t.Fatal(err)
	}
	for _, mid := range []int64{img, voice} {
		if refs, _ := r.CharacterMediaReferences(ctx, mid); len(refs) != 0 {
			t.Fatalf("软删后不应再命中：%v", refs)
		}
	}
}

func insertPreset(t *testing.T, pool *pgxpool.Pool, id int64, name string, sort int, enabled bool, imgs string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into marketing_preset_character
		(id, name, summary, image_media_ids, sort, is_enabled) values ($1,$2,'说明',$3::jsonb,$4,$5)`,
		id, name, imgs, sort, enabled); err != nil {
		t.Fatal(err)
	}
}

func TestPreset_用户端只读启用且按排序(t *testing.T) {
	r, pool, next := setupChar(t)
	ctx := context.Background()
	a, b, off := next(), next(), next()
	insertPreset(t, pool, a, "排第二", 2, true, `["11"]`)
	insertPreset(t, pool, b, "排第一", 1, true, `["12","13"]`)
	insertPreset(t, pool, off, "停用", 0, false, `["14"]`)
	var got []string
	cursor := ""
	for {
		page, more, err := r.ListEnabledPresets(ctx, repo.PresetFilter{Cursor: cursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page {
			got = append(got, p.Name)
		}
		if !more {
			break
		}
		cursor = repo.EncodePresetCursor(page[len(page)-1])
	}
	if len(got) != 2 || got[0] != "排第一" || got[1] != "排第二" {
		t.Fatalf("只列启用、按 sort, id：%v", got)
	}
	if p, err := r.GetEnabledPreset(ctx, b); err != nil || len(p.ImageMediaIDs) != 2 || p.ImageMediaIDs[1] != 13 {
		t.Fatalf("取启用的：%v %+v", err, p)
	}
	if _, err := r.GetEnabledPreset(ctx, off); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("停用的应 ErrNotFound：%v", err)
	}
}
