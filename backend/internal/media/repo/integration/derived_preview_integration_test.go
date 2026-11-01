//go:build integration

// 剪辑台的预览素材（docs/agent/smartcut SCT-10，SCD-37、SC-AC-63）：不进「我的素材」，源删除时一起软删，
// 源已经不在时随回收处理；宫格切块与共享导入不受影响（它们是独立素材，NEW-16）。
package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

func TestDerivedPreview_不进素材列表_随源软删与回收(t *testing.T) {
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	ctx := context.Background()
	r := mediarepo.New(pool)
	const ws = int64(7)
	insert := func(contentType string, source int64, kind string) int64 {
		t.Helper()
		mid := gen.Next()
		var src, dk any
		if source != 0 {
			src, dk = source, kind
		}
		if _, err := pool.Exec(ctx, `insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
			purpose, created_by_user_id, source_media_id, derive_kind, derive_index)
			values ($1,$2,'b',$3,$4,10,'upload_reference',1,$5,$6,0)`, mid, ws, fmt.Sprintf("k/%d", mid), contentType, src, dk); err != nil {
			t.Fatal(err)
		}
		return mid
	}
	video := insert("video/mp4", 0, "")
	proxy := insert("video/mp4", video, "preview_proxy")
	strip := insert("image/jpeg", video, "filmstrip")
	speech := insert("audio/mpeg", video, "speech_audio")
	tile := insert("image/png", video, "grid_tile")

	list, err := r.ListByWorkspace(ctx, ws, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int64]bool{}
	for _, m := range list {
		listed[m.ID] = true
	}
	if !listed[video] || !listed[tile] || listed[proxy] || listed[strip] || listed[speech] {
		t.Fatalf("预览版 / 拼图 / 音轨不进素材列表，源与宫格切块照常：%v", listed)
	}

	if err := r.SoftDelete(ctx, ws, video); err != nil {
		t.Fatal(err)
	}
	status := func(mid int64) string {
		var s string
		_ = pool.QueryRow(ctx, `select status from media_object where id = $1`, mid).Scan(&s)
		return s
	}
	for _, mid := range []int64{proxy, strip, speech} {
		if status(mid) != "deleted" {
			t.Errorf("派生 %d 应随源一起软删", mid)
		}
	}
	if status(tile) != "active" {
		t.Error("宫格切块是独立素材，不随源删除")
	}

	// 过了保留期：源与三种预览一起可回收；宫格切块不在其中。
	collect := func() map[int64]bool {
		got, err := r.FindCollectable(ctx, time.Now().Add(time.Hour), 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]bool{}
		for _, m := range got {
			out[m.ID] = true
		}
		return out
	}
	c := collect()
	if !c[video] || !c[proxy] || !c[strip] || !c[speech] || c[tile] {
		t.Fatalf("回收清单：%v", c)
	}

	// 源已经被永久删掉、预览却晚到（还是 active）：随回收处理，不必等保留期。
	// （先删掉已回收的那份旧预览：同一个源同一种派生只有一份，uk_media_derive。）
	if _, err := pool.Exec(ctx, `delete from media_object where id = $1`, proxy); err != nil {
		t.Fatal(err)
	}
	late := insert("video/mp4", video, "preview_proxy")
	if _, err := pool.Exec(ctx, `delete from media_object where id = $1`, video); err != nil {
		t.Fatal(err)
	}
	got, err := r.FindCollectable(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range got {
		found = found || m.ID == late
		if m.ID == tile {
			t.Fatal("源没了的宫格切块仍是独立素材，不回收")
		}
	}
	if !found {
		t.Fatal("源已不在的预览版应随回收处理")
	}
}
