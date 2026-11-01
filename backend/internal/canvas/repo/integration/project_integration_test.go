//go:build integration

// 项目/文件夹/回收站的真库测试。
//
// ⚠️ 必须用真库：这里要守的几条都由 **SQL 本身**决定 ——
// 游标分页的漏行与重复、workspace 过滤、软删条件、保留期按 deleted_at 起算。
// mock 仓储测的只是「我写的 Go 有没有按我想的调用」，测不出 SQL 对不对。
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	"github.com/li904338203/tabtv/backend/internal/canvas/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/testsupport"
)

const ws = int64(9001) // 本测试的工作区；另起一个用来验越权

func setup(t *testing.T) (*repo.Repo, *pgxpool.Pool, func() int64) {
	t.Helper()
	pool := testsupport.Fresh(t)
	gen, _ := id.New(0)
	return repo.New(pool), pool, gen.Next
}

func mustInsert(t *testing.T, r *repo.Repo, next func() int64, workspace int64, name string) int64 {
	t.Helper()
	p := repo.Project{ID: next(), WorkspaceID: workspace, CreatedBy: 1, Name: name}
	if err := r.Insert(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// 游标分页的真实契约：**不重复**，且不漏掉排序键没变的行。
//
// ⚠️ 排序键在翻页途中被改掉的那一行**可以**被漏掉，这是正确行为而不是 bug：
// 它跳到了游标前面（已经翻过的区间），本轮扫描不会再看到它。
// 没有任何分页方案能在排序键变动时既不重不漏 —— offset 在同样场景下会
// **重复**已展示的行，那更糟：用户看到两个一模一样的项目，还以为自己复制过。
// 这条断言存在的意义是拦住「为了修这个漏行而换回 offset」的改动。
func TestPage_游标翻页不重复且不漏未改动的行(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()

	const total = 7
	ids := make([]int64, 0, total)
	for i := 0; i < total; i++ {
		ids = append(ids, mustInsert(t, r, next, ws, fmt.Sprintf("项目-%d", i)))
		// 拉开 updated_at，避免同一微秒内插入导致排序不稳定。
		time.Sleep(2 * time.Millisecond)
	}
	// ids[0] 最早插入，按 updated_at 倒序排在最后一页。
	moved := ids[0]

	seen := map[int64]int{}
	q := repo.PageQuery{WorkspaceID: ws, Limit: 3}
	pages := 0
	for {
		rows, err := r.Page(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, p := range rows {
			seen[p.ID]++
		}
		if len(rows) < q.Limit {
			break
		}
		last := rows[len(rows)-1]
		q.CursorTime, q.CursorID = last.UpdatedAt, last.ID
		// 翻完第 1 页后把最老的一条改名：它的 updated_at 跳到最新，
		// 也就是跳到**游标前面**。
		if pages == 1 {
			if _, err := r.UpdateMeta(ctx, ws, moved, strPtr("被改过的项目"), nil, nil); err != nil {
				t.Fatal(err)
			}
		}
		if pages > 10 {
			t.Fatal("翻页不收敛")
		}
	}

	for _, id := range ids {
		if seen[id] > 1 {
			t.Errorf("项目 %d 出现 %d 次 —— 游标分页绝不允许重复", id, seen[id])
		}
		if id != moved && seen[id] == 0 {
			t.Errorf("项目 %d 没被改动过却漏掉了", id)
		}
	}
	if seen[moved] != 0 {
		// 不是错误，只是记录实际行为，便于以后改排序键时对照。
		t.Logf("被改动的行仍出现了 %d 次（取决于它跳到游标的哪一侧）", seen[moved])
	}
}

// 每条查询都必须带 workspace_id：只靠中间件校验成员资格是不够的，
// 用户可以带着自己工作区的 ID 去访问别人工作区的资源 ID（CD-04 第 2 条）。
func TestWorkspace_隔离(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	other := ws + 1
	id := mustInsert(t, r, next, ws, "别人的项目")

	if _, err := r.Get(ctx, other, id); err == nil {
		t.Error("用别的工作区 ID 居然查到了项目")
	}
	if err := r.SoftDelete(ctx, other, id); err == nil {
		t.Error("用别的工作区 ID 居然删掉了项目")
	}
	if _, err := r.UpdateMeta(ctx, other, id, strPtr("改名"), nil, nil); err == nil {
		t.Error("用别的工作区 ID 居然改掉了项目")
	}
	// 本工作区仍然正常。
	if _, err := r.Get(ctx, ws, id); err != nil {
		t.Errorf("本工作区应能访问: %v", err)
	}
}

// 软删 → 恢复 → 再删。重复软删不得刷新 deleted_at，否则保留期永远重新开始。
func TestTrash_软删恢复与重复删除(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	id := mustInsert(t, r, next, ws, "会被删的项目")

	if err := r.SoftDelete(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err := pool.QueryRow(ctx, `select deleted_at from project where id=$1`, id).Scan(&first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := r.SoftDelete(ctx, ws, id); err == nil {
		t.Error("重复软删应返回未找到，而不是再删一次")
	}
	var second time.Time
	_ = pool.QueryRow(ctx, `select deleted_at from project where id=$1`, id).Scan(&second)
	if !first.Equal(second) {
		t.Error("重复软删刷新了 deleted_at —— 保留期会被无限推迟")
	}

	// 活动列表看不到它，回收站看得到。
	active, _ := r.Page(ctx, repo.PageQuery{WorkspaceID: ws, Limit: 10})
	if len(active) != 0 {
		t.Errorf("活动列表不该有已删项目，实际 %d 条", len(active))
	}
	trash, _ := r.Page(ctx, repo.PageQuery{WorkspaceID: ws, Limit: 10, Trash: true})
	if len(trash) != 1 {
		t.Fatalf("回收站应有 1 条，实际 %d", len(trash))
	}

	p, err := r.Restore(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "active" || p.DeletedAt != nil {
		t.Errorf("恢复后应为 active 且 deleted_at 为空，实际 %+v", p)
	}
}

// 恢复时原文件夹若已被删，项目要落回未分类 ——
// 否则它挂在一个查不到的 folder_id 上，在任何视图里都看不到，等于消失。
func TestRestore_原文件夹已删时落回未分类(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	f, err := r.InsertFolder(ctx, next(), ws, "临时夹")
	if err != nil {
		t.Fatal(err)
	}
	id := mustInsert(t, r, next, ws, "夹内项目")
	if _, err := r.UpdateMeta(ctx, ws, id, nil, ptrPtr(f.ID), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.SoftDelete(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	// 项目在回收站期间，文件夹被删了。
	if err := r.DeleteFolder(ctx, ws, f.ID); err != nil {
		t.Fatal(err)
	}
	p, err := r.Restore(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.FolderID != 0 {
		t.Errorf("原文件夹已删，应落回未分类，实际 folderID=%d", p.FolderID)
	}
}

// 删文件夹不删项目：文件夹是归类方式不是容器，连带删会把一次误点变成丢数据。
func TestDeleteFolder_项目落回未分类(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	f, _ := r.InsertFolder(ctx, next(), ws, "短片")
	id := mustInsert(t, r, next, ws, "夹内项目")
	if _, err := r.UpdateMeta(ctx, ws, id, nil, ptrPtr(f.ID), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteFolder(ctx, ws, f.ID); err != nil {
		t.Fatal(err)
	}
	p, err := r.Get(ctx, ws, id)
	if err != nil {
		t.Fatalf("项目不该跟着消失: %v", err)
	}
	if p.FolderID != 0 {
		t.Errorf("项目应落回未分类，实际 folderID=%d", p.FolderID)
	}
}

func TestFolder_同工作区重名被拒(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	if _, err := r.InsertFolder(ctx, next(), ws, "短片"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.InsertFolder(ctx, next(), ws, "短片"); err == nil {
		t.Error("同工作区重名应被唯一索引拒绝")
	}
	// 另一个工作区可以同名。
	if _, err := r.InsertFolder(ctx, next(), ws+1, "短片"); err != nil {
		t.Errorf("跨工作区同名应允许: %v", err)
	}
}

// 到期清理只能按 deleted_at 判定，且不能误删未到期或未删除的项目。
func TestPurgeExpired_只清到期的(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	alive := mustInsert(t, r, next, ws, "活着的项目")
	fresh := mustInsert(t, r, next, ws, "刚删的项目")
	stale := mustInsert(t, r, next, ws, "删了很久的项目")
	for _, id := range []int64{fresh, stale} {
		if err := r.SoftDelete(ctx, ws, id); err != nil {
			t.Fatal(err)
		}
	}
	// 把一条的删除时间调到 31 天前；同时把它的 updated_at 设成"刚刚"，
	// 模拟「删了又恢复又删」——若按 updated_at 判定，它永远不会被清掉。
	if _, err := pool.Exec(ctx, `
		update project set deleted_at = now() - interval '31 days', updated_at = now()
		where id = $1`, stale); err != nil {
		t.Fatal(err)
	}

	ids, _, err := r.PurgeExpired(ctx, time.Now().Add(-30*24*time.Hour), 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != stale {
		t.Fatalf("应只清掉过期那条，实际 %v", ids)
	}
	if _, err := r.Get(ctx, ws, alive); err != nil {
		t.Error("活动项目被误删")
	}
	if _, err := r.Get(ctx, ws, fresh); err != nil {
		t.Error("未到期的回收站项目被误删")
	}
}

// Cleaner 跑一轮应把过期项目清空，并且可重复执行（第二轮清 0 条）。
func TestCleaner_一轮清空且可重复执行(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		id := mustInsert(t, r, next, ws, fmt.Sprintf("过期项目-%d", i))
		_ = r.SoftDelete(ctx, ws, id)
		if _, err := pool.Exec(ctx,
			`update project set deleted_at = now() - interval '40 days' where id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	svc := canvasapp.New(r, next)
	c := canvasapp.NewCleaner(svc, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	if n := c.Once(ctx); n != 3 {
		t.Fatalf("首轮应清 3 条，实际 %d", n)
	}
	if n := c.Once(ctx); n != 0 {
		t.Fatalf("再跑一轮应清 0 条，实际 %d", n)
	}
}

// 复制走的是 app 层：副本必须解绑产物、保留未知字段、且不产生任何账本或生成记录。
func TestDuplicate_不复制账本与生成(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)

	src := repo.Project{
		ID: next(), WorkspaceID: ws, CreatedBy: 1, Name: "原项目",
		CanvasData: json.RawMessage(
			`{"未来字段":1,"nodes":[{"id":"n1","result":{"id":"7001","outputUrl":"u"}}]}`),
	}
	if err := r.Insert(ctx, src); err != nil {
		t.Fatal(err)
	}

	cp, err := svc.Duplicate(ctx, ws, 1, src.ID, collab.RoleOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.GetWithCanvas(ctx, ws, cp.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tree map[string]any
	_ = json.Unmarshal(got.CanvasData, &tree)
	result := tree["nodes"].([]any)[0].(map[string]any)["result"].(map[string]any)
	if _, ok := result["id"]; ok {
		t.Error("副本仍绑着原项目的 generationId")
	}
	if _, ok := tree["未来字段"]; !ok {
		t.Error("未知字段在复制中丢了")
	}

	for _, tbl := range []string{"generation", "credit_transaction"} {
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from `+tbl).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("复制不该产生 %s 记录，实际 %d 行", tbl, n)
		}
	}

	// viewer 不能复制（复制是写操作）。
	if _, err := svc.Duplicate(ctx, ws, 1, src.ID, collab.RoleViewer, ""); err == nil {
		t.Error("viewer 不该能复制项目")
	}
}

func strPtr(s string) *string { return &s }

func ptrPtr(v int64) **int64 {
	p := &v
	return &p
}

// 乐观锁：两端拿着同一个 version 保存，**只有一个能成功**（AC-03）。
func TestSaveCanvas_并发保存只有一个成功(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	id := mustInsert(t, r, next, ws, "并发编辑的项目")

	p, _ := r.Get(ctx, ws, id)
	base := p.Version

	doc := func(tag string) json.RawMessage {
		return json.RawMessage(`{"nodes":[{"id":"` + tag + `","type":"image"}],"edges":[]}`)
	}

	const editorA, editorB = int64(7001), int64(7002)
	st1, err1 := r.SaveCanvas(ctx, ws, id, base, editorA, doc("tabA"))
	if err1 != nil {
		t.Fatalf("第一个保存应成功: %v", err1)
	}
	if st1.Version != base+1 {
		t.Fatalf("版本应 +1，实际 %d → %d", base, st1.Version)
	}

	// 第二个标签页手里还是 base。
	cur, err2 := r.SaveCanvas(ctx, ws, id, base, editorB, doc("tabB"))
	if !errors.Is(err2, repo.ErrVersionConflict) {
		t.Fatalf("第二个保存应冲突，实际 %v", err2)
	}
	// 冲突时要回报服务端当前版本，客户端据此决定重载还是放弃。
	if cur.Version != st1.Version {
		t.Errorf("冲突应带回当前版本 %d，实际 %d", st1.Version, cur.Version)
	}
	// ⚠️ 还要带回**是谁改的**（P12-02、NEW-19）：只给版本号的话，
	// 用户分不出"我的另一个标签页"与"同事"，而这两种情况他要做的事完全不同。
	if cur.EditorID != editorA {
		t.Errorf("冲突应带回最后编辑者 %d，实际 %d", editorA, cur.EditorID)
	}

	// ⚠️ 冲突方的内容**绝不能**落库：那正是"几分钟的工作被静默覆盖"。
	got, _ := r.GetWithCanvas(ctx, ws, id)
	if !strings.Contains(string(got.CanvasData), "tabA") {
		t.Errorf("库里应还是先保存的那份，实际 %s", got.CanvasData)
	}
}

// 0 行更新有两种原因，必须分开回报 ——
// 一律报冲突的话，用户会对着一个已被删掉的项目反复点"重新加载"。
func TestSaveCanvas_已删项目报未找到而非冲突(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	id := mustInsert(t, r, next, ws, "会被删的项目")
	p, _ := r.Get(ctx, ws, id)
	if err := r.SoftDelete(ctx, ws, id); err != nil {
		t.Fatal(err)
	}

	_, err := r.SaveCanvas(ctx, ws, id, p.Version, 1, json.RawMessage(`{"nodes":[],"edges":[]}`))
	if !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("回收站里的项目保存应报未找到，实际 %v", err)
	}

	// 跨工作区同理：不能借别人的项目 ID 写入。
	id2 := mustInsert(t, r, next, ws, "别人的项目")
	p2, _ := r.Get(ctx, ws, id2)
	if _, err := r.SaveCanvas(ctx, ws+1, id2, p2.Version, 1,
		json.RawMessage(`{"nodes":[],"edges":[]}`)); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("跨工作区保存应报未找到，实际 %v", err)
	}
}

// 保存的是**原始字节**：后端没定义的字段必须原样躺在库里。
func TestSaveCanvas_保留未知字段(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	id := mustInsert(t, r, next, ws, "带未来字段的项目")
	p, _ := r.Get(ctx, ws, id)

	raw := json.RawMessage(`{"schemaVersion":2,"未来字段":{"deep":[1,{"x":true}]},
		"nodes":[{"id":"n1","type":"image","未来参数":1}],"edges":[]}`)
	if _, err := r.SaveCanvas(ctx, ws, id, p.Version, 1, raw); err != nil {
		t.Fatal(err)
	}

	got, _ := r.GetWithCanvas(ctx, ws, id)
	var tree map[string]any
	if err := json.Unmarshal(got.CanvasData, &tree); err != nil {
		t.Fatal(err)
	}
	if _, ok := tree["未来字段"]; !ok {
		t.Error("顶层未知字段没了")
	}
	node := tree["nodes"].([]any)[0].(map[string]any)
	if _, ok := node["未来参数"]; !ok {
		t.Error("节点里的未知字段没了")
	}
	if tree["schemaVersion"].(float64) != 2 {
		t.Error("schemaVersion 应原样保留")
	}
}

// 改名/移动文件夹**不得**推进乐观锁版本，否则另一个正在编辑画布的标签页
// 会凭空撞冲突、被迫丢掉草稿。
func TestUpdateMeta_不推进画布版本(t *testing.T) {
	r, _, next := setup(t)
	ctx := context.Background()
	id := mustInsert(t, r, next, ws, "改名不影响画布")
	p, _ := r.Get(ctx, ws, id)

	if _, err := r.UpdateMeta(ctx, ws, id, strPtr("新名字"), nil, nil); err != nil {
		t.Fatal(err)
	}
	// 用改名前的 version 保存，仍应成功。
	if _, err := r.SaveCanvas(ctx, ws, id, p.Version, 1,
		json.RawMessage(`{"nodes":[],"edges":[]}`)); err != nil {
		t.Fatalf("改名后画布保存不该冲突: %v", err)
	}
}

// 冲突要能说出**是谁**改的（P12-02、NEW-19）。
//
// 只给版本号的话，用户看到「画布已被其他窗口修改」而分不出两种情况：
// 自己的另一个标签页（直接覆盖就行）与同事（覆盖等于抹掉别人的工作）。
func TestCanvasState_带出最后编辑者(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	id := mustInsert(t, r, next, ws, "要查编辑者的项目")

	// 历史项目（P12-02 之前保存的）last_editor_user_id 是空的 ——
	// ⚠️ 这里必须用 left join：内连接会让整行消失，调用方拿到 ErrNotFound，
	// 界面会说"项目不存在"，一句和"版本冲突"完全不同的话。
	st, err := r.CanvasState(ctx, ws, id)
	if err != nil {
		t.Fatalf("没有编辑者的项目也要查得出来：%v", err)
	}
	if st.EditorID != 0 || st.EditorName != "" {
		t.Fatalf("尚无编辑者时应为空，得到 %+v", st)
	}

	editor := next()
	if _, err := pool.Exec(ctx, `
		insert into app_user (id, phone, nickname, password_hash)
		values ($1, $2, '林同学', 'x')`, editor, "139"+itoa10(editor%100000000)); err != nil {
		t.Fatal(err)
	}
	p, _ := r.Get(ctx, ws, id)
	if _, err := r.SaveCanvas(ctx, ws, id, p.Version, editor,
		json.RawMessage(`{"nodes":[],"edges":[]}`)); err != nil {
		t.Fatal(err)
	}
	st, err = r.CanvasState(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if st.EditorID != editor || st.EditorName != "林同学" {
		t.Fatalf("应带出编辑者，得到 %+v", st)
	}

	// 对账查询同样受租户约束：别人的工作区查不到。
	if _, err := r.CanvasState(ctx, ws+1, id); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("跨工作区对账应报未找到，实际 %v", err)
	}
}

func itoa10(v int64) string { return strconv.FormatInt(v, 10) }

// ── 展示封面（迁移 00030 的 project_display_cover）──────────────────────
//
// ⚠️ 这条规则**只能在真库上验**：它整个就是一段 SQL（显式优先 / 只认成功的图片 /
// 取最近一张 / 跳过已删除的媒体），mock 仓储测不出任何一条。

// seedMedia 造一个媒体对象，返回 ID。
func seedMedia(t *testing.T, pool *pgxpool.Pool, next func() int64, workspace int64, status string) int64 {
	t.Helper()
	mid := next()
	if _, err := pool.Exec(context.Background(), `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
		                          purpose, created_by_user_id, status)
		values ($1, $2, 'seeaitv', $3, 'image/png', 1, 'generation_output', 1, $4)`,
		mid, workspace, fmt.Sprintf("test/%d.png", mid), status); err != nil {
		t.Fatal(err)
	}
	return mid
}

// seedGeneration 造一次生成与它的一个产物。`ago` 控制先后（越大越早）。
// mediaID 为 0 时产物是文本（text 产物没有媒体对象）。
func seedGeneration(t *testing.T, pool *pgxpool.Pool, next func() int64, project int64,
	status, resultType string, mediaID int64, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	gid := next()
	if _, err := pool.Exec(ctx, `
		insert into generation (id, workspace_id, payer_account_id, created_by_user_id, project_id,
		                        operation_id, model_id, result_type, idempotency_key, status, created_at)
		values ($1, $2, 1, 1, $3, 1, 1, $4, $5, $6, now() - $7::interval)`,
		gid, ws, project, resultType, strconv.FormatInt(gid, 10), status,
		fmt.Sprintf("%d seconds", int(ago.Seconds()))); err != nil {
		t.Fatal(err)
	}
	if status != "success" {
		return // 没成功的生成没有产物
	}
	outType := resultType
	var media any
	var text any
	if mediaID != 0 {
		media = mediaID
	} else {
		text = "一段文本"
	}
	if _, err := pool.Exec(ctx, `
		insert into generation_output (id, generation_id, idx, media_object_id, output_type, text_content)
		values ($1, $2, 0, $3, $4, $5)`, next(), gid, media, outType, text); err != nil {
		t.Fatal(err)
	}
}

func displayCover(t *testing.T, r *repo.Repo, project int64) (explicit, display int64) {
	t.Helper()
	p, err := r.Get(context.Background(), ws, project)
	if err != nil {
		t.Fatal(err)
	}
	return p.CoverMedia, p.DisplayCover
}

func TestDisplayCover_只认成功的图片产出(t *testing.T) {
	r, pool, next := setup(t)
	p := mustInsert(t, r, next, ws, "封面")

	if _, d := displayCover(t, r, p); d != 0 {
		t.Fatalf("没有任何产出时应为 0（显示占位），得到 %d", d)
	}

	// 失败的图片、**还没有封面帧的**成功视频、成功的文本 —— 都不该成为封面：
	// 视频不能直接当 <img>，要等 Worker 截出封面帧（CD-83，见下一条用例）；文本/音频没有画面。
	seedGeneration(t, pool, next, p, "failed", "image", 0, 50*time.Second)
	seedGeneration(t, pool, next, p, "success", "video", seedMedia(t, pool, next, ws, "active"), 40*time.Second)
	seedGeneration(t, pool, next, p, "success", "text", 0, 30*time.Second)
	if _, d := displayCover(t, r, p); d != 0 {
		t.Fatalf("没有成功的图片时应为 0，得到 %d —— 视频/文本/失败的生成被当成了封面", d)
	}

	img := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, p, "success", "image", img, 20*time.Second)
	if _, d := displayCover(t, r, p); d != img {
		t.Fatalf("有成功的图片后应取它，期望 %d 得到 %d", img, d)
	}
}

func TestDisplayCover_取最近一张_删掉后自动换下一张(t *testing.T) {
	r, pool, next := setup(t)
	p := mustInsert(t, r, next, ws, "封面")

	older := seedMedia(t, pool, next, ws, "active")
	newer := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, p, "success", "image", older, 60*time.Second)
	seedGeneration(t, pool, next, p, "success", "image", newer, 10*time.Second)
	if _, d := displayCover(t, r, p); d != newer {
		t.Fatalf("应取最近一次生成的图（封面反映项目的当前状态），期望 %d 得到 %d", newer, d)
	}

	// ⚠️ 这是「算出来」相对「写进去」的关键收益：用户在素材库里删掉那张图之后，
	//    封面**自动换成下一张**，而不是留下一个取不到的 ID（列表上就是一张裂图）。
	if _, err := pool.Exec(context.Background(),
		`update media_object set status = 'deleted', deleted_at = now() where id = $1`, newer); err != nil {
		t.Fatal(err)
	}
	if _, d := displayCover(t, r, p); d != older {
		t.Fatalf("最新那张被删掉后应退到上一张，期望 %d 得到 %d", older, d)
	}
}

func TestDisplayCover_显式封面优先_且不改写显式封面的含义(t *testing.T) {
	r, pool, next := setup(t)
	p := mustInsert(t, r, next, ws, "封面")
	auto := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, p, "success", "image", auto, 10*time.Second)

	// ⚠️ 没手动设过封面时，`CoverMedia` 必须仍然是 0 —— 哪怕展示封面有值。
	//    画布域会把 CoverMedia 读回去写：改名时拿它登记媒体引用、复制项目时原样带给副本。
	//    若这里混进了自动挑的那张，改一次名就给一张用户没选过的图登记引用，
	//    复制出来的副本会拿源项目的产出图当显式封面（从此不跟着自己的产出变）。
	explicit, display := displayCover(t, r, p)
	if explicit != 0 {
		t.Fatalf("没手动设封面时 CoverMedia 应为 0，得到 %d —— 展示封面漏进了显式封面", explicit)
	}
	if display != auto {
		t.Fatalf("展示封面应为自动挑选的那张，期望 %d 得到 %d", auto, display)
	}

	chosen := seedMedia(t, pool, next, ws, "active")
	if _, err := pool.Exec(context.Background(),
		`update project set cover_media_id = $1 where id = $2`, chosen, p); err != nil {
		t.Fatal(err)
	}
	if e, d := displayCover(t, r, p); e != chosen || d != chosen {
		t.Fatalf("用户手动设了封面就用它，期望 (%d,%d) 得到 (%d,%d)", chosen, chosen, e, d)
	}

	// 显式封面被删掉 → 退回自动挑选，而不是给出一个取不到的 ID。
	if _, err := pool.Exec(context.Background(),
		`update media_object set status = 'deleted', deleted_at = now() where id = $1`, chosen); err != nil {
		t.Fatal(err)
	}
	if _, d := displayCover(t, r, p); d != auto {
		t.Fatalf("显式封面被删后应退回自动挑选，期望 %d 得到 %d", auto, d)
	}
}

// 列表（全部项目页）与单查走的是同一个 projectCols，这里钉住列表那条路也带上了它 ——
// 只验 Get 的话，哪天有人给 Page 单独写了列清单，列表上的封面就悄悄没了。
func TestDisplayCover_列表也带展示封面(t *testing.T) {
	r, pool, next := setup(t)
	p := mustInsert(t, r, next, ws, "封面")
	img := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, p, "success", "image", img, 10*time.Second)

	rows, err := r.Page(context.Background(), repo.PageQuery{WorkspaceID: ws, Limit: 10, NoFolder: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].DisplayCover != img {
		t.Fatalf("全部项目列表应带展示封面 %d，得到 %+v", img, rows)
	}
}

// ── 副本的封面（迁移 00031 的继承封面）──────────────────────────────
//
// ⚠️ 起因：00030 之后副本**永远是占位图** —— 展示封面只看项目自己的生成记录，
// 而副本按设计一条都不拥有（CV-03 / GAP-16）。这组用例走的是 app 层 Duplicate，
// 因为缺陷就出在「复制那一步没把封面带过去」。

func TestDuplicate_副本继承源项目的展示封面(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)

	src := mustInsert(t, r, next, ws, "有封面的项目")
	img := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, src, "success", "image", img, 10*time.Second)

	cp, err := svc.Duplicate(ctx, ws, 1, src, collab.RoleOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	explicit, display := displayCover(t, r, cp.ID)
	if display != img {
		t.Fatalf("副本应显示与源项目相同的封面 %d，得到 %d —— 这正是「复制后封面没了」", img, display)
	}
	// ⚠️ 继承的**不是**显式封面：写进 cover_media_id 的话，副本被钉死在这张图上、
	//    删图保护还会把它当引用（用户删不掉一张他从没设成封面的图）。
	if explicit != 0 {
		t.Fatalf("副本的显式封面应为 0（继承 ≠ 用户选择），得到 %d", explicit)
	}
	// 继承封面不能靠「复制生成记录」来实现 —— 那等于凭空多一份消费记录（CV-03）。
	var gens int
	if err := pool.QueryRow(ctx, `select count(*) from generation`).Scan(&gens); err != nil {
		t.Fatal(err)
	}
	if gens != 1 {
		t.Fatalf("复制不该产生生成记录，应仍只有源项目那 1 条，得到 %d", gens)
	}

	// 副本的副本同样对：继承的是源项目「此刻显示的」封面，而副本此刻显示的就是继承来的那张。
	cp2, err := svc.Duplicate(ctx, ws, 1, cp.ID, collab.RoleOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, d := displayCover(t, r, cp2.ID); d != img {
		t.Fatalf("副本的副本应仍显示 %d，得到 %d", img, d)
	}
}

func TestDuplicate_继承封面的优先级(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)

	src := mustInsert(t, r, next, ws, "源")
	inherited := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, src, "success", "image", inherited, 60*time.Second)
	cp, err := svc.Duplicate(ctx, ws, 1, src, collab.RoleOwner, "")
	if err != nil {
		t.Fatal(err)
	}

	// 副本自己出了图 → 换成自己的（不被钉死在源项目的图上）
	own := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, cp.ID, "success", "image", own, 5*time.Second)
	if _, d := displayCover(t, r, cp.ID); d != own {
		t.Fatalf("副本自己出了图后应换成自己的 %d，得到 %d —— 被钉死在了继承的图上", own, d)
	}

	// 自己那张被删 → 退回继承的
	mustDeleteMedia(t, pool, own)
	if _, d := displayCover(t, r, cp.ID); d != inherited {
		t.Fatalf("自己的图删掉后应退回继承的 %d，得到 %d", inherited, d)
	}

	// 用户手动设的封面压过一切
	chosen := seedMedia(t, pool, next, ws, "active")
	if _, err := pool.Exec(ctx, `update project set cover_media_id = $1 where id = $2`, chosen, cp.ID); err != nil {
		t.Fatal(err)
	}
	if _, d := displayCover(t, r, cp.ID); d != chosen {
		t.Fatalf("显式封面应优先，期望 %d 得到 %d", chosen, d)
	}
	if _, err := pool.Exec(ctx, `update project set cover_media_id = null where id = $1`, cp.ID); err != nil {
		t.Fatal(err)
	}

	// 继承的那张也被删 → 占位，而不是给一个取不到的 ID（列表上就是一张裂图）
	mustDeleteMedia(t, pool, inherited)
	if _, d := displayCover(t, r, cp.ID); d != 0 {
		t.Fatalf("所有候选都没了应为 0（占位），得到 %d", d)
	}
}

func mustDeleteMedia(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`update media_object set status = 'deleted', deleted_at = now() where id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

// ── 空画布套用模板（空画布上的四张快捷生成卡片）────────────────────────

func seedTemplate(t *testing.T, pool *pgxpool.Pool, next func() int64, code, canvas string, enabled bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into project_template (id, code, name, description, canvas_data, sort, is_enabled)
		values ($1, $2, $2, '', $3::jsonb, 1, $4)`, next(), code, canvas, enabled); err != nil {
		t.Fatal(err)
	}
}

func TestApplyTemplate_只允许空画布且走乐观锁(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	svc := canvasapp.New(r, next)
	const tpl = `{"schemaVersion":1,"nodes":[
		{"id":"n_first_frame","type":"image","title":"首帧图片","position":{"x":80,"y":120},"params":{}},
		{"id":"n_video","type":"video","title":"视频","position":{"x":500,"y":120},"params":{"mode":"img2video"}}],
		"edges":[{"id":"e1","source":"n_first_frame","target":"n_video","order":1,"operation":"img2video"}]}`
	seedTemplate(t, pool, next, "first_frame_video", tpl, true)
	seedTemplate(t, pool, next, "retired", tpl, false)

	id := mustInsert(t, r, next, ws, "空项目")
	cur, _ := r.Get(ctx, ws, id)

	// ① 空画布 → 套上，版本进位
	got, err := svc.ApplyTemplate(ctx, ws, id, 1, collab.RoleOwner, "first_frame_video", cur.Version)
	if err != nil {
		t.Fatal(err)
	}
	var tree struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []map[string]any `json:"edges"`
	}
	_ = json.Unmarshal(got.CanvasData, &tree)
	if len(tree.Nodes) != 2 || len(tree.Edges) != 1 {
		t.Fatalf("应套上 2 个节点 1 条连线，得到 %d/%d", len(tree.Nodes), len(tree.Edges))
	}
	if got.Version != cur.Version+1 {
		t.Fatalf("套用模板就是一次保存，版本应 +1：%d → %d", cur.Version, got.Version)
	}

	// ② 已经不空 → 40905，画布不被覆盖
	_, err = svc.ApplyTemplate(ctx, ws, id, 1, collab.RoleOwner, "first_frame_video", got.Version)
	if err == nil || !strings.Contains(err.Error(), "仅空画布") {
		t.Fatalf("画了东西的画布不能再套模板，应报「仅空画布」，得到 %v", err)
	}

	// ③ 版本过期 → 冲突，而不是覆盖（「检查为空」与「写入」之间被别人改过）
	id2 := mustInsert(t, r, next, ws, "另一个空项目")
	cur2, _ := r.Get(ctx, ws, id2)
	if _, err := svc.ApplyTemplate(ctx, ws, id2, 1, collab.RoleOwner, "first_frame_video", cur2.Version+5); err == nil {
		t.Fatal("版本对不上时应报冲突，而不是照写")
	}

	// ④ 停用的模板取不到
	if _, err := svc.ApplyTemplate(ctx, ws, id2, 1, collab.RoleOwner, "retired", cur2.Version); err == nil {
		t.Fatal("停用的模板不该还能套用")
	}

	// ⑤ 只读成员不能套（套模板就是改画布）
	if _, err := svc.ApplyTemplate(ctx, ws, id2, 1, collab.RoleViewer, "first_frame_video", cur2.Version); err == nil {
		t.Fatal("只读成员不该能套用模板")
	}
}

// seedVideo 造一段视频媒体（purpose 与生成产出一致）。
func seedVideo(t *testing.T, pool *pgxpool.Pool, next func() int64, purpose string) int64 {
	t.Helper()
	mid := next()
	if _, err := pool.Exec(context.Background(), `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
		                          purpose, created_by_user_id, status)
		values ($1, $2, 'seeaitv', $3, 'video/mp4', 1, $4, 1, 'active')`,
		mid, ws, fmt.Sprintf("test/%d.mp4", mid), purpose); err != nil {
		t.Fatal(err)
	}
	return mid
}

// seedPoster 给视频造一张封面帧（Worker 的 PosterScanner 截出来的就是这样一行）。
func seedPoster(t *testing.T, pool *pgxpool.Pool, next func() int64, video int64) int64 {
	t.Helper()
	pid := next()
	if _, err := pool.Exec(context.Background(), `
		insert into media_object (id, workspace_id, bucket, object_key, content_type, size_bytes,
		                          purpose, created_by_user_id, status,
		                          source_media_id, derive_kind, derive_index)
		values ($1, $2, 'seeaitv', $3, 'image/jpeg', 1, 'generation_output', 1, 'active',
		        $4, 'video_poster', 1)`,
		pid, ws, fmt.Sprintf("test/%d.jpg", pid), video); err != nil {
		t.Fatal(err)
	}
	return pid
}

// CD-83：视频与成片有了封面帧就算进封面候选，按「最近一次产出」取。
//
// ⚠️ 起因：Agent 项目只出视频，卡片一直是占位图 —— 封面此前只认图片。
func TestDisplayCover_视频与成片用封面帧(t *testing.T) {
	r, pool, next := setup(t)
	ctx := context.Background()
	p := mustInsert(t, r, next, ws, "Agent 出的片子")

	img := seedMedia(t, pool, next, ws, "active")
	seedGeneration(t, pool, next, p, "success", "image", img, 90*time.Second)
	video := seedVideo(t, pool, next, "generation_output")
	seedGeneration(t, pool, next, p, "success", "video", video, 60*time.Second)

	// 封面帧还没截出来（Worker 一分钟扫一次）：先用那张图，不出裂图
	if _, d := displayCover(t, r, p); d != img {
		t.Fatalf("视频还没有封面帧时应退回图片，期望 %d 得到 %d", img, d)
	}
	poster := seedPoster(t, pool, next, video)
	if _, d := displayCover(t, r, p); d != poster {
		t.Fatalf("视频更近、且有了封面帧，应取它的封面帧，期望 %d 得到 %d", poster, d)
	}

	// 成片（时间线渲染）更近：用成片的封面帧
	film := seedVideo(t, pool, next, "render_output")
	tl := next()
	if _, err := pool.Exec(ctx, `insert into timeline (id, workspace_id, project_id, name)
		values ($1, $2, $3, '成片')`, tl, ws, p); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into render_job (id, workspace_id, timeline_id, timeline_version,
		snapshot, payer_account_id, created_by_user_id, status, output_media_id, created_at)
		values ($1, $2, $3, 1, '{}'::jsonb, 1, 1, 'success', $4, now() - interval '10 seconds')`,
		next(), ws, tl, film); err != nil {
		t.Fatal(err)
	}
	filmPoster := seedPoster(t, pool, next, film)
	if _, d := displayCover(t, r, p); d != filmPoster {
		t.Fatalf("成片最近，应取成片的封面帧，期望 %d 得到 %d", filmPoster, d)
	}

	// 用户删掉了那段成片：不再拿它的帧当封面，退回上一次产出
	if _, err := pool.Exec(ctx, `update media_object set status = 'deleted', deleted_at = now()
		where id = $1`, film); err != nil {
		t.Fatal(err)
	}
	if _, d := displayCover(t, r, p); d != poster {
		t.Fatalf("成片被删后应退到视频的封面帧，期望 %d 得到 %d", poster, d)
	}
}
