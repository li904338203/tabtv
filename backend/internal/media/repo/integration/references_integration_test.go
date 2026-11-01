//go:build integration

// 删除保护的**计算式**判定（P14-03）。
//
// ⚠️ 必须用真库：四个来源各是一条 SQL，而它们要回答的是
// 「这个文件还有没有人在用」—— 答错的后果是不可逆的。
//
// 起因：`media_usage` 是**登记式**的，八个 UsageType 常量里只有两个真的有人
// 登记过（`project_cover`、`generation_output`）。脚本资产、时间线片段一条都没有，
// 于是"没人用"对几乎每一个素材都成立。这与提示词 ready/stale 是同一条教训
// （CD-36）：标记式的正确性依赖"每个改动点都记得改"，而计算式不依赖任何人的记性。
package integration

import (
	"context"
	"testing"

	canvasrepo "github.com/li904338203/tabtv/backend/internal/canvas/repo"
	genrepo "github.com/li904338203/tabtv/backend/internal/generation/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
	scriptrepo "github.com/li904338203/tabtv/backend/internal/script/repo"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// 四个来源都要能当场查出引用 —— 而这四处此前**没有一处**会往 media_usage 登记
// （脚本资产、时间线片段从来没有；封面与产物登记了，但登记式的会漏）。
func TestMediaReferences_四个来源都查得到(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ctx := context.Background()

	const mediaID = int64(990001)
	const otherMedia = int64(990002)

	// 脚本资产挂着它
	if _, err := pool.Exec(ctx, `
		insert into script_asset (id, workspace_id, asset_type, name, media_object_id, status)
		values ($1, 1, 'character', '林澈', $2, 'ready')`, gen.Next(), mediaID); err != nil {
		t.Fatal(err)
	}
	// 时间线片段用着它（时间线要挂在一个还在的项目上：项目被清理后它的时间线不再保护素材，SCT-10）
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name) values (1, 1, 1, '项目')`); err != nil {
		t.Fatal(err)
	}
	tlID, trackID := gen.Next(), gen.Next()
	if _, err := pool.Exec(ctx, `
		insert into timeline (id, workspace_id, project_id, name, width, height, fps, version)
		values ($1, 1, 1, 'T', 1920, 1080, 30, 1)`, tlID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into timeline_track (id, timeline_id, kind, idx)
		values ($1, $2, 'video', 1)`, trackID, tlID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into timeline_clip (id, track_id, idx, media_object_id,
			source_in_ms, source_out_ms, start_ms, duration_ms)
		values ($1, $2, 1, $3, 0, 5000, 0, 5000)`, gen.Next(), trackID, mediaID); err != nil {
		t.Fatal(err)
	}
	// 是某次生成的产物
	genID := gen.Next()
	if _, err := pool.Exec(ctx, `
		insert into generation_output (id, generation_id, idx, media_object_id, output_type)
		values ($1, $2, 0, $3, 'image')`, gen.Next(), genID, mediaID); err != nil {
		t.Fatal(err)
	}
	// 是某个项目的封面
	projID := gen.Next()
	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, cover_media_id, version)
		values ($1, 1, 1, 'P', $2, 1)`, projID, mediaID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		fn   func(context.Context, int64) ([]int64, error)
	}{
		{"脚本资产", scriptrepo.New(pool).MediaReferences},
		{"时间线片段", timelinerepo.New(pool, gen.Next).MediaReferences},
		{"生成产物", genrepo.New(pool).MediaReferences},
		{"项目封面", canvasrepo.New(pool).MediaReferences},
	}
	for _, c := range cases {
		refs, err := c.fn(ctx, mediaID)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(refs) != 1 {
			t.Errorf("%s 应查到 1 条引用，实际 %d 条 —— "+
				"查不到就意味着这个文件会被当成没人用的删掉", c.name, len(refs))
		}
		// 与本媒体无关的那个不能被算进来，否则所有文件都永远删不掉。
		other, err := c.fn(ctx, otherMedia)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(other) != 0 {
			t.Errorf("%s 对无关媒体报出了 %d 条引用", c.name, len(other))
		}
	}
}

// 回收站里的项目仍然算引用。
//
// ⚠️ 只算 active 的话：用户删了项目 → 删了封面图 → 30 天内恢复项目，
// 项目回来了而封面是裂的，**而这三步他每一步都做对了**。
func TestMediaReferences_回收站里的项目仍算引用(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ctx := context.Background()
	const mediaID = int64(990003)

	if _, err := pool.Exec(ctx, `
		insert into project (id, workspace_id, created_by_user_id, name, cover_media_id, version,
			status, deleted_at)
		values ($1, 1, 1, 'P', $2, 1, 'deleted', now())`, gen.Next(), mediaID); err != nil {
		t.Fatal(err)
	}
	refs, err := canvasrepo.New(pool).MediaReferences(ctx, mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatal("回收站里的项目仍在引用这张封面，不能算作没人用")
	}
}
