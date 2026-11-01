package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GetUserPref 取用户级模型偏好。没有记录时返回 `{}`（不是错误：没设过偏好是常态）。
func (r *Repo) GetUserPref(ctx context.Context, userID int64) (json.RawMessage, error) {
	var prefs json.RawMessage
	err := r.pool.QueryRow(ctx, `select prefs from agent_user_pref where user_id = $1`, userID).Scan(&prefs)
	if errors.Is(err, pgx.ErrNoRows) {
		return json.RawMessage(`{}`), nil
	}
	return prefs, err
}

// UpsertUserPref 整份写入用户偏好（原样存，保留前端未知字段）。
func (r *Repo) UpsertUserPref(ctx context.Context, userID int64, prefs json.RawMessage) error {
	_, err := r.pool.Exec(ctx, `
		insert into agent_user_pref (user_id, prefs) values ($1, $2)
		on conflict (user_id) do update set prefs = excluded.prefs, updated_at = now()`,
		userID, jsonOr(prefs, "{}"))
	return err
}
