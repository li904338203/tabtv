package seed

import (
	"context"
	"database/sql"
)

// seedParams 写入控件字典（param_def）与节点绑定（node_param）。
//
// ⚠️ **没有这批数据，画布的参数面板就是空的** —— P04-02 实测发现
// `GET /canvas/config` 的每个节点都带着空 params 数组：表和接口都在，只是从没被填过。
//
// 配置驱动的两层在这里成形：
//   - `param_def` 是**控件字典**，按 code 复用（prompt 在文本/图片/视频节点都是同一行）；
//   - `node_param` 是**绑定**，决定某节点用哪些控件、顺序、是否必填、默认值，
//     以及 `config_override`（同一个控件在不同节点上的差异，比如视频节点的时长档位）。
//
// 合并优先级：`param_def.config < node_param.config_override < model_param_support.override`。
//
// ⚠️ **数量/体积上限一律写在 config 里，不写进前端**（参考图张数 maxCount、单张 maxSize）。
// 在前端写死张数就是把配置驱动作废 —— 调一个数就得发一次版。
func seedParams(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	defs := []struct {
		code, name, control, config string
	}{
		{"prompt", "提示词", "text",
			`{"placeholder":"描述你想要的画面","rows":4,"maxLength":2000}`},
		{"model", "模型", "select",
			// domainFrom 让前端懒加载大列表：模型/音色可能上百项，
			// 塞进配置快照会让每次拉配置都多传几十 KB。
			`{"domainFrom":"models","searchable":true}`},
		{"aspect_ratio", "画幅", "segmented",
			`{"options":[{"value":"1:1","label":"1:1"},{"value":"16:9","label":"16:9"},
			  {"value":"9:16","label":"9:16"},{"value":"4:3","label":"4:3"},{"value":"3:4","label":"3:4"}]}`},
		{"quality", "画质", "segmented",
			// ⚠️ 老系统里画质**真的发给上游**（openai 的 quality：standard 映射成 medium）；
			// 同一个弹窗里的「清晰度 1K/2K/4K」对图片从没发出去过 —— 那个不搬，搬了就是个摆设。
			`{"options":[{"value":"low","label":"低画质"},{"value":"standard","label":"标准画质"},
			  {"value":"high","label":"高画质"}]}`},
		{"count", "数量", "segmented",
			`{"options":[{"value":"1","label":"1 张"},{"value":"2","label":"2 张"},
			  {"value":"4","label":"4 张"}]}`},
		{"reference", "参考图", "upload",
			// ⚠️ maxCount 是配置。旧系统把 <input> 掐成单选、只取 files[0]，
			// 于是后端早就收 List 了，放开张数却还要改前端代码。
			`{"accept":["image/png","image/jpeg","image/webp"],"maxCount":6,"maxSize":10485760}`},
		{"preset", "玩法", "grid",
			`{"domainFrom":"imagePresets","columns":3,"clearable":true}`},
		{"video_mode", "模式", "segmented",
			`{"domainFrom":"videoModes"}`},
		{"duration", "时长", "segmented",
			`{"options":[{"value":"5","label":"5 秒"},{"value":"10","label":"10 秒"}],"unit":"秒"}`},
		{"resolution", "分辨率", "segmented",
			// ⚠️ 具体型号支持到哪一档由 model_param_support 覆盖，不在这里写死：
			// 上游放开一档就得改代码重新发版，正是配置驱动要避免的。
			`{"options":[{"value":"720p","label":"720P"},{"value":"1080p","label":"1080P"}]}`},
		{"voice", "音色", "select", `{"domainFrom":"voices","searchable":true,"previewable":true}`},
		{"speed", "语速", "slider", `{"min":0.5,"max":2,"step":0.1,"default":1}`},
		{"style_ref", "风格", "tag_group", `{"domainFrom":"materials","materialType":"style"}`},
		{"effect_ref", "特效", "tag_group", `{"domainFrom":"materials","materialType":"effect"}`},
		{"seed", "随机种子", "inline", `{"type":"number","clearable":true,"advanced":true}`},
		// 视频节点顶部的「角色库」「特效」是按钮：点开选择器，本身不存值进提示词 ——
		// 角色的主图作为参考图进生成输入，特效是连一个素材节点（服务端按素材码拼词）。
		// 一次最多挂几个角色是配置（maxCount），悬停提示（hint）也是。
		{"roleLib", "角色库", "button",
			`{"action":"openRolePicker","icon":"user-round","maxCount":4,"hint":"从角色库选角色，主图作为参考图"}`},
		{"effectLib", "特效", "button", `{"action":"openEffectPicker","icon":"sparkles","hint":"从特效库给视频加特效"}`},
		// 运镜：按钮，点开「运镜广场」（camera_move 目录）挑一个镜头运动；节点里只存选了哪一个
		// （params.camera = {code,name}），提示词由服务端按目录拼（catalog/app.CameraPrompt）。
		{"camera", "运镜", "button", `{"action":"openCameraPicker","icon":"camera","hint":"从运镜广场选择镜头运动"}`},
		// 智能剪辑（docs/agent/smartcut SCT-08）：AI 剪辑的模式、目标时长、方案数。
		{"smartcut_mode", "剪辑方式", "segmented",
			`{"options":[{"value":"mix","label":"素材混剪"},{"value":"board","label":"分镜成片"},
			  {"value":"talk","label":"口播精剪"},{"value":"narr","label":"解说成片"}]}`},
		// 目标时长：「自动」= 按素材定；自定义上限 30 分钟（SC-PARAM-21）。前端认不出 custom 时只显示固定档位。
		{"target_duration", "目标时长", "segmented",
			`{"options":[{"value":"auto","label":"自动"},{"value":"10","label":"10 秒"},{"value":"15","label":"15 秒"},
			  {"value":"20","label":"20 秒"},{"value":"30","label":"30 秒"},{"value":"60","label":"60 秒"}],
			  "custom":{"min":1,"max":1800,"unit":"秒"}}`},
		// 一次出几套方案（SC-F-80），上限是配置不是代码。
		{"plan_variants", "方案数", "segmented",
			`{"options":[{"value":"1","label":"1 套"},{"value":"2","label":"2 套"},{"value":"3","label":"3 套"}]}`},
	}
	defIDs := map[string]int64{}
	for _, d := range defs {
		var id int64
		if err := tx.QueryRowContext(ctx, `
			insert into param_def (id, code, name, control_type, config)
			values ($1, $2, $3, $4, $5::jsonb)
			on conflict (code) do update set
				name = excluded.name, control_type = excluded.control_type,
				config = excluded.config, updated_at = now()
			returning id`, nextID(), d.code, d.name, d.control, d.config).Scan(&id); err != nil {
			return err
		}
		defIDs[d.code] = id
	}

	// 绑定。param_group 决定面板分区；advanced 组在 UI 里默认折叠。
	binds := []struct {
		node, param, group string
		sort               int
		required           bool
		defaultValue       string
		override           string
	}{
		{"text", "prompt", "main", 1, true, "", `{"rows":6}`},
		{"text", "model", "main", 2, true, "", ""},

		{"image", "prompt", "main", 1, true, "", ""},
		// 上游是 3D 导演台时默认选「按白模构图出图」（DR-F-93）：默认值是配置，前端不写死预设码（DRT-07）。
		{"image", "preset", "main", 2, false, "", `{"defaultByUpstream":{"director":"whitebox_to_real"}}`},
		{"image", "reference", "main", 3, false, "", ""},
		{"image", "model", "main", 4, true, "", ""},
		// 「规格」组在界面上收进一个弹窗（老系统的「尺寸」），顺序照老弹窗：画质 → 比例 → 数量。
		{"image", "quality", "format", 5, false, "standard", ""},
		// ⚠️ 默认 16:9 照老系统（老「尺寸」弹窗的比例默认档）：新建图片节点底部栏显示
		// 「16:9 · 标准画质 · 1 张」，与老系统一致。
		{"image", "aspect_ratio", "format", 6, false, "16:9",
			// 比例用老系统定稿的 13 种 + 自适应（老系统预设里的 adaptive = 交给模型按原图定）。
			// 上游只有横/竖/方三种尺寸，协议按宽高比折算（openai.sizeFromAspect），与老系统一致。
			`{"options":[{"value":"adaptive","label":"自适应"},{"value":"1:1","label":"1:1"},{"value":"1:2","label":"1:2"},{"value":"2:1","label":"2:1"},{"value":"9:16","label":"9:16"},{"value":"16:9","label":"16:9"},{"value":"3:4","label":"3:4"},{"value":"4:3","label":"4:3"},{"value":"3:2","label":"3:2"},{"value":"2:3","label":"2:3"},{"value":"5:4","label":"5:4"},{"value":"4:5","label":"4:5"},{"value":"21:9","label":"21:9"},{"value":"9:21","label":"9:21"}]}`},
		{"image", "count", "format", 7, false, "1", ""},
		{"image", "style_ref", "format", 8, false, "", ""},
		{"image", "negative_prompt", "advanced", 9, false, "", ""},
		{"image", "seed", "advanced", 10, false, "", ""},

		// 上游是 3D 导演台时默认「全能参考」（DR-F-94、DRK-5）：截图是构图参考，不是首帧。
		{"video", "video_mode", "main", 1, true, "text2video", `{"defaultByUpstream":{"director":"ref2video"}}`},
		{"video", "prompt", "main", 2, false, "", ""},
		{"video", "model", "main", 3, true, "", ""},
		{"video", "duration", "format", 4, false, "5", ""},
		{"video", "resolution", "format", 5, false, "720p", ""},
		{"video", "aspect_ratio", "format", 6, false, "16:9",
			// 视频不支持 4:3 / 3:4：同一个控件在不同节点上的差异走 override，
			// 而不是再建一个 video_aspect_ratio 控件（那样改一处要改两处）。
			`{"options":[{"value":"16:9","label":"16:9"},{"value":"9:16","label":"9:16"},{"value":"1:1","label":"1:1"}]}`},
		{"video", "effect_ref", "format", 7, false, "", ""},
		{"video", "seed", "advanced", 8, false, "", ""},
		// 顶部一行「上传 · 角色库 · 运镜 · 特效」（前端按 code 放进顶部，见 utils/paramLayout）。
		// 上传的参考可以是图、视频、音频：图生 / 首尾帧 / 全能参考 / 视频编辑都要用得上。
		// ⚠️ 单份大小按类型给，与服务端上传校验（media/domain.DefaultLimits）一致：只给一个总上限的话，
		// 一张 50MB 的图在前端放行、传完才被拒。
		{"video", "reference", "main", 9, false, "", `{"accept":["image/png","image/jpeg","image/webp",
			"video/mp4","video/webm","audio/mpeg","audio/wav","audio/ogg"],"maxCount":9,"maxSize":104857600,
			"maxSizeByType":{"image":10485760,"video":104857600,"audio":20971520}}`},
		{"video", "roleLib", "main", 10, false, "", ""},
		{"video", "camera", "main", 11, false, "", ""},
		{"video", "effectLib", "main", 12, false, "", ""},

		{"audio", "prompt", "main", 1, true, "", `{"placeholder":"要朗读的文本","rows":4}`},
		{"audio", "voice", "main", 2, true, "", ""},
		{"audio", "model", "main", 3, true, "", ""},
		{"audio", "speed", "format", 4, false, "1", ""},

		// 3D 导演台（DRT-05）：节点下方的 AI 搭景面板（DR-F-04）。参考图张数是配置（DR-PARAM-14），不在前端写死。
		{"director", "prompt", "main", 1, false, "", `{"rows":3,"placeholder":"描述想要搭建的场景，支持通过参考图创建"}`},
		{"director", "reference", "main", 2, false, "", `{"maxCount":4}`},

		// 智能剪辑（SCT-08，dev/02 §3）：节点卡下方的指令面板。画幅 / 分辨率是导出的默认值（SC-F-89）。
		// 默认值是设计建议值（dev/02 R-5）：竖屏短视频、1080P、时长自动、1 套方案、素材混剪。
		{"smartcut", "prompt", "main", 1, false, "",
			`{"rows":3,"placeholder":"说说想剪成什么样，例如：剪一条 15 秒的新品卡点视频"}`},
		{"smartcut", "smartcut_mode", "main", 2, true, "mix", ""},
		{"smartcut", "aspect_ratio", "format", 3, false, "9:16",
			// 「跟随素材」的值是 custom：与时间线的画幅取值一致（尺寸按第一段素材算，SC-F-21）。
			`{"options":[{"value":"16:9","label":"16:9"},{"value":"9:16","label":"9:16"},{"value":"1:1","label":"1:1"},{"value":"4:3","label":"4:3"},{"value":"3:4","label":"3:4"},{"value":"21:9","label":"21:9"},{"value":"custom","label":"跟随素材"}]}`},
		{"smartcut", "resolution", "format", 4, false, "1080p", ""},
		{"smartcut", "target_duration", "format", 5, false, "auto", ""},
		{"smartcut", "plan_variants", "format", 6, false, "1", ""},
	}
	for _, b := range binds {
		defID, ok := defIDs[b.param]
		if !ok {
			continue
		}
		var override any
		if b.override != "" {
			override = b.override
		}
		var dv any
		if b.defaultValue != "" {
			dv = b.defaultValue
		}
		if _, err := tx.ExecContext(ctx, `
			insert into node_param (id, node_type_id, param_def_id, param_group,
				sort, is_required, default_value, config_override)
			select $1, nt.id, $2, $3, $4, $5, $6, $7::jsonb
			from node_type nt where nt.code = $8
			on conflict (node_type_id, param_def_id) do update set
				param_group = excluded.param_group, sort = excluded.sort,
				is_required = excluded.is_required, default_value = excluded.default_value,
				config_override = excluded.config_override, updated_at = now()`,
			nextID(), defID, b.group, b.sort, b.required, dv, override, b.node); err != nil {
			return err
		}
	}
	return nil
}
