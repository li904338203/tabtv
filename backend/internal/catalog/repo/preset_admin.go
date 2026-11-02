package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AdminImagePreset 是设置页看到的一条图片预设（含提示词、启停与内置标记，TT-T-20）。
type AdminImagePreset struct {
	Code           string          `json:"code"`
	Name           string          `json:"name"`
	PresetGroup    string          `json:"presetGroup"`
	Spec           string          `json:"spec"`
	InputMode      string          `json:"inputMode"`
	Icon           string          `json:"icon"`
	Prompt         string          `json:"prompt"`
	Params         json.RawMessage `json:"params"`
	Dimensions     json.RawMessage `json:"dimensions"`
	Operation      string          `json:"operation"`
	Sort           int             `json:"sort"`
	Description    string          `json:"description"`
	IsNew          bool            `json:"isNew"`
	IsEnabled      bool            `json:"isEnabled"`
	IsBuiltin      bool            `json:"isBuiltin"`
	IsUserModified bool            `json:"isUserModified"`
}

const adminPresetCols = `code, name, preset_group, spec, input_mode, coalesce(icon,''), coalesce(prompt,''),
	params, coalesce(dimensions, 'null'::jsonb), coalesce(operation,''), sort, coalesce(description,''), is_new,
	is_enabled, builtin_hash is not null, is_user_modified`

func scanAdminPreset(row pgx.Row) (AdminImagePreset, error) {
	var p AdminImagePreset
	err := row.Scan(&p.Code, &p.Name, &p.PresetGroup, &p.Spec, &p.InputMode, &p.Icon, &p.Prompt,
		&p.Params, &p.Dimensions, &p.Operation, &p.Sort, &p.Description, &p.IsNew,
		&p.IsEnabled, &p.IsBuiltin, &p.IsUserModified)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if string(p.Dimensions) == "null" {
		p.Dimensions = nil
	}
	return p, err
}

// AdminListImagePresets 列出全部预设（含已停用），按分组与排序。
func (r *Repo) AdminListImagePresets(ctx context.Context) ([]AdminImagePreset, error) {
	rows, err := r.pool.Query(ctx, `select `+adminPresetCols+` from image_preset order by preset_group, sort, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminImagePreset
	for rows.Next() {
		p, err := scanAdminPreset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetAdminImagePreset 按 code 取一条。
func (r *Repo) GetAdminImagePreset(ctx context.Context, code string) (AdminImagePreset, error) {
	return scanAdminPreset(r.pool.QueryRow(ctx, `select `+adminPresetCols+` from image_preset where code = $1`, code))
}

// SaveImagePreset 新建（create=true）或更新一条预设。
//
// ⚠️ 更新一律置 is_user_modified：启动时的 seed 据此不再覆盖它（内置版本规则，TT-T-24）。
// ⚠️ 新建的预设没有 builtin_hash —— 它是用户自己的，不受内置版本规则管。
func (r *Repo) SaveImagePreset(ctx context.Context, id int64, p AdminImagePreset, create bool) error {
	var dims any
	if len(p.Dimensions) > 0 && string(p.Dimensions) != "null" {
		dims = string(p.Dimensions)
	}
	params := string(p.Params)
	if params == "" || params == "null" {
		params = "{}"
	}
	if create {
		_, err := r.pool.Exec(ctx, `
			insert into image_preset (id, code, name, preset_group, spec, input_mode, prompt, params, dimensions,
				operation, sort, is_enabled, icon, description, is_new)
			values ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,nullif($10,''),$11,true,nullif($12,''),nullif($13,''),$14)`,
			id, p.Code, p.Name, p.PresetGroup, p.Spec, p.InputMode, p.Prompt, params, dims,
			p.Operation, p.Sort, p.Icon, p.Description, p.IsNew)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return err
	}
	tag, err := r.pool.Exec(ctx, `
		update image_preset set name = $2, preset_group = $3, spec = $4, input_mode = $5, prompt = $6,
			params = $7::jsonb, dimensions = $8::jsonb, operation = nullif($9,''), sort = $10,
			icon = nullif($11,''), description = nullif($12,''), is_new = $13,
			is_user_modified = true, updated_at = now()
		where code = $1`,
		p.Code, p.Name, p.PresetGroup, p.Spec, p.InputMode, p.Prompt, params, dims,
		p.Operation, p.Sort, p.Icon, p.Description, p.IsNew)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetImagePresetEnabled 启用 / 停用。停用不删行（历史节点 params 里的 code 要保持可追溯，TT-F-31）。
func (r *Repo) SetImagePresetEnabled(ctx context.Context, code string, enabled bool) error {
	tag, err := r.pool.Exec(ctx, `update image_preset set is_enabled = $2, updated_at = now() where code = $1`,
		code, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RestoreImagePreset 用内置内容覆盖一条预设并清掉「已修改」标记（恢复内置版本）。
func (r *Repo) RestoreImagePreset(ctx context.Context, p AdminImagePreset, builtinHash string) error {
	var dims any
	if len(p.Dimensions) > 0 && string(p.Dimensions) != "null" {
		dims = string(p.Dimensions)
	}
	params := string(p.Params)
	if params == "" || params == "null" {
		params = "{}"
	}
	tag, err := r.pool.Exec(ctx, `
		update image_preset set name = $2, preset_group = $3, spec = $4, input_mode = $5, prompt = $6,
			params = $7::jsonb, dimensions = $8::jsonb, operation = nullif($9,''), sort = $10,
			icon = nullif($11,''), description = nullif($12,''), is_new = $13,
			builtin_hash = $14, is_user_modified = false, updated_at = now()
		where code = $1`,
		p.Code, p.Name, p.PresetGroup, p.Spec, p.InputMode, p.Prompt, params, dims,
		p.Operation, p.Sort, p.Icon, p.Description, p.IsNew, builtinHash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
