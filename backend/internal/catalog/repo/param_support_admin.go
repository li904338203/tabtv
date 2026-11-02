package repo

import (
	"context"
	"encoding/json"
)

// 型号级参数覆盖的读写（TT-T-18）。来源系统只有 seed 写这张表、没有任何写接口，
// 桌面版的使用者接自己的中转或新型号时要能改「这个型号能选哪些时长 / 清晰度」。

// AdminParamSupport 是设置页看到的一行覆盖（含停用参数定义上的覆盖，便于清理）。
type AdminParamSupport struct {
	ParamCode     string          `json:"paramCode"`
	ParamName     string          `json:"paramName"`
	ParamEnabled  bool            `json:"paramEnabled"`
	AllowedValues json.RawMessage `json:"allowedValues"`
	DefaultValue  string          `json:"defaultValue"`
	Override      json.RawMessage `json:"override"`
}

// ParamDefBrief 是可被覆盖的参数定义（下拉用）。
type ParamDefBrief struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	ControlType string `json:"controlType"`
}

// ListParamSupportsAdmin 取一个模型的全部覆盖。模型不存在时 ErrNotFound。
func (r *Repo) ListParamSupportsAdmin(ctx context.Context, modelCode string) ([]AdminParamSupport, error) {
	var modelID int64
	if err := r.pool.QueryRow(ctx, `select id from model where code = $1`, modelCode).Scan(&modelID); err != nil {
		return nil, ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `
		select pd.code, pd.name, pd.is_enabled,
		       coalesce(mps.allowed_values, 'null'::jsonb), coalesce(mps.default_value, ''),
		       coalesce(mps.override, 'null'::jsonb)
		from model_param_support mps
		join param_def pd on pd.id = mps.param_def_id
		where mps.model_id = $1
		order by pd.code`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminParamSupport{}
	for rows.Next() {
		var s AdminParamSupport
		if err := rows.Scan(&s.ParamCode, &s.ParamName, &s.ParamEnabled, &s.AllowedValues, &s.DefaultValue, &s.Override); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListParamDefsBrief 取启用中的参数定义。
func (r *Repo) ListParamDefsBrief(ctx context.Context) ([]ParamDefBrief, error) {
	rows, err := r.pool.Query(ctx, `select code, name, control_type from param_def where is_enabled order by code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ParamDefBrief{}
	for rows.Next() {
		var p ParamDefBrief
		if err := rows.Scan(&p.Code, &p.Name, &p.ControlType); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertParamSupport 写一行覆盖（同一模型同一参数只有一行）。模型或参数不存在时 ErrNotFound。
// allowed / override 为 nil 表示该列置空。
func (r *Repo) UpsertParamSupport(ctx context.Context, id int64, modelCode, paramCode string,
	allowed, override json.RawMessage, defaultValue string) error {
	tag, err := r.pool.Exec(ctx, `
		insert into model_param_support (id, model_id, param_def_id, allowed_values, default_value, override)
		select $1, m.id, pd.id, $4::jsonb, nullif($5, ''), $6::jsonb
		from model m, param_def pd where m.code = $2 and pd.code = $3
		on conflict (model_id, param_def_id) do update
		set allowed_values = excluded.allowed_values, default_value = excluded.default_value,
		    override = excluded.override, updated_at = now()`,
		id, modelCode, paramCode, nullableJSON(allowed), defaultValue, nullableJSON(override))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteParamSupport 删掉一行覆盖（之后这个参数回到节点通用配置）。没有这一行时 ErrNotFound。
func (r *Repo) DeleteParamSupport(ctx context.Context, modelCode, paramCode string) error {
	tag, err := r.pool.Exec(ctx, `
		delete from model_param_support mps using model m, param_def pd
		where mps.model_id = m.id and mps.param_def_id = pd.id and m.code = $1 and pd.code = $2`,
		modelCode, paramCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}
