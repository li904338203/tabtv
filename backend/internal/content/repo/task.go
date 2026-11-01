package repo

import (
	"context"
	"strings"
	"time"

	"github.com/li904338203/tabtv/backend/internal/content/domain"
)

// 任务中心：四类任务的统一历史（P13-02；GAP-05）。
//
// ⚠️ **合并在 SQL 里**，与消息中心同一条：两边各取一页再拼的话第二页会漏
// （被挤掉的那些再也不会出现在任何一页上，而列表"看起来很正常"）。
//
// ⚠️ **状态映射也下推到 SQL**（`case` 表达式），而不是取回来再用
// `domain.NormalizeState` 转一遍。理由是**筛选**：按统一状态筛选时必须在
// `where` 里生效，取回来再过滤在分页下是错的 —— 取 20 条再过滤剩 3 条，
// 用户翻一页只看到 3 条，而他以为那就是全部。
// SQL 与 domain 是同一份映射，集成测试用同一批数据同时验两边。

// Task 是任务中心的一行。
//
// ⚠️ 这是一个**最小公共视图**，不是四张表的并集：各类任务自己的细节
// （generation 的 outputs、workflow_run 的节点、render_job 的快照）
// 走各自的详情端点。塞进列表的话这个结构会跟着四个域一起变，
// 而它每变一次四个域都得跟着改。
type Task struct {
	ID        int64
	Kind      domain.TaskKind
	State     domain.TaskState
	Title     string
	Credits   int
	Error     string
	CreatedAt time.Time
	// RefID 是回到各域详情页所需的标识（多数时候就是 ID 本身）。
	RefID int64
}

// tenantScope 是 union 里每一段的租户条件，**按表别名生成**。
//
// ⚠️ 它接受别名而不是直接给一段 SQL：四段各有各的别名（g/w/r/e），
// 把别名写死在模板里（`where g.占位符`）的话，"不按租户过滤"那个取值
// 会被拼成 `where g.true` —— PG 报 `column g.true does not exist`，
// 而那条错误指向一个根本不存在的列名（实测第一次跑就红在这里）。
//
// ⚠️ 做成**具名类型 + 两个构造函数**而不是随手传字符串：这一个条件是整条
// 跨租户防线的落点，必须让"哪一处没带它"在代码里一眼看得出来 ——
// 而 `allTenants()` 这个名字在审查时是刺眼的。
type tenantScope func(alias string) string

// tenantScoped 是**用户侧**的条件：每一段都按当前工作区过滤。
func tenantScoped() tenantScope {
	return func(alias string) string { return alias + ".workspace_id = $1" }
}

// allTenants 是**后台运维台**的条件：不按工作区过滤，看全站。
//
// ⚠️ 用它的路由必须挂 `ops:read`。后台本来就没有工作区上下文
// （运维手上只有一个任务 ID），归属由权限位把关。
func allTenants() tenantScope {
	return func(string) string { return "true" }
}

// taskUnionSQL 是四类任务的统一投影。
//
// ⚠️ **一份定义两处用**（用户任务中心 + 后台运维台）。各写一份的话，
// 新增一个状态词时只改一处 —— 而漏掉的那一处的症状是**某一类任务在筛选下
// 整体消失**，没有任何报错（CD-62 记过一次，这里是它的第二个消费者）。
// 列多出来对用户侧是免费的：那边的 select 只挑自己要的几列。
//
// ⚠️ 每一段都必须带 `scope`：少一处就是一个跨租户泄漏，
// 而它不会报错 —— 用户只会在自己的任务列表里看到别人的任务。
//
// ⚠️ `case` 里的映射与 `domain.NormalizeState` 必须逐字一致。
// 少写一个 `when 'pending'` 的话，整图运行的排队任务会全部落进 else 分支
// （failed），而用户看到的是一排"失败"的运行。
func taskUnionSQL(scope tenantScope) string {
	out := taskUnionTemplate
	// 四段各自的别名。⚠️ 加一段 union 时这里**也要加别名** ——
	// 漏了的话那一段的占位符原样留在 SQL 里，PG 会在运行时才报语法错误。
	for _, alias := range []string{"g", "w", "r", "e"} {
		out = strings.ReplaceAll(out, "{{scope:"+alias+"}}", scope(alias))
	}
	return out
}

const taskUnionTemplate = `
	select g.id, 'generation' as kind,
	       case g.status
	         when 'queued' then 'queued' when 'running' then 'running'
	         when 'success' then 'success' when 'cancelled' then 'cancelled'
	         else 'failed' end as state,
	       -- generation 存的是 operation_id 不是 code，要关联取名字；
	       -- 用 left join：操作被删掉之后那些历史任务仍要出现在列表里
	       -- （用户花过的钱不能因为一条配置被删就从他的历史里消失）。
	       -- ⚠️ SQL 字符串是 Go 的原始字符串，注释里**不能出现反引号**
	       --    （会提前终止字面量，而编译错误指向几十行之外）。
	       coalesce(op.name, '生成') as title,
	       coalesce(g.credits_cost, 0) as credits,
	       coalesce(g.error_message, '') as err,
	       g.created_at, g.id as ref_id,
	       -- 以下是**运维台**才用的列。用户侧的 select 不挑它们，等于不存在；
	       -- 而它们在这里，是为了让两处共用同一份 union 与同一份状态映射。
	       g.workspace_id, g.payer_account_id, g.created_by_user_id,
	       g.status as raw_status, g.attempt,
	       coalesce(pc.code, '') as provider_code, coalesce(m.code, '') as model_code,
	       coalesce(g.error_code, '') as err_code,
	       -- 上游原话：只给运维台看（用户侧 err 是归类后的说法，见 platform/usererr）。
	       coalesce(g.error_detail, '') as err_detail,
	       g.deadline_at, g.finished_at
	from generation g
	left join operation op on op.id = g.operation_id
	-- 模型/供应商被删之后历史任务仍要出现在运维台里：排查的往往正是
	-- "这个模型刚被下掉，它之前那批任务怎么样了"。
	left join model m on m.id = g.model_id
	left join provider_config pc on pc.id = m.provider_id
	where {{scope:g}}
	union all
	select w.id, 'workflow',
	       case w.status
	         -- ⚠️ 整图运行用的是 pending 不是 queued。
	         when 'pending' then 'queued' when 'running' then 'running'
	         when 'success' then 'success'
	         -- ⚠️ partial_success 不摊平（AV-24）。
	         when 'partial_success' then 'partial_success'
	         when 'cancelled' then 'cancelled'
	         else 'failed' end,
	       '整图运行', coalesce(w.total_credits, 0), '', w.created_at, w.id,
	       w.workspace_id, w.payer_account_id, w.created_by_user_id,
	       w.status, 0, '', '', '', '', null, w.finished_at
	from workflow_run w where {{scope:w}}
	union all
	select r.id, 'render',
	       case r.status
	         when 'queued' then 'queued' when 'running' then 'running'
	         when 'success' then 'success' when 'cancelled' then 'cancelled'
	         else 'failed' end,
	       '成片渲染', coalesce(r.credits_cost, 0),
	       coalesce(r.error_message, ''), r.created_at, r.id,
	       r.workspace_id, r.payer_account_id, r.created_by_user_id,
	       r.status, 0, '', '', coalesce(r.error_code, ''), '',
	       r.deadline_at, r.finished_at
	from render_job r where {{scope:r}}
	union all
	select e.id, 'export',
	       case e.status
	         when 'queued' then 'queued' when 'running' then 'running'
	         when 'success' then 'success'
	         else 'failed' end,
	       '打包导出', 0, coalesce(e.error_message, ''), e.created_at, e.id,
	       -- ⚠️ 导出表没有付费账户列（导出不扣费），给 0 而不是去关联一个 ——
	       -- 关联出来的那个数会让人以为这次导出花了钱。
	       e.workspace_id, 0, e.user_id,
	       e.status, 0, '', '', coalesce(e.error_code, ''), '', null, null
	from export_task e where {{scope:e}}
`

// TaskFilter 限定任务中心的查询。
type TaskFilter struct {
	Kind  domain.TaskKind
	State domain.TaskState
	Limit int
	// 游标是 (createdAt, id) 两段：四张表的 id 来自同一个雪花发号器，
	// 但顺序与时间不严格一致。
	CursorAt *time.Time
	CursorID int64
}

// ListTasks 列出统一任务历史。
func (r *Repo) ListTasks(ctx context.Context, workspaceID int64, f TaskFilter) ([]Task, error) {
	args := []any{workspaceID}
	cond := []string{}

	if f.Kind != "" {
		args = append(args, string(f.Kind))
		cond = append(cond, "kind = $"+itoa(len(args)))
	}
	if f.State != "" {
		// ⚠️ 筛选比的是**统一状态**（union 里已经算好的 `state` 列），
		// 不是各表的原始值 —— 后者要写四段不同的条件，而漏一段就是
		// "那一类任务在这个筛选下整体消失"。
		args = append(args, string(f.State))
		cond = append(cond, "state = $"+itoa(len(args)))
	}
	if f.CursorAt != nil {
		args = append(args, *f.CursorAt, f.CursorID)
		cond = append(cond, "(created_at < $"+itoa(len(args)-1)+
			" or (created_at = $"+itoa(len(args)-1)+" and id < $"+itoa(len(args))+"))")
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	args = append(args, f.Limit)

	rows, err := r.pool.Query(ctx, `
		with merged as (`+taskUnionSQL(tenantScoped())+`)
		select id, kind, state, title, credits, err, created_at, ref_id
		from merged`+where+`
		order by created_at desc, id desc limit $`+itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Kind, &t.State, &t.Title, &t.Credits,
			&t.Error, &t.CreatedAt, &t.RefID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskCounts 是各统一状态的条数，供任务中心的筛选器显示数字。
type TaskCounts map[domain.TaskState]int

// CountTasks 统计各状态条数。
//
// ⚠️ 与列表用**同一个 union**：分开写的话筛选器上的数字与列表内容对不上，
// 而两个数都"没错"（同消息中心的未读数与列表必须同条件）。
func (r *Repo) CountTasks(ctx context.Context, workspaceID int64,
	kind domain.TaskKind) (TaskCounts, error) {
	args := []any{workspaceID}
	where := ""
	if kind != "" {
		args = append(args, string(kind))
		where = " where kind = $2"
	}
	rows, err := r.pool.Query(ctx, `
		with merged as (`+taskUnionSQL(tenantScoped())+`)
		select state, count(*)::int from merged`+where+` group by state`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := TaskCounts{}
	for rows.Next() {
		var s domain.TaskState
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}
