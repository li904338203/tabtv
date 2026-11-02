//go:build integration

// 真库集成测试（AC-25、INIT-01）。在共享的内嵌 PG 集群里建一个空库（TT-T-04），
// 不依赖 docker，CI 里也能跑。
//
// 04 §5 指出旧系统"未见 Testcontainers 集成链路"，Service 测试大多 mock mapper，
// 因此关键的 schema 事实必须由真库证明，而不是靠读 SQL 文件推断。
package integration

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/platform/bootstrapadmin"
	"github.com/li904338203/tabtv/backend/internal/platform/id"
	"github.com/li904338203/tabtv/backend/internal/platform/migrate"
	"github.com/li904338203/tabtv/backend/internal/platform/pgembed/testcluster"
	"github.com/li904338203/tabtv/backend/internal/platform/seed"
	"github.com/li904338203/tabtv/backend/migrations"
)

func startPG(t *testing.T) string {
	t.Helper()
	dsn, err := testcluster.EmptyDatabase(context.Background())
	if err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	return dsn
}

func count(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("查询失败 %q: %v", q, err)
	}
	return n
}

// 空库从零跑通全部迁移，并验证 down 能干净回滚。
func TestIntegration_空库迁移与回滚(t *testing.T) {
	dsn := startPG(t)
	r, err := migrate.Open(dsn, migrations.FS, migrations.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	if err := r.Up(ctx); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 表数是**故意写死的绊线**：改动它应当是一次有意识的决定，
	// 而不是某次迁移顺手删表后没人发现。新增建表的迁移时同步改这里。
	if got := count(t, r.DB(),
		`select count(*) from information_schema.tables
		 where table_schema='public' and table_name<>'schema_migration'`); got != 119 {
		t.Errorf("期望 119 张业务表，实际 %d", got)
	}

	// 版本号不写死：它随每次迁移递增，写死等于每加一条迁移就改一次测试，
	// 而这条断言真正要证明的是「全部迁移都应用了」。
	want := latestMigrationVersion(t)
	v, _ := r.Version(ctx)
	if v != want {
		t.Errorf("期望迁移到最新版本 %d，实际 %d", want, v)
	}

	// ⚠️ 不能断言"回滚后表数减少"：不是每条迁移都建表
	// （00015 只改列默认值），那样写会让下一条非建表迁移无辜挂掉。
	// down 的真实契约是**可逆**：回滚一步版本号退一步，再 up 能回到最新。
	if err := r.Down(ctx); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if v, _ := r.Version(ctx); v != want-1 {
		t.Errorf("回滚一步后应为版本 %d，实际 %d", want-1, v)
	}
	if err := r.Up(ctx); err != nil {
		t.Fatalf("回滚后重新迁移失败（down 与 up 不一致）: %v", err)
	}
	if v, _ := r.Version(ctx); v != want {
		t.Errorf("重新迁移后应回到版本 %d，实际 %d", want, v)
	}
}

// latestMigrationVersion 取迁移目录里最大的版本号。
func latestMigrationVersion(t *testing.T) int64 {
	t.Helper()
	entries, err := migrations.FS.ReadDir(migrations.Dir)
	if err != nil {
		t.Fatalf("读取迁移目录失败: %v", err)
	}
	var max int64
	for _, e := range entries {
		name := e.Name()
		i := strings.IndexByte(name, '_')
		if i <= 0 {
			continue
		}
		n, err := strconv.ParseInt(name[:i], 10, 64)
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	if max == 0 {
		t.Fatal("迁移目录里没有可识别的版本号")
	}
	return max
}

// seed 与 bootstrap 必须幂等 —— INIT-01 要求可**重复**验证。
func TestIntegration_seed与bootstrap幂等(t *testing.T) {
	dsn := startPG(t)
	r, err := migrate.Open(dsn, migrations.FS, migrations.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if err := r.Up(ctx); err != nil {
		t.Fatal(err)
	}

	gen, _ := id.New(0)
	run := func(round int) (perms, roles, admins, bindings int) {
		if err := seed.Apply(ctx, r.DB(), gen.Next); err != nil {
			t.Fatalf("第 %d 轮 seed 失败: %v", round, err)
		}
		if _, err := bootstrapadmin.Apply(ctx, r.DB(), gen.Next, "admin", "pw-for-test"); err != nil {
			t.Fatalf("第 %d 轮 bootstrap 失败: %v", round, err)
		}
		return count(t, r.DB(), `select count(*) from permission`),
			count(t, r.DB(), `select count(*) from role`),
			count(t, r.DB(), `select count(*) from admin_user`),
			count(t, r.DB(), `select count(*) from role_permission`)
	}

	p1, r1, a1, b1 := run(1)
	p2, r2, a2, b2 := run(2)

	if p1 != p2 || r1 != r2 || a1 != a2 || b1 != b2 {
		t.Errorf("重复执行后计数变化，seed/bootstrap 不幂等：\n  权限 %d→%d 角色 %d→%d 管理员 %d→%d 绑定 %d→%d",
			p1, p2, r1, r2, a1, a2, b1, b2)
	}
	if a1 != 1 {
		t.Errorf("期望恰好 1 个管理员，实际 %d", a1)
	}

	// seed 只含配置，**不得**含任何用户业务数据（10 §1）。
	for _, tbl := range []string{"app_user", "workspace", "credit_grant",
		"credit_transaction", "purchase_order", "project", "generation", "media_object"} {
		if got := count(t, r.DB(), `select count(*) from "`+tbl+`"`); got != 0 {
			t.Errorf("seed 后 %s 应为空，实际 %d 行", tbl, got)
		}
	}
}

// 账本的三条硬约束必须由**数据库**保证，而不是只靠应用层小心。
func TestIntegration_账本约束由数据库强制(t *testing.T) {
	dsn := startPG(t)
	r, err := migrate.Open(dsn, migrations.FS, migrations.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	db := r.DB()

	// ① 同一 (ref_type, ref_id, type) 只能有一条流水 —— 退款一次性的最终兜底（GO-21）。
	ins := `insert into credit_transaction (id, payer_account_id, type, change, balance_after, ref_type, ref_id)
	        values ($1, 1, 'refund', 10, 10, 'generation', 'g-1')`
	if _, err := db.Exec(ins, 1001); err != nil {
		t.Fatalf("首次插入应成功: %v", err)
	}
	if _, err := db.Exec(ins, 1002); err == nil {
		t.Error("重复退款流水应被 uk_ct_ref_type 拒绝")
	}

	// ② remaining 不得超过 amount，也不得为负。
	badGrant := `insert into credit_grant (id, payer_account_id, source, amount, remaining)
	             values ($1, 1, 'purchase', 100, $2)`
	if _, err := db.Exec(badGrant, 2001, 200); err == nil {
		t.Error("remaining > amount 应被 CHECK 拒绝")
	}
	if _, err := db.Exec(badGrant, 2002, -1); err == nil {
		t.Error("remaining < 0 应被 CHECK 拒绝")
	}

	// ③ generation 状态只允许五态；reviewing 已移出（CD-06）。
	badGen := `insert into generation
	   (id, workspace_id, payer_account_id, created_by_user_id, project_id,
	    operation_id, model_id, result_type, idempotency_key, status)
	   values ($1, 1, 1, 1, 1, 1, 1, 'image', $2, $3)`
	if _, err := db.Exec(badGen, 3001, "k1", "reviewing"); err == nil {
		t.Error("reviewing 不应再是合法的 generation 状态")
	}
	if _, err := db.Exec(badGen, 3002, "k2", "cancelled"); err != nil {
		t.Errorf("cancelled 应是合法终态: %v", err)
	}

	// ④ 幂等键在同一付费账户下唯一（AC-04）。
	if _, err := db.Exec(badGen, 3003, "k2", "queued"); err == nil {
		t.Error("同一 payer_account 下重复幂等键应被拒绝")
	}
}

// 00057：历史成功的媒体生成补登为待审作品。
//
// 口径与 Worker 的 generation/repo.RegisterWork 一致：按 idx 第一个媒体产出、
// 标题取项目名、文本/非成功不登、已有作品跳过；Down 只删本迁移补登且没人审过的。
func TestIntegration_00057历史生成补登作品(t *testing.T) {
	dsn := startPG(t)
	r, err := migrate.Open(dsn, migrations.FS, migrations.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if err := r.UpTo(ctx, 56); err != nil {
		t.Fatalf("迁移到 56 失败: %v", err)
	}
	db := r.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("造数据失败 %q: %v", q, err)
		}
	}
	exec(`insert into project (id, workspace_id, created_by_user_id, name)
		values (10, 1, 1, '  夏日汽水  ')`)
	gen := func(id int64, status, resultType string, projectID int64) {
		exec(`insert into generation (id, workspace_id, payer_account_id, created_by_user_id,
			project_id, operation_id, model_id, result_type, idempotency_key, status, finished_at)
			values ($1, 1, 1, 1, $2, 1, 1, $3, $4, $5, now())`,
			id, projectID, resultType,
			"k"+strconv.FormatInt(id, 10), status)
	}
	out := func(id, genID int64, idx int, typ string, media int64) {
		if typ == "text" {
			exec(`insert into generation_output (id, generation_id, idx, output_type, text_content)
				values ($1,$2,$3,'text','字')`, id, genID, idx)
			return
		}
		exec(`insert into generation_output (id, generation_id, idx, output_type, media_object_id)
			values ($1,$2,$3,$4,$5)`, id, genID, idx, typ, media)
	}
	gen(101, "success", "image", 10) // 文字在前、图在后：媒体应取 idx 1
	out(201, 101, 0, "text", 0)
	out(202, 101, 1, "image", 3002)
	out(203, 101, 2, "image", 3003)
	gen(102, "success", "text", 10) // 纯文本：不登
	out(204, 102, 0, "text", 0)
	gen(103, "failed", "video", 10)  // 失败：不登
	gen(104, "success", "video", 10) // 用户早已手动提交过：跳过，保持原样
	out(205, 104, 0, "video", 3005)
	exec(`insert into work (id, workspace_id, source_type, source_id, media_object_id, title,
		review_status, submitted_at) values (900, 1, 'generation', 104, 3005, '我的片子', 'rejected', now())`)
	gen(105, "success", "audio", 99) // 项目不存在：标题未命名
	out(206, 105, 0, "audio", 3006)
	gen(106, "cancelled", "image", 10)

	if err := r.UpTo(ctx, 57); err != nil {
		t.Fatalf("00057 失败: %v", err)
	}
	if n := count(t, db, `select count(*) from work where id = source_id`); n != 2 {
		t.Fatalf("应补登 2 件（101、105），实际 %d", n)
	}
	if n := count(t, db, `select count(*) from work where source_id=101 and media_object_id=3002
		and title='夏日汽水' and review_status='pending' and submitted_at is null
		and not is_featured and not is_offline`); n != 1 {
		t.Fatal("101 应以第一个媒体产出、项目名为标题补登为 pending")
	}
	if n := count(t, db, `select count(*) from work where source_id=105 and title='未命名作品'`); n != 1 {
		t.Fatal("项目取不到时标题应为「未命名作品」")
	}
	if n := count(t, db, `select count(*) from work where source_id in (102,103,106)`); n != 0 {
		t.Fatal("文本 / 失败 / 取消的不该补登")
	}
	if n := count(t, db, `select count(*) from work where source_id=104 and id=900
		and review_status='rejected' and title='我的片子'`); n != 1 {
		t.Fatal("已有作品必须原样保留")
	}

	// 运营审过 105（留了痕）：Down 不得删它；101 没人审过，Down 删掉。
	exec(`update work set review_status='approved', reviewed_by_admin_id=7 where source_id=105`)
	exec(`insert into content_review (id, work_id, action, operator_admin_id)
		values (1, 105, 'approved', 7)`)
	if err := r.Down(ctx); err != nil {
		t.Fatalf("00057 回滚失败: %v", err)
	}
	if n := count(t, db, `select count(*) from work`); n != 2 {
		t.Fatalf("回滚后应剩用户提交的 104 与已审的 105，实际 %d 件", n)
	}
	if n := count(t, db, `select count(*) from work where source_id in (104,105)`); n != 2 {
		t.Fatal("回滚误删了用户提交或人工审过的作品")
	}
	// 再 up：补回 101，已审的 105 不受影响（冲突跳过）。
	if err := r.UpTo(ctx, 57); err != nil {
		t.Fatalf("00057 重跑失败: %v", err)
	}
	if n := count(t, db, `select count(*) from work`); n != 3 {
		t.Fatalf("重跑后应为 3 件，实际 %d", n)
	}
	if n := count(t, db, `select count(*) from work where source_id=105 and review_status='approved'`); n != 1 {
		t.Fatal("重跑不得重置已审作品")
	}
	if err := r.Up(ctx); err != nil {
		t.Fatalf("迁移到最新失败: %v", err)
	}
}

// 00067 智能剪辑（docs/agent/smartcut SCT-02）：存量字幕回填到默认字幕轨、导出兜底价改 0、
// 新约束真的由数据库强制。
//
// ⚠️ 字幕回填是这条迁移里唯一改存量数据的地方：回填错一条，那条字幕就会出现在别的轨上
// （或者整条迁移因为 NOT NULL 失败）。所以造两种存量：有默认字幕轨的、没有字幕轨的。
func TestIntegration_00067字幕回填与约束(t *testing.T) {
	dsn := startPG(t)
	r, err := migrate.Open(dsn, migrations.FS, migrations.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if err := r.UpTo(ctx, 66); err != nil {
		t.Fatalf("迁移到 66 失败: %v", err)
	}
	db := r.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("执行失败 %q: %v", q, err)
		}
	}
	// 时间线 1：三条默认轨 + 3 条字幕；时间线 2：只有画面轨（历史脏数据）+ 1 条字幕。
	exec(`insert into timeline (id, workspace_id, project_id, name) values (1, 1, 10, 'a'), (2, 1, 10, 'b')`)
	exec(`insert into timeline_track (id, timeline_id, kind, idx, name) values
		(11, 1, 'video', 1, '画面'), (12, 1, 'audio', 1, '音频'), (13, 1, 'subtitle', 1, '字幕'),
		(21, 2, 'video', 1, '画面')`)
	exec(`insert into timeline_subtitle (id, timeline_id, start_ms, end_ms, text) values
		(101, 1, 0, 1000, '一'), (102, 1, 1000, 2000, '二'), (103, 1, 2000, 3000, '三'),
		(201, 2, 0, 500, '孤')`)
	// 兜底价：timeline_render 改 0，别的不动。
	exec(`insert into operation (id, code, name, domain, out_node_type) values
		(501, 'timeline_render', '时间线渲染', 'video', 'video'), (502, 'text2img', '文生图', 'image', 'image')`)
	exec(`insert into pricing (id, operation_id, model_id, credits, unit) values
		(601, 501, null, 1, 'per_second'), (602, 502, null, 5, 'per_image'), (603, 501, 77, 3, 'per_second')`)

	if err := r.UpTo(ctx, 67); err != nil {
		t.Fatalf("00067 失败: %v", err)
	}
	if n := count(t, db, `select count(*) from timeline_subtitle where timeline_id = 1 and track_id = 13`); n != 3 {
		t.Fatalf("时间线 1 的 3 条字幕都应回填到默认字幕轨 13，实际 %d 条", n)
	}
	if n := count(t, db, `select count(*) from timeline_subtitle s join timeline_track k on k.id = s.track_id
		where s.id = 201 and k.timeline_id = 2 and k.kind = 'subtitle'`); n != 1 {
		t.Fatal("没有字幕轨的时间线应先补一条字幕轨，再把字幕回填上去")
	}
	if n := count(t, db, `select count(*) from timeline_subtitle where id = 102
		and start_ms = 1000 and end_ms = 2000 and text = '二'`); n != 1 {
		t.Fatal("回填不能改动字幕的时间与内容（SC-F-55）")
	}
	if n := count(t, db, `select count(*) from timeline where is_arranged`); n != 2 {
		t.Fatal("存量时间线应默认「剪过」：它们的内容是有意排出来的，不能被连线即入轨改动")
	}
	if n := count(t, db, `select credits from pricing where id = 601`); n != 0 {
		t.Fatalf("timeline_render 兜底价应改为 0（SCK-3），实际 %d", n)
	}
	if n := count(t, db, `select credits from pricing where id = 602`) +
		count(t, db, `select credits from pricing where id = 603`); n != 8 {
		t.Fatalf("其它兜底价与模型价不应被改动，实际合计 %d", n)
	}

	// 约束由数据库强制。
	mustFail := func(what, q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err == nil {
			t.Errorf("%s：应被数据库拒绝，实际成功", what)
		}
	}
	exec(`insert into media_analysis (id, workspace_id, media_object_id, kind, engine)
		values (701, 1, 9001, 'preview_proxy', 'v1')`)
	mustFail("同一（媒体，种类，引擎，参数）的排队行重复",
		`insert into media_analysis (id, workspace_id, media_object_id, kind, engine)
		 values (702, 1, 9001, 'preview_proxy', 'v1')`)
	exec(`update media_analysis set status = 'failed' where id = 701`)
	exec(`insert into media_analysis (id, workspace_id, media_object_id, kind, engine)
		values (703, 1, 9001, 'preview_proxy', 'v1')`) // 失败的行不占位，重试另插一行
	mustFail("未知的分析种类", `insert into media_analysis (id, workspace_id, media_object_id, kind, engine)
		values (704, 1, 9002, 'thumbnail', 'v1')`)

	exec(`insert into render_job (id, workspace_id, timeline_id, timeline_version, snapshot,
		payer_account_id, created_by_user_id, idempotency_key) values (801, 1, 1, 1, '{}', 1, 1, 'k-1')`)
	mustFail("同一工作区同一把导出幂等键", `insert into render_job (id, workspace_id, timeline_id, timeline_version,
		snapshot, payer_account_id, created_by_user_id, idempotency_key) values (802, 1, 1, 1, '{}', 1, 1, 'k-1')`)
	exec(`update render_job set stage = 'mixing' where id = 801`)
	exec(`update render_job set stage = 'checking' where id = 801`)

	mustFail("非法的快照原因", `insert into timeline_snapshot (id, timeline_id, version, content, reason)
		values (901, 1, 1, '{}', 'whatever')`)
	exec(`insert into timeline_snapshot (id, timeline_id, version, content, reason)
		values (902, 1, 1, '{}', 'replace_changed')`)
	mustFail("同一项目同一节点两条编辑中的时间线",
		`update timeline set canvas_node_id = 'n1' where id in (1, 2)`)
	exec(`insert into timeline_track (id, timeline_id, kind, idx, name, role) values (14, 1, 'text', 1, '文字', null),
		(15, 1, 'audio', 2, '配乐', 'music')`)
	mustFail("未知的音频轨用途", `update timeline_track set role = 'bgm' where id = 15`)
	mustFail("字幕必须归属字幕轨", `insert into timeline_subtitle (id, timeline_id, start_ms, end_ms, text)
		values (104, 1, 0, 100, 'x')`)
}
