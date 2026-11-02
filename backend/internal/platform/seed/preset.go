package seed

import (
	"context"
	"database/sql"
)

// 图片预设目录（NEW-12、CV-14）。
//
// ⚠️ 这些是**配置不是代码**：调词、加工具、下线工具都应当只改这里或后台，
// 不该发版。所以 `prompt` 写在数据里，代码里一个工具名都不硬编码。
//
// ⚠️ `prompt` 字段**不下发给前端**（repo 的 json tag 是 `-`）：
// 前端拼出来的词会落进 canvasData，用户改一个字玩法就失效，
// 历史节点也永远停在旧词。
//
// ⚠️ 下线用 `is_enabled=false` **不删行**：历史节点的 params 里还存着
// 这个 code，删了就再也说不清"那张图当时用的是什么配方"。

type presetSeed struct {
	code, name, group, spec, inputMode, operation string
	prompt                                        string
	params                                        string
	dimensions                                    string
	sort                                          int
	// icon 选择器里的图标（lucide 的 kebab-case 名）；description 选中后提示词框的占位文案；
	// isNew 选择器里的 NEW 角标。三样都是老系统有、P15-02 之前新 seed 没写的。
	icon, description string
	isNew             bool
}

// presets 是随系统发布的基础**工具**集（P09）。
//
// ⚠️ 全部归 `tool` 组（迁移 00033）：它们对应图片工具条上的按钮（人像质感、打光、高清、
// 抠图、涂抹重绘……），不出现在「图片预设」弹窗与「九宫格」下拉里 —— 那两处是老系统
// 定稿的玩法目录（`preset_picker.go`）。涂抹重绘要蒙版编辑器，放进选择器里点一下是用不对的。
// 代号一个不改：`/image-tools`、P09-04 工具矩阵与 E2E 都按代号引用它们。
//
// ⚠️ 参数键用节点参数的代号 `aspect_ratio`（此前写的是 `aspectRatio`，
// 节点参数与上游协议都不认，预设带的画幅从来没生效过）。
var presets = []presetSeed{
	// ---- sheet：设定图 ----
	{
		code: "character_sheet", name: "角色三视图", group: "tool", spec: "sheet",
		inputMode: "either", operation: "text2img", sort: 10,
		// ⚠️ 版式由**配方**固定，不让模型自由发挥：模型会自作主张给角色
		// 塞一把武器、换一身衣服 —— 而设定图的全部价值就是下游每一镜
		// 拿它当参考，多出来的东西会被每一镜继承。
		prompt: "输出角色设定图：同一角色的正面、四分之三侧面、背面三个视图横向排列，" +
			"同一比例、同一光照、纯色背景。不持任何道具，不出现其他人物，不加文字与水印。",
		params: `{"aspect_ratio":"16:9"}`,
	},
	{
		code: "prop_sheet", name: "道具多视图", group: "tool", spec: "sheet",
		inputMode: "either", operation: "text2img", sort: 20,
		prompt: "输出道具设定图：同一物件的六个视图（正/背/左/右/俯/仰）2×3 排列，" +
			"同一比例、同一光照、纯色背景。不出现人手与人物，不加文字与水印。",
		params: `{"aspect_ratio":"3:2"}`,
	},

	// ---- tuning：质感调节（都要有原图）----
	{
		code: "portrait_retouch", name: "人像质感", group: "tool", spec: "retouch",
		inputMode: "image_required", operation: "img2img", sort: 10,
		// ⚠️ "保持五官与身份不变"这句不能省：不写的话模型会顺手"美化"成另一个人，
		// 而用户要的是同一个人拍得更好看。
		prompt: "提升人像质感：皮肤通透自然、保留毛孔与纹理，" +
			"**保持五官比例与身份特征完全不变**，不改变发型、服装与背景。",
		dimensions: `[{"code":"strength","name":"强度","default":"medium","options":[
			{"value":"light","label":"轻微","prompt":"修饰幅度克制，接近原片。"},
			{"value":"medium","label":"适中","prompt":"修饰幅度适中，保留真实感。"},
			{"value":"strong","label":"明显","prompt":"修饰幅度明显，偏商业精修。"}]}]`,
	},
	{
		code: "relight", name: "重新打光", group: "tool", spec: "retouch",
		inputMode: "image_required", operation: "img2img", sort: 20,
		prompt: "重新打光：保持人物、构图与背景内容不变，只改变光照与阴影关系。",
		dimensions: `[
			{"code":"direction","name":"光线方向","default":"front","options":[
				{"value":"front","label":"正面光","prompt":"光源来自正前方，面部均匀受光，阴影柔和。"},
				{"value":"side","label":"侧光","prompt":"光源来自侧方，明暗交界清晰，立体感强。"},
				{"value":"rim","label":"轮廓光","prompt":"光源来自背侧，勾出人物轮廓，主体略暗。"}]},
			{"code":"mood","name":"氛围","default":"natural","options":[
				{"value":"natural","label":"自然","prompt":"色温自然，不做风格化。"},
				{"value":"warm","label":"暖调","prompt":"整体偏暖，接近黄昏色温。"},
				{"value":"cool","label":"冷调","prompt":"整体偏冷，接近阴天或月光。"}]}]`,
	},
	{
		code: "upscale", name: "高清放大", group: "tool", spec: "retouch",
		inputMode: "image_required", operation: "img2img", sort: 30,
		// ⚠️ 明确"不要重新构图"：放大工具最常见的投诉就是"放大之后变成另一张图"。
		prompt: "提升清晰度与细节：锐化边缘、补充纹理、消除压缩噪点。" +
			"**不要重新构图、不要改变内容、不要添加原图没有的元素。**",
		dimensions: `[{"code":"scale","name":"倍数","default":"2x","options":[
			{"value":"2x","label":"2 倍","prompt":"细节提升幅度适中。"},
			{"value":"4x","label":"4 倍","prompt":"细节提升幅度较大，适合小图放大。"}]}]`,
	},

	{
		code: "inpaint", name: "涂抹重绘", group: "tool", spec: "retouch",
		inputMode: "image_required", operation: "inpaint", sort: 40,
		// ⚠️ 明确"只改蒙版覆盖的区域"：不写的话模型会顺手重绘整张，
		// 用户涂了一小块，回来发现整张图都变了。
		prompt: "只重绘蒙版覆盖的区域：新内容要与周围的透视、光照、材质自然衔接，" +
			"**蒙版之外的部分必须逐像素保持不变**。",
	},
	{
		code: "remove_bg", name: "抠图", group: "tool", spec: "retouch",
		inputMode: "image_required", operation: "remove_bg", sort: 50,
		// ⚠️ 透明输出要说明白：不说的话模型会给一张白底图，
		// 而用户要的是能叠到别处去的 PNG。
		prompt: "把主体从背景中分离，输出**背景完全透明的 PNG**：" +
			"保留发丝与边缘的半透明过渡，不要用纯色填充背景。",
		params: `{"output_format":"png"}`,
	},

	// ---- space：空间与机位 ----
	{
		code: "panorama", name: "全景展开", group: "tool", spec: "panorama",
		inputMode: "image_required", operation: "img2img", sort: 10,
		prompt: "把画面向左右扩展成全景：延续原有的透视、光照与材质，" +
			"新增部分要与原图自然衔接，**原有区域保持不变**。",
		params: `{"aspect_ratio":"21:9"}`,
	},
	{
		code: "multi_angle", name: "多角度", group: "tool", spec: "variation",
		inputMode: "image_required", operation: "img2img", sort: 20,
		// ⚠️ 这是"换机位"不是"换场景"：不强调的话模型会连人带景一起换掉。
		prompt: "从另一个机位重新呈现同一场景：**保持同一空间、同一批对象、" +
			"同一时间与光照**，只改变相机位置与角度。",
		dimensions: `[{"code":"angle","name":"机位","default":"side","options":[
			{"value":"side","label":"侧面","prompt":"相机移到侧面约 45 度。"},
			{"value":"high","label":"俯视","prompt":"相机升高俯拍。"},
			{"value":"low","label":"仰视","prompt":"相机降低仰拍。"}]}]`,
	},
}

func seedImagePresets(ctx context.Context, tx *sql.Tx, nextID func() int64) error {
	all := append(append([]presetSeed{}, pickerPresets...), presets...)
	for _, p := range all {
		params := p.params
		if params == "" {
			params = "{}"
		}
		var dims any
		if p.dimensions != "" {
			dims = p.dimensions
		}
		hash := builtinHash(p.code, p.name, p.group, p.spec, p.inputMode, p.prompt, params, p.dimensions,
			p.operation, p.sort, p.icon, p.description, p.isNew)
		// 内置版本规则（TT-T-24）：用户在设置页改过的预设不覆盖（来源系统是无条件 do update）。
		if _, err := tx.ExecContext(ctx, `
			insert into image_preset (id, code, name, preset_group, spec, input_mode,
				prompt, params, dimensions, operation, sort, is_enabled,
				icon, description, is_new, builtin_hash)
			values ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb,$10,$11,true,
				nullif($12,''), nullif($13,''), $14, $15)
			on conflict (code) do update set
				name = excluded.name, preset_group = excluded.preset_group,
				spec = excluded.spec, input_mode = excluded.input_mode,
				prompt = excluded.prompt, params = excluded.params,
				dimensions = excluded.dimensions, operation = excluded.operation,
				sort = excluded.sort, icon = excluded.icon,
				description = excluded.description, is_new = excluded.is_new,
				builtin_hash = excluded.builtin_hash, updated_at = now()
			where image_preset.is_user_modified = false`,
			nextID(), p.code, p.name, p.group, p.spec, p.inputMode,
			p.prompt, params, dims, p.operation, p.sort,
			p.icon, p.description, p.isNew, hash); err != nil {
			return err
		}
	}
	return nil
}
