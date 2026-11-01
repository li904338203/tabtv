package seed

import (
	"context"
	"database/sql"
)

// seedTemplates 写入新建项目时可选的画布模板（CV-06 的四种 + 空白）。
//
// ⚠️ 模板是**配置不是代码**：四种模板的节点与连线写在这里，
// 前端只负责渲染 canvas_data。把模板画布硬编码进前端，
// 调一个节点位置就要发一次版（这正是配置驱动画布要避免的）。
//
// 每个模板的连线都必须落在 connection_rule 允许的矩阵里，
// 否则用户一建项目就拿到一张连线非法、点生成必报错的画布。
func seedTemplates(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	rows := []struct {
		code, name, desc, canvas string
		sort                     int
	}{
		{"blank", "空白画布", "从零开始", `{"schemaVersion":1,"nodes":[],"edges":[]}`, 1},
		// ⚠️ 以下四个**逐字照老系统**（`aitv-canvas/template/TemplateConst.java`，已评审定稿）：
		// 节点标题、位置、预设、视频模式、连线顺序都不改。P15-02 之前这里是另设计的一版
		// （角色三视图是「文本 → 图片」、音频生视频的顺序是图 1 音 2、视频节点没带模式），
		// 用户点卡片得到的画布与老系统不一样。
		// ⚠️ 视频模式写在 `params.mode`：前端判断「这个视频节点走什么操作」读的就是它
		// （后端的参数代号叫 video_mode，但受理时用的是请求里的 operation，不读这个键）。
		{"story_script", "故事脚本", "剧本 → 分镜 → 批量出片",
			`{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},
			  "nodes":[{"id":"n_script","type":"script","title":"故事脚本","position":{"x":160,"y":120},"params":{}}],
			  "edges":[]}`, 2},
		{"role_three_view", "角色三视图", "角色图 → 角色三视图（图生图 + 预设）",
			`{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},
			  "nodes":[{"id":"n_role","type":"image","title":"角色图","position":{"x":80,"y":120},"params":{}},
			           {"id":"n_three_view","type":"image","title":"角色三视图","position":{"x":500,"y":120},
			            "params":{"preset":"role_three_view"}}],
			  "edges":[{"id":"e_role_three_view","source":"n_role","sourceHandle":"out","target":"n_three_view",
			            "targetHandle":"in","order":1,"operation":"img2img"}]}`, 3},
		{"first_frame_video", "首帧图生视频", "首帧图片 → 视频",
			`{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},
			  "nodes":[{"id":"n_first_frame","type":"image","title":"首帧图片","position":{"x":80,"y":120},"params":{}},
			           {"id":"n_video","type":"video","title":"视频","position":{"x":500,"y":120},
			            "params":{"mode":"img2video"}}],
			  "edges":[{"id":"e_first_frame_video","source":"n_first_frame","sourceHandle":"out","target":"n_video",
			            "targetHandle":"in","order":1,"operation":"img2video"}]}`, 4},
		{"audio_video", "音频生视频", "音频 + 参考图片 → 全能参考视频",
			`{"schemaVersion":1,"viewport":{"x":0,"y":0,"zoom":1},
			  "nodes":[{"id":"n_audio","type":"audio","title":"音频","position":{"x":80,"y":40},"params":{}},
			           {"id":"n_reference","type":"image","title":"参考图片","position":{"x":80,"y":260},"params":{}},
			           {"id":"n_video","type":"video","title":"全能参考视频","position":{"x":540,"y":150},
			            "params":{"mode":"ref2video"}}],
			  "edges":[{"id":"e_audio_video","source":"n_audio","sourceHandle":"out","target":"n_video",
			            "targetHandle":"in","order":1,"operation":"ref2video"},
			           {"id":"e_image_video","source":"n_reference","sourceHandle":"out","target":"n_video",
			            "targetHandle":"in","order":2,"operation":"ref2video"}]}`, 5},
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `
			insert into project_template (id, code, name, description, canvas_data, sort)
			values ($1, $2, $3, $4, $5::jsonb, $6)
			on conflict (code) do update set
				name = excluded.name, description = excluded.description,
				canvas_data = excluded.canvas_data, sort = excluded.sort, updated_at = now()`,
			nextID(), r.code, r.name, r.desc, r.canvas, r.sort); err != nil {
			return err
		}
	}
	// P15-02 之前「角色三视图」模板的代号是 character_sheet（且内容与老系统不同）。
	// 改回老系统的 role_three_view 之后，旧行**停用不删**：建项目时的模板选择只列启用的，
	// 而已经用它建出来的项目本来就不再引用模板（画布在建项目时就拷贝走了）。
	if _, err := tx.ExecContext(ctx,
		`update project_template set is_enabled = false, updated_at = now()
		 where code = 'character_sheet' and is_enabled`); err != nil {
		return err
	}
	return nil
}
