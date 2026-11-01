package repo

import (
	"context"
	"encoding/json"
)

// AppendScenePins 把规划时新钉的选项追加进营销会话的 scene_input.pins（MKT-T-20、MKD-5）。
//
// pins 是 `[{kind, code, version, source}]` 数组。⚠️ 只追加 (kind, code) 还没钉过的 ——
// 同一会话里同一 code 只认第一次的版本（marketing/domain.MergePins 的规则），这里在库里再兜一次底：
// 两次规划之间若有并发写入，后来者也不会把已钉住的版本改掉。
// 只改 scene='marketing' 的会话；通用会话不存在 pins，改不到即返回 ErrNotFound。
func (r *Repo) AppendScenePins(ctx context.Context, sessionID int64, pins json.RawMessage) error {
	return r.exec1(ctx, `
		update agent_session set scene_input = jsonb_set(scene_input, '{pins}',
			coalesce(scene_input->'pins', '[]'::jsonb) || coalesce((
				select jsonb_agg(p) from jsonb_array_elements($2::jsonb) p
				where not exists (
					select 1 from jsonb_array_elements(coalesce(scene_input->'pins', '[]'::jsonb)) e
					where e->>'kind' = p->>'kind' and e->>'code' = p->>'code')
			), '[]'::jsonb), true),
			updated_at = now()
		where id = $1 and scene = 'marketing'`, sessionID, jsonOr(pins, "[]"))
}

// PickPinnedInProject 报告某个选项版本是否被本项目的营销会话确认过：建会话时选的（scene_input.picks），
// 或规划时新钉的（scene_input.pins）。受理拼营销片段之前据此把关（04 文档 P-46）。
//
// ⚠️ 只看营销会话（scene = marketing）：通用会话不会钉选项，营销项目之外的生成不该带营销片段。
func (r *Repo) PickPinnedInProject(ctx context.Context, workspaceID, projectID int64, kind, code string,
	version int) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `
		select exists (
			select 1 from agent_session s
			where s.workspace_id = $1 and s.project_id = $2 and s.scene = 'marketing'
			  and (s.scene_input->'pins' @> jsonb_build_array(jsonb_build_object('kind', $3::text, 'code', $4::text, 'version', $5::int))
			       or s.scene_input->'picks'->$3::text @> jsonb_build_object('code', $4::text, 'version', $5::int)))`,
		workspaceID, projectID, kind, code, version).Scan(&ok)
	return ok, err
}
