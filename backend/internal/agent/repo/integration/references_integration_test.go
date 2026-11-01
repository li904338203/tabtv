//go:build integration

// Agent 会话的媒体引用保护（MKT-T-23 / MKD-8）的真库测试。
//
// ⚠️ 必须用真库：判定全在 jsonb 的 `@>` 里 —— 键名或 ID 的类型（字符串 vs 数字）写错一处，
// 查询照样成功、只是永远查不到，而查不到的后果是把会话里还在用的图删掉（不可逆）。
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	mediarepo "github.com/li904338203/tabtv/backend/internal/media/repo"
)

// appendUserMessage 往会话里追加一条带 refs 的用户消息。
func appendUserMessage(t *testing.T, r *repo.Repo, pool *pgxpool.Pool, next func() int64,
	sessionID int64, refs string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := r.Append(ctx, tx, repo.Message{ID: next(), SessionID: sessionID,
		Role: "user", Content: "看看这张图", Refs: json.RawMessage(refs)}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func marketingSession(t *testing.T, r *repo.Repo, pool *pgxpool.Pool, next func() int64,
	workspaceID int64, sceneInput string) repo.Session {
	t.Helper()
	s, err := r.Create(context.Background(), pool, repo.Session{
		ID: next(), WorkspaceID: workspaceID, ProjectID: project, PayerAccountID: payer,
		CreatedByUserID: user, Title: "营销会话",
		Scene: domain.SceneMarketing, SceneInput: json.RawMessage(sceneInput),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mediaRef(id int64) string {
	return `[{"kind":"media","id":"` + strconv.FormatInt(id, 10) + `","label":"a.png","mediaKind":"image"}]`
}

func TestAgentMediaReferences_消息refs命中(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const media, other = int64(880001), int64(880002)

	s := mustSession(t, r, pool, next)
	appendUserMessage(t, r, pool, next, s.ID, mediaRef(media))
	// 同 ID 但 kind 不是 media（节点 / 脚本资产）不算：那是别的东西的 ID 空间。
	appendUserMessage(t, r, pool, next, s.ID, `[{"kind":"asset","id":"880002"}]`)

	got, err := r.GeneralMediaReferences(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != s.ID {
		t.Fatalf("创造页会话的附件应查到该会话，got=%v", got)
	}
	if got, _ := r.GeneralMediaReferences(ctx, other); len(got) != 0 {
		t.Fatalf("kind=asset 的同号 ID 不该算媒体引用，got=%v", got)
	}
	// 通用会话的附件不归营销那一栏（文案要分得开）。
	if got, _ := r.MarketingMediaReferences(ctx, media); len(got) != 0 {
		t.Fatalf("通用会话的引用不该出现在营销检查器里，got=%v", got)
	}
	// 多条消息引用同一张图只报一次会话。
	appendUserMessage(t, r, pool, next, s.ID, mediaRef(media))
	if got, _ := r.GeneralMediaReferences(ctx, media); len(got) != 1 {
		t.Fatalf("同一会话应去重，got=%v", got)
	}
}

// scene_input 单独也要查得到：不依赖首条消息的 refs（首条消息若因故没写进去，
// 会话的输入快照仍然指着这些图）。
func TestAgentMediaReferences_sceneInput商品图命中(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const product, char, video, assetChar, unrelated = int64(880101), int64(880102),
		int64(880103), int64(880104), int64(880105)

	s := marketingSession(t, r, pool, next, ws, `{
		"products":["880101"],
		"product":{"name":"保温杯","sellingPoints":"24 小时保温"},
		"characters":[{"kind":"media","id":"880102"},{"kind":"asset","id":"880104"}],
		"sourceVideo":{"kind":"media","id":"880103"},
		"picks":{},"pins":[]}`)

	for name, m := range map[string]int64{"商品图": product, "角色图": char, "源视频": video} {
		got, err := r.MarketingMediaReferences(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != s.ID {
			t.Errorf("%s 应查到营销会话，got=%v", name, got)
		}
		if g, _ := r.GeneralMediaReferences(ctx, m); len(g) != 0 {
			t.Errorf("%s 的营销引用不该出现在通用检查器里，got=%v", name, g)
		}
	}
	// kind=asset 的角色是脚本资产 ID，由脚本域保护；这里按媒体 ID 查不该命中。
	if got, _ := r.MarketingMediaReferences(ctx, assetChar); len(got) != 0 {
		t.Errorf("kind=asset 的角色不是媒体引用，got=%v", got)
	}
	if got, _ := r.MarketingMediaReferences(ctx, unrelated); len(got) != 0 {
		t.Errorf("无关媒体报出了引用，got=%v", got)
	}

	// 首条消息的 refs 也指着商品图：同一会话只报一次。
	appendUserMessage(t, r, pool, next, s.ID, mediaRef(product))
	if got, _ := r.MarketingMediaReferences(ctx, product); len(got) != 1 {
		t.Fatalf("消息与 scene_input 同时引用应去重成一个会话，got=%v", got)
	}
}

// 商品库会话（MKT-T-37）：角色快照里的参考音频 voiceMediaId 也算引用（MKT-D-55）；
// 角色主图走已有的 {"kind":"media","id"} 条件（快照上多了 characterId 等字段，@> 照样命中）。
func TestAgentMediaReferences_快照音频命中(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const mainImg, voice, charRow = int64(880201), int64(880202), int64(880203)
	s := marketingSession(t, r, pool, next, ws, `{
		"productId":"880200","productVersion":1,"products":["880204"],
		"product":{"name":"保温杯","sellingPoints":"","description":"双层"},
		"characters":[{"kind":"media","id":"880201","characterId":"880203","name":"柚子君",
		  "appearance":"圆脸","voiceText":"少年音","voiceMediaId":"880202"}],
		"sourceVideo":null,"picks":{},"pins":[]}`)
	for name, m := range map[string]int64{"角色主图": mainImg, "参考音频": voice} {
		got, err := r.MarketingMediaReferences(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != s.ID {
			t.Errorf("%s 应查到营销会话，got=%v", name, got)
		}
	}
	// characterId 是角色库的行 ID，不是媒体：同号媒体不该被当成引用。
	if got, _ := r.MarketingMediaReferences(ctx, charRow); len(got) != 0 {
		t.Errorf("characterId 不是媒体引用，got=%v", got)
	}
}

// 跨工作区查：引用判定不带工作区条件（media/app 的要求）。
func TestAgentMediaReferences_跨工作区查(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	const media = int64(880201)

	s1 := mustSession(t, r, pool, next)
	appendUserMessage(t, r, pool, next, s1.ID, mediaRef(media))
	s2, err := r.Create(ctx, pool, repo.Session{ID: next(), WorkspaceID: ws + 1, ProjectID: project + 1,
		PayerAccountID: payer, CreatedByUserID: user, Title: "另一个工作区"})
	if err != nil {
		t.Fatal(err)
	}
	appendUserMessage(t, r, pool, next, s2.ID, mediaRef(media))
	// 失败 / 取消的会话仍可打开、仍显示附件 —— 照样算引用。
	if _, err := pool.Exec(ctx, `update agent_session set state='cancelled' where id=$1`, s2.ID); err != nil {
		t.Fatal(err)
	}
	m := marketingSession(t, r, pool, next, ws+2, `{"products":["880201"],"characters":[],"pins":[]}`)

	got, err := r.GeneralMediaReferences(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("两个工作区的通用会话都应查到，got=%v", got)
	}
	gm, err := r.MarketingMediaReferences(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if len(gm) != 1 || gm[0] != m.ID {
		t.Fatalf("第三个工作区的营销会话应查到，got=%v", gm)
	}
}

// ---------------------------------------------------------------- 两道闸（媒体服务 + 真实检查器）

type refChecker struct {
	kind string
	fn   func(context.Context, int64) ([]int64, error)
}

func (c refChecker) Kind() string { return c.kind }
func (c refChecker) References(ctx context.Context, id int64) ([]int64, error) {
	return c.fn(ctx, id)
}

// mediaService 按 cmd/api、cmd/worker 的方式注册 Agent 两个检查器。
// 对象存储给 nil：被拒 / 被跳过的路径不碰存储，碰了就会 panic —— 那本身就是失败。
func mediaService(r *repo.Repo, pool *pgxpool.Pool, next func() int64) (*mediaapp.Service, *mediarepo.Repo) {
	mr := mediarepo.New(pool)
	svc := mediaapp.New(mr, nil, next)
	svc.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.AddReferenceChecker(refChecker{mediaapp.UsageAgentSession, r.GeneralMediaReferences})
	svc.AddReferenceChecker(refChecker{mediaapp.UsageAgentMarketing, r.MarketingMediaReferences})
	return svc, mr
}

func insertMedia(t *testing.T, mr *mediarepo.Repo, next func() int64, workspaceID int64) int64 {
	t.Helper()
	m := mediarepo.MediaObject{ID: next(), WorkspaceID: workspaceID, Bucket: "seeaitv",
		ObjectKey:   "upload/" + time.Now().Format("150405.000000000"),
		ContentType: "image/png", SizeBytes: 1024, Checksum: "abc",
		Purpose: "upload_reference", CreatedByUserID: user}
	if err := mr.Insert(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func TestMediaDelete_被营销会话引用拒绝(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc, mr := mediaService(r, pool, next)

	product := insertMedia(t, mr, next, ws)
	marketingSession(t, r, pool, next, ws,
		`{"products":["`+strconv.FormatInt(product, 10)+`"],"characters":[],"pins":[]}`)
	err := svc.Delete(ctx, ws, product)
	if err == nil || !strings.Contains(err.Error(), "营销会话") {
		t.Fatalf("被营销会话引用的商品图应拒绝删除并点名营销会话，got=%v", err)
	}

	attach := insertMedia(t, mr, next, ws)
	s := mustSession(t, r, pool, next)
	appendUserMessage(t, r, pool, next, s.ID, mediaRef(attach))
	err = svc.Delete(ctx, ws, attach)
	if err == nil || !strings.Contains(err.Error(), "Agent 会话") {
		t.Fatalf("创造页会话的附件应拒绝删除并点名 Agent 会话，got=%v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `select status from media_object where id=$1`, product).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("被拒的删除不该改状态，got=%s", status)
	}

	// 没人用的照常能删。
	free := insertMedia(t, mr, next, ws)
	if err := svc.Delete(ctx, ws, free); err != nil {
		t.Fatalf("无引用的媒体应能删除：%v", err)
	}
}

// 回收器：已软删、过保留期、media_usage 里没有登记，但会话还指着它 —— 必须跳过。
func TestMediaReclaim_跳过会话引用(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc, mr := mediaService(r, pool, next)

	product := insertMedia(t, mr, next, ws)
	attach := insertMedia(t, mr, next, ws)
	marketingSession(t, r, pool, next, ws,
		`{"products":["`+strconv.FormatInt(product, 10)+`"],"characters":[],"pins":[]}`)
	s := mustSession(t, r, pool, next)
	appendUserMessage(t, r, pool, next, s.ID, mediaRef(attach))
	// 模拟引用之前已经被软删（或登记式保护漏掉的历史数据）并已过保留期。
	if _, err := pool.Exec(ctx, `
		update media_object set status='deleted', deleted_at = now() - interval '30 days'
		 where id = any($1)`, []int64{product, attach}); err != nil {
		t.Fatal(err)
	}

	c := mediaapp.NewCollector(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := c.Once(ctx); n != 0 {
		t.Fatalf("会话引用中的媒体不该被回收，删了 %d 条", n)
	}
	var left int
	if err := pool.QueryRow(ctx, `select count(*) from media_object where id = any($1)`,
		[]int64{product, attach}).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 2 {
		t.Fatalf("两条媒体行都应保留，剩 %d", left)
	}
}
