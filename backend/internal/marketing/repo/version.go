package repo

import (
	"context"
	"encoding/json"
	"time"
)

// OptionVersion 是 marketing_option_version 的一行。
//
// ⚠️ 只增不改：本包没有任何 update / delete 这张表的语句。会话与生成快照按 (code, version)
// 回到这里取原文（MKD-5），改一行就等于悄悄改掉了一批历史生成的「用的是哪一版」。
type OptionVersion struct {
	ID               int64
	OptionID         int64
	Version          int
	Snapshot         json.RawMessage
	CreatedByAdminID int64
	CreatedAt        time.Time
}

// ListVersions 列出一条选项的全部版本，新版本在前。
func (r *Repo) ListVersions(ctx context.Context, optionID int64) ([]OptionVersion, error) {
	rows, err := r.pool.Query(ctx, `
		select id, option_id, version, snapshot, coalesce(created_by_admin_id, 0), created_at
		from marketing_option_version where option_id = $1 order by version desc`, optionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OptionVersion
	for rows.Next() {
		var v OptionVersion
		var snap []byte
		if err := rows.Scan(&v.ID, &v.OptionID, &v.Version, &snap, &v.CreatedByAdminID, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Snapshot = snap
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersionByCode 按 (code, version) 取一版原文（生成时钉版本用；不看是否在投）。
func (r *Repo) GetVersionByCode(ctx context.Context, code string, version int) (OptionVersion, error) {
	var v OptionVersion
	var snap []byte
	err := r.pool.QueryRow(ctx, `
		select v.id, v.option_id, v.version, v.snapshot, coalesce(v.created_by_admin_id, 0), v.created_at
		from marketing_option_version v join marketing_option o on o.id = v.option_id
		where o.code = $1 and v.version = $2`, code, version).
		Scan(&v.ID, &v.OptionID, &v.Version, &snap, &v.CreatedByAdminID, &v.CreatedAt)
	if err != nil {
		return OptionVersion{}, mapNoRows(err)
	}
	v.Snapshot = snap
	return v, nil
}
