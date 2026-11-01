package repo

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/agent/domain"
)

// 媒体引用保护（MKT-T-23 / MKD-8）：回答「这个媒体还有没有 Agent 会话在用」。
//
// ⚠️ 实现 `media/app.ReferenceChecker`，是**计算式**的删除保护（P14-03）。
// 在此之前 Agent 域没有实现它 —— 创造页会话的附件、营销会话的商品图 / 角色图 / 源视频，
// 用户在素材库一删就没了，回到会话里是裂图，而回收器也照样把它们收走。
//
// 引用来源两处：
//   - `agent_message.refs`：用户消息的附件（`[{"kind":"media","id":"<id>",...}]`），
//     走 GIN `idx_agent_message_refs`（jsonb_path_ops，`@>` 可用）。
//   - `agent_session.scene_input`（只有营销会话非空）：`products` 是媒体 ID 字符串数组，
//     `characters` / `sourceVideo` 里 kind=media 的才是媒体（kind=asset 的是脚本资产，
//     其图由脚本域那份检查器保护）；角色库角色快照里的参考音频 `characters[].voiceMediaId`
//     （MKT-T-37，MKD-33：只存不发，但会话快照引用着它，删掉的话会话详情里「有参考音频」是假的）。
//     走部分 GIN `idx_agent_session_scene_input`，
//     所以条件里必须带 `scene = 'marketing'` 才命中索引。
//
// ⚠️ **跨工作区查**（同 media/app 的要求）；**不按会话状态过滤**：Agent 会话没有删除语义，
// failed / cancelled 的会话仍可打开、仍显示那张附件。会话行随项目被真正清理时引用自然消失。
//
// 按场景分两个入口，是为了让文案分得开（「正被营销会话引用」/「正被 Agent 会话引用」）：
// 两个检查器各查一次，代价是同一张图被查两遍 —— 都走索引，可以接受。

// MarketingMediaReferences 返回引用该媒体的营销会话 ID。
func (r *Repo) MarketingMediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	return r.mediaReferences(ctx, mediaID, domain.SceneMarketing)
}

// GeneralMediaReferences 返回引用该媒体的通用 Agent 会话 ID（创造页、画布面板、MCP）。
func (r *Repo) GeneralMediaReferences(ctx context.Context, mediaID int64) ([]int64, error) {
	return r.mediaReferences(ctx, mediaID, domain.SceneGeneral)
}

func (r *Repo) mediaReferences(ctx context.Context, mediaID int64, scene domain.Scene) ([]int64, error) {
	id := strconv.FormatInt(mediaID, 10)
	ref, err := json.Marshal([]map[string]string{{"kind": "media", "id": id}})
	if err != nil {
		return nil, err
	}
	products, err := json.Marshal(map[string][]string{"products": {id}})
	if err != nil {
		return nil, err
	}
	chars, err := json.Marshal(map[string][]map[string]string{"characters": {{"kind": "media", "id": id}}})
	if err != nil {
		return nil, err
	}
	video, err := json.Marshal(map[string]map[string]string{"sourceVideo": {"kind": "media", "id": id}})
	if err != nil {
		return nil, err
	}
	voice, err := json.Marshal(map[string][]map[string]string{"characters": {{"voiceMediaId": id}}})
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		select m.session_id from agent_message m
		  join agent_session s on s.id = m.session_id
		 where m.refs @> $1::jsonb and s.scene = $2
		union
		select s.id from agent_session s
		 where $2 = 'marketing' and s.scene = 'marketing'
		   and (s.scene_input @> $3::jsonb or s.scene_input @> $4::jsonb or s.scene_input @> $5::jsonb
		        or s.scene_input @> $6::jsonb)
		order by 1`,
		string(ref), string(scene), string(products), string(chars), string(video), string(voice))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var sid int64
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		out = append(out, sid)
	}
	return out, rows.Err()
}
