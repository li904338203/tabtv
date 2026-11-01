package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 后台项目运维（P13-03；AD-08）。
//
// ⚠️ 这里**不带工作区条件** —— 后台没有工作区上下文（客服手上只有一个
// 项目 ID 或用户报的名字），归属由 `ops:read`/`ops:write` 权限位把关。
// 所有查询都在 `ops` 域里，用户侧的 `canvas/repo` 一个字都不改：
// 往那边加 `or 是管理员` 的话，整条越权防线就从"每条查询都带 workspace_id"
// 退化成"每条查询都要记得判断调用者是谁"，而漏掉的那一条不会报错。

// ProjectRow 是后台项目列表的一行。
type ProjectRow struct {
	ID            int64
	WorkspaceID   int64
	WorkspaceName string
	OwnerUserID   int64
	OwnerName     string
	Name          string
	Status        string
	Version       int
	NodeCount     int
	EdgeCount     int
	GenCount      int
	DeletedAt     *time.Time
	UpdatedAt     time.Time
	CreatedAt     time.Time
}

// projectCols 里 node/edge 的条数**从 canvasData 里当场算**。
//
// ⚠️ 不另存两列计数：那两列要在每次保存画布时同步更新，而漏掉任何一条
// 更新路径都不会报错 —— 症状是后台显示的节点数与用户看到的对不上，
// 且没人说得清哪个是对的。jsonb_array_length 便宜，而且永远是对的。
//
// ⚠️ 用 `coalesce(jsonb_array_length(...), 0)`：`canvas_data` 可能是 `{}`
// （刚建的项目）或字段缺失，那时函数回 null —— 不兜底的话扫描直接报错，
// 整个列表打不开。
const projectCols = `p.id, p.workspace_id, coalesce(w.name,''),
	p.created_by_user_id, coalesce(u.nickname, ''), p.name, p.status, p.version,
	coalesce(jsonb_array_length(p.canvas_data -> 'nodes'), 0),
	coalesce(jsonb_array_length(p.canvas_data -> 'edges'), 0),
	(select count(*)::int from generation g where g.project_id = p.id),
	p.deleted_at, p.updated_at, p.created_at`

const projectFrom = ` from project p
	left join workspace w on w.id = p.workspace_id
	left join app_user u on u.id = p.created_by_user_id `

func scanProject(row pgx.Row) (ProjectRow, error) {
	var p ProjectRow
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.WorkspaceName, &p.OwnerUserID,
		&p.OwnerName, &p.Name, &p.Status, &p.Version, &p.NodeCount, &p.EdgeCount,
		&p.GenCount, &p.DeletedAt, &p.UpdatedAt, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectRow{}, ErrNotFound
	}
	return p, err
}

// ProjectFilter 限定后台项目检索。
type ProjectFilter struct {
	// Keyword 一个框查两样：项目 ID / 工作区 ID。
	Keyword string
	// Name 按名字模糊找（用户报障时往往只说得出项目叫什么）。
	Name string
	// Status 为空表示两种都要 —— ⚠️ 默认**包含回收站里的**：
	// 后台来查项目十有八九正是因为"用户说他的项目不见了"。
	Status   string
	Limit    int
	CursorID int64
}

func (r *Repo) ListProjects(ctx context.Context, f ProjectFilter) ([]ProjectRow, error) {
	args := []any{}
	cond := []string{}
	add := func(v any) string {
		args = append(args, v)
		return "$" + itoa(len(args))
	}
	if f.Status != "" {
		cond = append(cond, "p.status = "+add(f.Status))
	}
	if id := ParseID(f.Keyword); id > 0 {
		p := add(id)
		cond = append(cond, "(p.id = "+p+" or p.workspace_id = "+p+")")
	}
	if n := strings.TrimSpace(f.Name); n != "" {
		// ⚠️ 参数化 + 转义 like 的元字符：名字来自后台输入框，
		// 而后台账号恰恰权限最大。`%` 不转义的话一个 `%` 会拖出全表。
		cond = append(cond, "p.name ilike "+add("%"+escapeLike(n)+"%")+" escape '\\'")
	}
	if f.CursorID > 0 {
		cond = append(cond, "p.id < "+add(f.CursorID))
	}
	where := ""
	if len(cond) > 0 {
		where = " where " + strings.Join(cond, " and ")
	}
	limit := add(f.Limit)
	rows, err := r.pool.Query(ctx, `select `+projectCols+projectFrom+where+
		` order by p.id desc limit `+limit, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRow
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject 取一个项目（不带工作区条件）。
func (r *Repo) GetProject(ctx context.Context, id int64) (ProjectRow, error) {
	return scanProject(r.pool.QueryRow(ctx,
		`select `+projectCols+projectFrom+` where p.id = $1`, id))
}

// ProjectCanvas 取画布原文。
//
// ⚠️ **原样给**，不重新组装：排障要看的恰恰是"库里到底存着什么"。
// 重新序列化一遍会引入第二种解释，而两种解释不一致时没人知道该信哪个
// （同渲染追溯给快照原文的理由）。
func (r *Repo) ProjectCanvas(ctx context.Context, id int64) (json.RawMessage, error) {
	var raw json.RawMessage
	err := r.pool.QueryRow(ctx,
		`select canvas_data from project where id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return raw, err
}

// RestoreProject 把回收站里的项目恢复回来。
//
// ⚠️ 带 `status='deleted'` 条件：返回 0 行表示它本来就没被删 ——
// 静默成功的话客服会告诉用户"我已经帮你恢复了"，而用户那边什么都没变
// （它一直在，只是他自己找不到）。
//
// ⚠️ **不刷 `version`**：那是画布的乐观锁。恢复动了它的话，用户那个还开着
// 的标签页下一次保存会撞冲突 —— 而他什么都没做错。
func (r *Repo) RestoreProject(ctx context.Context, id int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update project set status = 'active', deleted_at = null, updated_at = now()
		where id = $1 and status = 'deleted'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// DeleteProject 后台软删。
//
// ⚠️ 仍然是**软删**，后台没有硬删的入口：硬删不可逆，而回收站的清理作业
// 已经会在保留期后清掉它（保留期算 `deleted_at`）。给后台一个"彻底删除"
// 按钮，只会让一次误点变成不可恢复的事故。
func (r *Repo) DeleteProject(ctx context.Context, id int64) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		update project set status = 'deleted', deleted_at = now(), updated_at = now()
		where id = $1 and status = 'active'`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// RenameProject 后台改名。
//
// ⚠️ 后台能改的**只有名字**，改不了画布。理由不是偷懒：画布有乐观锁与
// 在线协作（P12-02），后台在用户不知情时写一次 `canvas_data`，
// 用户那边的自动保存要么撞 409（他刚画的连线保不进去），要么被这次写入
// 直接覆盖 —— 而两种情况他都看不出发生了什么。
// 「代编辑画布」要先有一套"接管编辑权"的交互，不在本任务范围（见挂账）。
func (r *Repo) RenameProject(ctx context.Context, id int64, name string) (bool, error) {
	tag, err := r.pool.Exec(ctx,
		`update project set name = $2, updated_at = now() where id = $1`, id, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// escapeLike 转义 like 的元字符。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	return strings.ReplaceAll(s, "_", "\\_")
}

// ParseID 把关键词当雪花号解析；不是纯数字时回 0。
//
// ⚠️ 解析失败**不报错**：为一个格式问题把整个列表挡掉不值。
func ParseID(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int64(c-'0')
		if v < 0 {
			return 0
		}
	}
	return v
}
