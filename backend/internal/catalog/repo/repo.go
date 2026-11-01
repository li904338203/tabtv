// Package repo 是画布配置目录的数据访问层。
package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound  = errors.New("配置项不存在")
	ErrDuplicate = errors.New("code 已存在")
	ErrInUse     = errors.New("配置项已被引用")
	// ErrVersionConflict 乐观锁冲突：并发编辑时后提交者必须先重载。
	ErrVersionConflict = errors.New("配置已被他人修改")
)

type Repo struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

type NodeType struct {
	ID        int64           `json:"-"`
	Code      string          `json:"code"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Icon      string          `json:"icon,omitempty"`
	Sort      int             `json:"sort"`
	IsEnabled bool            `json:"isEnabled"`
	Params    []NodeParamView `json:"params,omitempty"`
}

// NodeParamView 是合成后的参数视图：控件配置已按优先级合并。
type NodeParamView struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Control      string `json:"control"`
	Group        string `json:"group,omitempty"`
	Sort         int    `json:"sort"`
	IsRequired   bool   `json:"isRequired"`
	DefaultValue string `json:"defaultValue,omitempty"`
	// Config 是合并后的最终配置，由 app 层按优先级算出。
	Config json.RawMessage `json:"config"`
	// Base / Override 是合并前的两层，不下发前端。
	Base     []byte `json:"-"`
	Override []byte `json:"-"`
}

type ConnectionRule struct {
	SourceType       string `json:"sourceType"`
	TargetType       string `json:"targetType"`
	IsAllowed        bool   `json:"isAllowed"`
	DefaultOperation string `json:"defaultOperation,omitempty"`
}

type ImagePreset struct {
	Code        string          `json:"code"`
	Name        string          `json:"name"`
	PresetGroup string          `json:"presetGroup"`
	Spec        string          `json:"spec"`
	InputMode   string          `json:"inputMode"`
	Icon        string          `json:"icon,omitempty"`
	Params      json.RawMessage `json:"params"`
	Dimensions  json.RawMessage `json:"dimensions,omitempty"`
	Operation   string          `json:"operation,omitempty"`
	Sort        int             `json:"sort"`
	// Description 选中该预设后提示词框的占位文案（「描述一个 10-15 秒内能演完的小剧情…」）。
	// 没有它用户不知道这个玩法要他写什么（老系统有，迁移 00033 补上）。
	Description string `json:"description,omitempty"`
	// IsNew 选择器里的 NEW 角标。
	IsNew bool `json:"isNew"`
	// ⚠️ prompt 是服务端拼接用的，**不下发给前端**：
	// 前端拼出来的词会落进 canvas_data，用户改一个字玩法就失效（CD 见图片预设设计）。
	Prompt string `json:"-"`
}

// Revision 取配置版本。写配置即 bump，缓存键含 revision（CD-14）。
func (r *Repo) Revision(ctx context.Context, scope string) (int64, error) {
	var rev int64
	err := r.pool.QueryRow(ctx,
		`select revision from config_revision where scope = $1`, scope).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// BumpRevision 递增配置版本。
// 所有写配置的操作都必须调它 —— 漏掉就会让前端继续用旧配置。
func (r *Repo) BumpRevision(ctx context.Context, scope string, adminID int64) error {
	_, err := r.pool.Exec(ctx, `
		insert into config_revision (scope, revision, updated_by_admin_id, updated_at)
		values ($1, 1, nullif($2::bigint,0), now())
		on conflict (scope) do update
		set revision = config_revision.revision + 1,
		    updated_by_admin_id = excluded.updated_by_admin_id,
		    updated_at = now()`, scope, adminID)
	return err
}

// ListNodeTypes 取节点目录及其参数绑定，控件配置已在 SQL 侧连表取出。
func (r *Repo) ListNodeTypes(ctx context.Context, enabledOnly bool) ([]NodeType, error) {
	rows, err := r.pool.Query(ctx, `
		select id, code, name, kind, coalesce(icon,''), sort, is_enabled
		from node_type
		where (not $1 or is_enabled)
		order by sort, code`, enabledOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeType
	byID := map[int64]int{}
	for rows.Next() {
		var n NodeType
		if err := rows.Scan(&n.ID, &n.Code, &n.Name, &n.Kind, &n.Icon, &n.Sort, &n.IsEnabled); err != nil {
			return nil, err
		}
		byID[n.ID] = len(out)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// 一次取全部绑定，避免 N+1。
	prows, err := r.pool.Query(ctx, `
		select np.node_type_id, pd.code, pd.name, pd.control_type,
		       coalesce(np.param_group,''), np.sort, np.is_required,
		       coalesce(np.default_value,''), pd.config, np.config_override
		from node_param np
		join param_def pd on pd.id = np.param_def_id
		where pd.is_enabled
		order by np.sort, pd.code`)
	if err != nil {
		return nil, err
	}
	defer prows.Close()
	for prows.Next() {
		var nodeID int64
		var v NodeParamView
		var base, override []byte
		if err := prows.Scan(&nodeID, &v.Code, &v.Name, &v.Control, &v.Group,
			&v.Sort, &v.IsRequired, &v.DefaultValue, &base, &override); err != nil {
			return nil, err
		}
		v.Base, v.Override = base, override
		if idx, ok := byID[nodeID]; ok {
			out[idx].Params = append(out[idx].Params, v)
		}
	}
	return out, prows.Err()
}

func (r *Repo) ListConnectionRules(ctx context.Context) ([]ConnectionRule, error) {
	rows, err := r.pool.Query(ctx, `
		select source_type, target_type, is_allowed, coalesce(default_operation,'')
		from connection_rule order by source_type, target_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectionRule
	for rows.Next() {
		var c ConnectionRule
		if err := rows.Scan(&c.SourceType, &c.TargetType, &c.IsAllowed, &c.DefaultOperation); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repo) ListImagePresets(ctx context.Context, enabledOnly bool) ([]ImagePreset, error) {
	rows, err := r.pool.Query(ctx, `
		select code, name, preset_group, spec, input_mode, coalesce(icon,''),
		       params, dimensions, coalesce(operation,''), sort, coalesce(prompt,''),
		       coalesce(description,''), is_new
		from image_preset
		where (not $1 or is_enabled)
		order by preset_group, sort, code`, enabledOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImagePreset
	for rows.Next() {
		var p ImagePreset
		if err := rows.Scan(&p.Code, &p.Name, &p.PresetGroup, &p.Spec, &p.InputMode,
			&p.Icon, &p.Params, &p.Dimensions, &p.Operation, &p.Sort, &p.Prompt,
			&p.Description, &p.IsNew); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type ParamDef struct {
	ID        int64
	Code      string
	Name      string
	Control   string
	Config    []byte
	IsEnabled bool
}

func (r *Repo) ListParamDefs(ctx context.Context) ([]ParamDef, error) {
	rows, err := r.pool.Query(ctx,
		`select id, code, name, control_type, config, is_enabled from param_def order by code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParamDef
	for rows.Next() {
		var p ParamDef
		if err := rows.Scan(&p.ID, &p.Code, &p.Name, &p.Control, &p.Config, &p.IsEnabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertParamDef 新增或更新参数定义。enabled 为 nil 时**不改**启用状态（新建默认启用）。
//
// ⚠️ 此前不碰 is_enabled，而停用走的是另一条路径 —— 于是**停用之后没有任何接口能重新启用**，
// 运营点错一次「停用」，这个参数就从所有节点上永久消失了（P15-05 发现）。
func (r *Repo) UpsertParamDef(ctx context.Context, id int64, code, name, control string, config []byte,
	enabled *bool) error {
	_, err := r.pool.Exec(ctx, `
		insert into param_def (id, code, name, control_type, config, is_enabled)
		values ($1,$2,$3,$4,$5, coalesce($6, true))
		on conflict (code) do update
		set name = excluded.name, control_type = excluded.control_type,
		    config = excluded.config, is_enabled = coalesce($6, param_def.is_enabled),
		    updated_at = now()`,
		id, code, name, control, config, enabled)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

// DisableParamDef 停用而非删除。
// ⚠️ 有引用不能删（02 §2）：历史节点的 params 仍带着这个 code，
// 删掉会让旧项目打开即报错。
func (r *Repo) DisableParamDef(ctx context.Context, code string) error {
	tag, err := r.pool.Exec(ctx,
		`update param_def set is_enabled = false, updated_at = now() where code = $1`, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) UpsertConnectionRule(ctx context.Context, id int64, c ConnectionRule) error {
	var op any
	if c.DefaultOperation != "" {
		op = c.DefaultOperation
	}
	_, err := r.pool.Exec(ctx, `
		insert into connection_rule (id, source_type, target_type, is_allowed, default_operation)
		values ($1,$2,$3,$4,$5)
		on conflict (source_type, target_type) do update
		set is_allowed = excluded.is_allowed,
		    default_operation = excluded.default_operation, updated_at = now()`,
		id, c.SourceType, c.TargetType, c.IsAllowed, op)
	return err
}
