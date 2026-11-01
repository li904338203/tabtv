package seed

// 图片预设「玩法」目录 —— 取自**老系统已评审定稿**的 15 个预设（P15-02）。
//
// ⚠️ 这些是出现在「图片预设」弹窗与图片工具条「九宫格」下拉里的玩法；
//    `preset.go` 里那 9 个是 P09 的**工具**（归 `tool` 组，不进选择器）。两份分开放，
//    是因为它们在界面上的位置不同，混在一起的话选择器里会出现两个「角色三视图」。
//
// ⚠️ 配方词**原样搬自老系统**，只做了三处格式转换（生成于 P15-02，不要手工逐条改）：
//    · 维度 `key/label` → `code/name`（新后端按这两个字段解析，老格式会被读成空）；
//    · 参数 `ratio` → `aspect_ratio`（节点参数与上游协议用的都是它）、保留 `quality`、
//      去掉 `clarity`（老系统里清晰度对图片从没发给过上游，是个摆设）；
//    · 两个代号与 P09 工具撞名：老 `character_sheet`（角色设定图）→ `character_setting`，
//      老 `prop_sheet`（产品设定图）→ `product_sheet`。P09 的代号被 E2E 引用，不动它们。

var pickerPresets = []presetSeed{
	{
		code: "character_face_sheet", name: "角色脸部三视图", group: "sheet", spec: "sheet",
		inputMode: "either", operation: "img2img", sort: 110,
		icon: "scan-face", isNew: false,
		description: "点击生成，直接基于当前图像生成白底脸部三视图；支持文本或参考图生成。",
		prompt:      "构图规范：高质量专业角色脸部设定图，横向构图，纯白色纯净背景，中性摄影棚灯光，平光布光；布局结构（必须是脸部三视图）：正面、左前方 45 度侧面、正左侧面三个头肩特写并列排列，三个视图中间用淡灰色(#E2E2E2)的 2px 细线分割，视图高度一致，五官水平对齐。只出头部与肩部，不出全身；面部清晰，五官结构准确，发型完整可辨识。三个视图中的面部特征、发型、发色、瞳色、肤色与妆容必须完全一致。不得出现任何道具、武器、手持物；不得出现夸张表情与面部遮挡；不得出现环境背景（仅白色）；不得出现其他角色；不得出现文字、水印、标签、logo。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"standard\"}",
	},
	{
		code: "character_setting", name: "角色设定图", group: "sheet", spec: "sheet",
		inputMode: "either", operation: "img2img", sort: 120,
		icon: "user-round", isNew: false,
		description: "点击生成，直接基于当前图像生成完整的角色设定图；支持文本或参考图生成。",
		prompt:      "构图规范：高质量专业角色设定图，横向构图，纯白色纯净背景，中性摄影棚灯光，平光布光；布局结构（必须是角色四视图）：正面面部头部特写（占图片水平 1/3 的空间）+ 全身正面视图、全身左侧面视图、全身背面视图（占图片水平剩余的 2/3 的空间，并列排列），四个视图中间用淡灰色(#E2E2E2)的 2px 细线分割，无任何道具或背景物体。光影：中性摄影棚灯光，柔和的前侧光，清晰的轮廓定义，自然的肤色，面部清晰服装可辨识，平视镜头，完整全身，无裁剪。不得出现任何道具、武器、食物、饮料、手持物（角色必须空手）；不得出现复杂动作、情绪表情、面部遮挡；不得出现环境背景（仅白色）；不得出现其他角色；确保所有视图中的面部特征、发型、体型和服装保持一致；不得出现文字、水印、标签、logo。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"standard\"}",
	},
	{
		code: "role_three_view", name: "角色三视图", group: "sheet", spec: "sheet",
		inputMode: "either", operation: "img2img", sort: 130,
		icon: "square-user-round", isNew: false,
		description: "点击生成，直接基于当前图像生成白底全身三视图；支持文本或参考图生成。",
		prompt:      "构图规范：高质量专业角色三视图，横向构图，纯白色纯净背景，中性摄影棚灯光，平光布光；布局结构（必须是角色三视图）：全身正面视图、全身左侧面视图、全身背面视图三个等高视图并列排列，三个视图中间用淡灰色(#E2E2E2)的 2px 细线分割，人物身高在三个视图中完全一致，头顶与脚底水平对齐。平视镜头，完整全身，无裁剪，双臂自然下垂，站姿放松，双脚与肩同宽。三个视图中的面部特征、发型、体型、服装与配饰必须完全一致。不得出现任何道具、武器、食物、饮料、手持物（角色必须空手）；不得出现复杂动作与情绪表情；不得出现环境背景（仅白色）；不得出现其他角色；不得出现文字、水印、标签、logo。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"standard\"}",
	},
	{
		code: "scene_sheet", name: "场景设定图", group: "sheet", spec: "sheet",
		inputMode: "either", operation: "img2img", sort: 140,
		icon: "layers", isNew: false,
		description: "点击生成，直接基于当前图像生成完整的场景设定图；支持文本或参考图生成。",
		prompt:      "构图规范：高质量专业场景设定图，横向构图，以 2 行 2 列的干净网格四等分整齐排版，每个格子都是独立的 16:9 横向画面，展示同一场景的四个大全景视角（1 为正面中心线大全景视图，镜头正对场景中心轴，构图严格居中，画面同时包含顶面与底面，尽量展示完整空间层次、更多环境细节和深景深；2 以 1 的中心线为参考，摄像机移动到场景左前方 45 度位置的大全景视图，镜头仍对准场景核心区域；3 为以 1 的中心线为参考，摄像机移动到场景右前方 45 度位置的大全景视图；4 为镜头在最深处向外拍摄的正中心全景图）。四个视角也可以是东南西北四个方位的视角。四个视角必须表现同一地点、同一时间、同一天气、同一光源、同一空间结构和同一美术风格。环境清晰，细节丰富，景深较深，光影自然，专业摄影，超清画质。不得出现任何人物（这是空场景参考图，必须空无一人），也不得出现人群、背影、剪影、人脸、手脚、人物倒影、人物影子、照片人物、屏幕人物、镜中人物；不得出现文字、水印、标签、logo。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"standard\"}",
	},
	{
		code: "product_sheet", name: "产品设定图", group: "sheet", spec: "sheet",
		inputMode: "either", operation: "img2img", sort: 150,
		icon: "package", isNew: false,
		description: "点击生成，直接基于当前图像生成完整的产品设定图；支持文本或参考图生成。",
		prompt:      "构图规范：高质量写实道具多角度展示图，横向构图，以 2 行 3 列的干净网格整齐排版，展示道具的六个极正视角。纯白色纯净背景，专业产品影棚摄影，标准六视图参考。六视图包括（必须是道具六视图）：绝对正前方视图、绝对正后方视图、绝对左侧视图、绝对右侧视图、绝对正上方俯拍视图、绝对正下方仰拍视图。所有视图必须是同一件道具，材质、颜色、比例、结构完全一致。使用超长焦镜头或移轴镜头效果，将透视变形降到最低，物体所有本该平行的边缘在画面中保持平行，接近正交投影。每个视图都像在专业产品影棚中用三脚架精密校准拍摄，构图绝对端正，物体在每个格子中居中，无任何倾斜、旋转或透视畸变。画面中不得出现任何人物、角色、人群、人影；不得出现手、脚、人脸、场景、建筑、自然景观；无其他道具；必须无文字、无水印、无 logo、无 UI 元素，不要任何剧情事件，保持道具本体清晰，保持完整轮廓，保持所有角度的材质和结构一致。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"standard\"}",
	},
	{
		code: "panorama_720", name: "720全景", group: "space", spec: "panorama",
		inputMode: "either", operation: "img2img", sort: 90,
		icon: "globe", isNew: false,
		description: "点击生成，直接将场景图像转为全景视角；支持文生图或参考图生成。",
		prompt:      "构图规范：超宽幅全景视角，尽可能采用画幅允许的最宽横向构图，模拟等距柱状投影的观感。以参考图中的场景为基准，向左右两侧完整补全环境，向上补全天空或天花板、向下补全地面，形成一张连续无断裂的宽幅全景；左右两端的景物应当能够自然衔接。场景的建筑结构、材质、光源方向、时间与天气必须与参考图一致，补全部分要符合原场景的空间逻辑，不得凭空加入不相干的地标。不得出现任何人物；不得出现分格、拼图、多画面；不得出现镜头畸变造成的撕裂与黑洞；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"high\"}",
	},
	{
		code: "multicam_9grid", name: "多机位九宫格", group: "space", spec: "variation",
		inputMode: "either", operation: "img2img", sort: 100,
		icon: "grid-3x3", isNew: false,
		description: "点击生成，直接基于当前图生成 9 个不同角度与景别的视角；支持文生图或参考图生成。",
		prompt:      "构图规范：以 3 行 3 列的干净网格整齐排版，共 9 个等大画格，格与格之间用淡灰色(#E2E2E2)的 2px 细线分割。9 个画格必须是同一时刻、同一场景、同一主体的 9 个不同机位与景别，覆盖大全景、全景、中景、近景、特写等不同景别，以及正面、侧面、背面、俯拍、仰拍、过肩等不同机位。时间必须完全冻结：所有画格中人物的姿态、表情、服装、道具位置，以及光源方向、天气与色调必须完全一致，只有观察位置与焦距发生变化。不得出现分格错乱、重复机位、镜像复制；不得改变场景与角色身份；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"high\"}",
	},
	// 3D 导演台的白模截图出图（docs/agent/director，DRT-07，dev/02 §3.1）：上游是导演台的图片节点默认选它
	// （node_param.preset 的 defaultByUpstream）。配方讲「保持构图、出真实画面」；「不采用白模外观」由受理时的
	// 约束段负责（director.reference_guard.capture），两段不重复。
	{
		code: "whitebox_to_real", name: "按白模构图出图", group: "space", spec: "variation",
		inputMode: "image_required", operation: "img2img", sort: 105,
		icon: "box", isNew: true,
		description: "把 3D 导演台的白模截图当构图参考：保持人物的数量、站位、朝向、姿势与机位，按描述生成真实画面。",
		prompt:      "参考图是 3D 白模构图：严格保持参考图里人物的数量、站位、朝向与姿势，以及画面的景别与机位；按描述生成真实的画面。",
		params:      "{}",
		dimensions: `[{"code":"texture","name":"画面质感","default":"realistic","options":[
			{"value":"realistic","label":"写实","prompt":"画面质感：写实摄影，真实的人物、服装、材质与光影。"},
			{"value":"anime","label":"动漫","prompt":"画面质感：动漫风格，干净的线条与平涂上色。"},
			{"value":"render3d","label":"3D 渲染","prompt":"画面质感：高质量 3D 渲染，细腻的材质与柔和的全局光照。"}]}]`,
	},
	{
		code: "blocking_storyboard", name: "调度故事板", group: "storyboard", spec: "sequence",
		inputMode: "text_required", operation: "text2img", sort: 10,
		icon: "library-big", isNew: true,
		description: "描述一个 10-15 秒内能演完的小剧情片段。可补充角色、场景、道具、动作过程、走位、对白或参考图，系统会整理为 4-6 个调度分镜手绘版。",
		prompt:      "构图规范：黑白手绘调度分镜，横向构图，以干净网格整齐排版为 4 到 6 个连续画格，每格为 16:9 横向画面，格与格之间用淡灰色(#E2E2E2)的 2px 细线分割。画面为导演调度草图风格：铅笔线稿或马克笔速写，只保留主体轮廓、场景结构与空间关系，不做完整上色与精细渲染。每格必须标注调度信息：实线箭头表示人物走位方向，虚线箭头表示镜头运动方向（推、拉、摇、移、跟）。时间必须严格向前推进，相邻画格是同一段连续动作的先后关系，不得跳跃到无关场景。所有画格中的角色身份、体型、服装、发型必须完全一致；场景的空间结构与光源方向必须保持连续。不得出现分格错乱、重复画格、镜像复制；不得出现水印、logo、UI 元素。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"high\"}",
	},
	{
		code: "storyboard", name: "故事板", group: "storyboard", spec: "sequence",
		inputMode: "text_required", operation: "text2img", sort: 20,
		icon: "film", isNew: true,
		description: "描述一个 10-15 秒内能演完的小剧情片段，系统会整理为 4-8 个连续分镜，内容过长时会自动提炼关键剧情。提供角色三视图、美术设定图及准确的需求描述，生成效果会更好。",
		prompt:      "构图规范：专业影视故事板，横向构图，以干净网格整齐排版为 4 到 8 个连续画格，每格为 16:9 横向画面，格与格之间用淡灰色(#E2E2E2)的 2px 细线分割。画面为完整分镜画风：线条清晰，明暗关系明确，景别与构图准确，可上色但保持统一美术风格。每格必须体现明确的景别（大全景、全景、中景、近景、特写）与机位高度，相邻画格的景别应有变化，避免连续同景别。时间必须严格向前推进，相邻画格是同一段剧情的先后关系；剧情过长时只保留关键节点，不得丢失因果链。所有画格中的角色身份、体型、服装、发型必须完全一致；场景的空间结构、光源方向与色调必须保持连续。不得出现分格错乱、重复画格、镜像复制；不得出现水印、logo、UI 元素。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"high\"}",
	},
	{
		code: "storyboard_25grid", name: "25宫格连贯分镜", group: "storyboard", spec: "sequence",
		inputMode: "either", operation: "img2img", sort: 30,
		icon: "grid-3x3", isNew: false,
		description: "点击生成，直接基于当前图像进行 25 宫格剧情推演；支持文生图或参考图生成。",
		prompt:      "构图规范：以 5 行 5 列的干净网格整齐排版，共 25 个等大画格，按从左到右、从上到下的顺序阅读，格与格之间用淡灰色(#E2E2E2)的 2px 细线分割。25 个画格必须构成一段完整连贯的剧情推演：以首格画面为起点，时间严格向前推进，每一格相对前一格只发生小幅度变化（动作的下一个瞬间、机位的小幅调整），整体应当像连续抽帧而不是 25 个独立场景。所有画格中的角色身份、面部特征、体型、服装必须完全一致；场景的空间结构、光源方向、色调与美术风格必须保持统一。不得出现分格错乱、重复画格、镜像复制、跳跃到无关场景；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"high\"}",
	},
	{
		code: "storyboard_4grid", name: "剧情推演四宫格", group: "storyboard", spec: "sequence",
		inputMode: "either", operation: "img2img", sort: 40,
		icon: "grid-2x2", isNew: false,
		description: "点击生成，直接基于当前图像进行 4 宫格剧情推演；支持文生图或参考图生成。",
		prompt:      "构图规范：以 2 行 2 列的干净网格四等分整齐排版，共 4 个等大画格，按从左到右、从上到下的顺序阅读，格与格之间用淡灰色(#E2E2E2)的 2px 细线分割。4 个画格必须构成一段完整连贯的剧情推演：以首格画面为起点，时间严格向前推进，四格分别对应同一段动作的起势、发展、高潮与结果，因果关系清晰可读。所有画格中的角色身份、面部特征、体型、服装必须完全一致；场景的空间结构、光源方向、色调与美术风格必须保持统一。不得出现分格错乱、重复画格、镜像复制、跳跃到无关场景；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"16:9\",\"quality\":\"high\"}",
	},
	{
		code: "forward_3s", name: "画面推演 - 3秒后", group: "storyboard", spec: "sequence",
		inputMode: "image_required", operation: "img2img", sort: 50,
		icon: "clock-3", isNew: false,
		description: "基于物理逻辑，推演 3 秒后的动作走向。",
		prompt:      "构图规范：单幅画面，保持与参考图完全一致的画幅、美术风格、色调与光源方向。基于参考图中人物与物体的姿态、朝向、受力与运动趋势，按真实物理规律推演 3 秒之后的那一瞬间：动作自然延续到下一阶段，位移幅度、衣物与头发的惯性、表情变化都要符合 3 秒的时间跨度，既不能几乎没变，也不能跨越到另一个场景。角色身份、面部特征、体型、服装与场景结构必须与参考图完全一致，只有姿态与位置发生变化。不得更换场景、不得增删角色、不得改变服装与道具；不得出现分格、拼图、多画面；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"adaptive\",\"quality\":\"standard\"}",
	},
	{
		code: "backward_5s", name: "画面推演 - 5秒前", group: "storyboard", spec: "sequence",
		inputMode: "image_required", operation: "img2img", sort: 60,
		icon: "clock-5", isNew: false,
		description: "基于物理逻辑，反推 5 秒前的动作起因。",
		prompt:      "构图规范：单幅画面，保持与参考图完全一致的画幅、美术风格、色调与光源方向。把参考图当作结果，按真实物理规律反向回溯 5 秒之前的那一瞬间：还原导致当前姿态的起因动作，位移幅度、衣物与头发的惯性、表情变化都要符合 5 秒的时间跨度，且从该瞬间自然演变到参考图画面必须说得通。角色身份、面部特征、体型、服装与场景结构必须与参考图完全一致，只有姿态与位置发生变化。不得更换场景、不得增删角色、不得改变服装与道具；不得出现分格、拼图、多画面；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"adaptive\",\"quality\":\"standard\"}",
	},
	{
		code: "portrait_tuning", name: "人像质感调节", group: "tuning", spec: "retouch",
		inputMode: "either", operation: "img2img", sort: 70,
		icon: "person-standing", isNew: true,
		description: "上传图片或输入描述，选择质感强度，让人物更自然、更真实。",
		prompt:      "构图规范：只做质感层面的精修，严禁改变画面内容。人物的身份、五官比例、脸型、发型、体型、服装、姿态、表情，以及场景、构图、景别与画幅，必须与参考图完全一致。只允许调整人景融合、光影融合、皮肤、纹理与锐度这五个方面。不得美化五官、不得瘦脸瘦身、不得改变年龄与性别特征、不得增删任何元素；不得出现分格、拼图、多画面；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"adaptive\",\"quality\":\"standard\"}",
		dimensions:  "[{\"code\":\"fusion\",\"name\":\"人景融合\",\"default\":\"natural\",\"options\":[{\"label\":\"轻度对齐\",\"value\":\"light\",\"prompt\":\"人物与背景轻度对齐，仅统一边缘过渡，各自的明暗关系基本保持原样\"},{\"label\":\"自然融合\",\"value\":\"natural\",\"prompt\":\"人物与背景自然融合，统一色温与环境光影响，边缘过渡真实无抠图感\"},{\"label\":\"深度融合\",\"value\":\"deep\",\"prompt\":\"人物与背景深度融合，让环境光与反射充分作用于人物，如同同一次拍摄所得\"}]},{\"code\":\"lighting\",\"name\":\"光影融合\",\"default\":\"match\",\"options\":[{\"label\":\"柔和补光\",\"value\":\"soft\",\"prompt\":\"为人物补柔和的正面光，压低阴影对比，面部通透\"},{\"label\":\"自然匹配\",\"value\":\"match\",\"prompt\":\"使人物的受光方向、强度与色温与场景光源完全匹配\"},{\"label\":\"氛围强化\",\"value\":\"enhance\",\"prompt\":\"强化画面的光影氛围，加强主光与轮廓光的层次，但不改变光源方向\"}]},{\"code\":\"skin\",\"name\":\"皮肤\",\"default\":\"natural\",\"options\":[{\"label\":\"清透修饰\",\"value\":\"clear\",\"prompt\":\"皮肤清透干净，肤色均匀，保留必要的毛孔细节，不要磨皮塑料感\"},{\"label\":\"自然肤质\",\"value\":\"natural\",\"prompt\":\"保持自然肤质，真实的肤色过渡与轻微不均匀，避免过度修饰\"},{\"label\":\"真实肌理\",\"value\":\"real\",\"prompt\":\"强调真实肌理，保留毛孔、绒毛与细微纹路，呈现高解析度皮肤质感\"}]},{\"code\":\"texture\",\"name\":\"纹理\",\"default\":\"natural\",\"options\":[{\"label\":\"柔和纹理\",\"value\":\"soft\",\"prompt\":\"画面纹理柔和干净，噪点极低\"},{\"label\":\"自然纹理\",\"value\":\"natural\",\"prompt\":\"保持自然细腻的画面纹理\"},{\"label\":\"颗粒质感\",\"value\":\"grain\",\"prompt\":\"加入均匀的胶片颗粒质感，颗粒不得破坏五官细节\"}]},{\"code\":\"sharpness\",\"name\":\"锐度\",\"default\":\"standard\",\"options\":[{\"label\":\"柔焦\",\"value\":\"soft\",\"prompt\":\"整体轻微柔焦，焦点仍落在人物面部\"},{\"label\":\"标准清晰\",\"value\":\"standard\",\"prompt\":\"标准清晰度，五官与服装细节清楚，无过度锐化痕迹\"},{\"label\":\"高清锐化\",\"value\":\"high\",\"prompt\":\"高清锐化，强化五官、发丝与织物细节，不得出现白边与光晕\"}]}]",
	},
	{
		code: "cinematic_light", name: "电影级光影校正", group: "tuning", spec: "retouch",
		inputMode: "either", operation: "img2img", sort: 80,
		icon: "aperture", isNew: false,
		description: "点击生成，直接对当前图像进行电影级光影校正；支持文本或参考图生成。",
		prompt:      "构图规范：只做光影与色彩层面的电影感校正，严禁改变画面内容。人物身份、五官、服装、姿态、场景结构、构图、景别与画幅必须与参考图完全一致。按电影摄影机的成像特性重建光影：明确主光方向与强度，补充轮廓光与环境反射光，建立深浅有序的明暗层次，压制高光溢出，保留暗部细节，统一色温并施加电影级调色，同时保持肤色真实。只允许通过光影强化空间纵深感，不得改变构图与景别。不得增删任何元素；不得出现分格、拼图、多画面；不得出现水印、logo、UI 元素、文字标注。",
		params:      "{\"aspect_ratio\":\"adaptive\",\"quality\":\"high\"}",
	},
}
