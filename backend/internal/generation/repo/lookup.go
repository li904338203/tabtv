package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// OutputRef 是一次产出的归属与可用性，供画布域解析输入时查（GO-27）。
//
// ⚠️ 只回**事实**，不做判断：能不能用（同项目？成功了吗？类型兼容吗？）
// 由画布域按它的规则决定。这里替它判的话，规则会散在两个域里，
// 而改规则的人只会改到其中一处。
type OutputRef struct {
	ProjectID  int64
	NodeID     string
	ResultType string
	Status     string
	// MediaID 取第一个媒体产物；文本产物取 Text。
	MediaID int64
	Text    string
	// DurationMs 第一个媒体产物的音视频时长（毫秒，0 = 未知）：输入区间据此校验终点（docs/agent/smartcut SCD-34）。
	DurationMs int
}

// LookupOutput 按 ID 查一次产出。
//
// ⚠️ 取**第一个**产物是有意的简化，且只在"把上游节点接成下游输入"这一处成立：
// 一个节点在画布上就是一个产物位。多产物的选择（用户挑第几张）发生在
// 前端的节点面板里，选完以后传过来的是那一项的 generation + idx —— 那要等
// 组节点（P07）把 idx 也带进来。在那之前，这里**不隐式扩展成"随便挑一个"**。
func (r *Repo) LookupOutput(ctx context.Context, id int64) (OutputRef, error) {
	var out OutputRef
	err := r.pool.QueryRow(ctx, `
		select g.project_id, g.node_id, g.result_type, g.status,
		       coalesce(o.media_object_id, 0), coalesce(o.text_content, ''), coalesce(m.duration_ms, 0)
		from generation g
		left join generation_output o
		  on o.generation_id = g.id and o.idx = 0
		left join media_object m on m.id = o.media_object_id
		where g.id = $1`, id).
		Scan(&out.ProjectID, &out.NodeID, &out.ResultType, &out.Status,
			&out.MediaID, &out.Text, &out.DurationMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutputRef{}, ErrNotFound
	}
	return out, err
}
