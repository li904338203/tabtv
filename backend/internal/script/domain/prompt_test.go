package domain

import (
	"strings"
	"testing"
)

func baseUpstream() Upstream {
	return Upstream{
		GlobalStyle: "写实电影感", Description: "@林澈 站在天台边缘", DurationSec: 6,
		StartState: "背对镜头", EndState: "转头看镜头", ShotSize: "全景",
		Lighting: "黄昏逆光", CameraMove: "缓慢推近",
		Assets: []PromptAsset{
			{Name: "林澈", Type: AssetCharacter, Description: "三十岁男性，短发\n构图规范：四视图"},
			{Name: "天台", Type: AssetScene, Description: "老旧楼顶\n构图规范：四视角"},
		},
	}
}

// ---------------------------------------------------------------- 指纹

func TestFingerprint_上游任一项变了指纹就变(t *testing.T) {
	// ⚠️ 少算一项就会漏掉一类"上游变了"。最容易漏的是**资产描述** ——
	// 用户改完角色外观、提示词却还显示 ready，他拿着一段过时的词去出图，
	// 画面里那个角色还是旧的样子，而界面上一切正常。
	base := baseUpstream().Fingerprint()
	cases := []struct {
		name string
		mut  func(*Upstream)
	}{
		{"全片风格", func(u *Upstream) { u.GlobalStyle = "动画风" }},
		{"镜头描述", func(u *Upstream) { u.Description = "换了个画面" }},
		{"起始状态", func(u *Upstream) { u.StartState = "正对镜头" }},
		{"结束状态", func(u *Upstream) { u.EndState = "走出画面" }},
		{"景别", func(u *Upstream) { u.ShotSize = "特写" }},
		{"光线", func(u *Upstream) { u.Lighting = "顶光" }},
		{"运镜", func(u *Upstream) { u.CameraMove = "固定" }},
		{"对白", func(u *Upstream) { u.Dialogue = "你好" }},
		{"音效", func(u *Upstream) { u.SoundEffect = "风声" }},
		{"时长", func(u *Upstream) { u.DurationSec = 8 }},
		{"资产描述", func(u *Upstream) { u.Assets[0].Description = "四十岁男性，长发" }},
		{"资产名", func(u *Upstream) { u.Assets[0].Name = "别人" }},
		{"资产类型", func(u *Upstream) { u.Assets[0].Type = AssetProp }},
		{"多一个资产", func(u *Upstream) {
			u.Assets = append(u.Assets, PromptAsset{Name: "怀表", Type: AssetProp})
		}},
	}
	for _, c := range cases {
		u := baseUpstream()
		c.mut(&u)
		if u.Fingerprint() == base {
			t.Errorf("%s 变了，指纹却没变 —— 这类改动会静默地不触发 stale", c.name)
		}
	}
}

func TestFingerprint_资产顺序变了指纹也要变(t *testing.T) {
	// ⚠️ 顺序即参考图的锚定顺序：顺序变了，提示词里的编号就对不上了。
	u := baseUpstream()
	swapped := baseUpstream()
	swapped.Assets[0], swapped.Assets[1] = swapped.Assets[1], swapped.Assets[0]
	if u.Fingerprint() == swapped.Fingerprint() {
		t.Fatal("资产顺序变了指纹必须变，否则编号错位不会被发现")
	}
}

func TestFingerprint_同样的输入永远同样的指纹(t *testing.T) {
	// ⚠️ 用有序拼接而不是 map：map 的遍历顺序每次都不同，
	// 指纹会随机变化，于是所有提示词永远是 stale。
	for i := 0; i < 20; i++ {
		if baseUpstream().Fingerprint() != baseUpstream().Fingerprint() {
			t.Fatal("同样的上游必须产出同样的指纹")
		}
	}
	if n := len(baseUpstream().Fingerprint()); n != 16 {
		t.Fatalf("指纹要短（列宽 varchar(16)）: %d", n)
	}
}

// ---------------------------------------------------------------- 有效状态

func TestEffectiveStatus_指纹不一致就是stale(t *testing.T) {
	if got := EffectiveStatus(PromptReady, "aaa", "bbb"); got != PromptStale {
		t.Fatalf("上游变了应为 stale，实际 %s", got)
	}
	if got := EffectiveStatus(PromptReady, "aaa", "aaa"); got != PromptReady {
		t.Fatalf("上游没变应为 ready，实际 %s", got)
	}
	// ⚠️ 库里存的 stale + 指纹又对上了（用户把改动改回去了）→ 回到 ready。
	// 不回的话用户得白白重算一次。
	if got := EffectiveStatus(PromptStale, "aaa", "aaa"); got != PromptReady {
		t.Fatalf("改回去了应回到 ready，实际 %s", got)
	}
}

func TestEffectiveStatus_pending与failed不受上游影响(t *testing.T) {
	// 从没合成过 / 上次失败了：与上游变没变无关。
	if got := EffectiveStatus(PromptPending, "", "bbb"); got != PromptPending {
		t.Fatalf("pending 应保持: %s", got)
	}
	if got := EffectiveStatus(PromptFailed, "aaa", "bbb"); got != PromptFailed {
		t.Fatalf("failed 应保持: %s", got)
	}
}

func TestEffectiveStatus_没有指纹时按存的算(t *testing.T) {
	// 旧数据或手写但没记指纹的：无从比较，不能一律判 stale
	// （那会让所有历史提示词一夜之间全变灰）。
	if got := EffectiveStatus(PromptReady, "", "bbb"); got != PromptReady {
		t.Fatalf("无指纹时应按存的算: %s", got)
	}
}

// ---------------------------------------------------------------- 模板组装

func TestComposeTemplate_参考图按顺序编号(t *testing.T) {
	// ⚠️ 编号与 `shot_asset_ref.sort` 一一对应。错位的话模型会把 A 的特征
	// 画到 B 身上，而画面看上去"挺正常" —— 这种错最难被发现。
	got := ComposeTemplate(PromptImage, baseUpstream())
	i1 := strings.Index(got, "图1 = 林澈")
	i2 := strings.Index(got, "图2 = 天台")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("参考图应按顺序编号: %s", got)
	}
	if i1 > i2 {
		t.Fatal("编号顺序错了")
	}
}

func TestComposeTemplate_只取资产描述的第一行(t *testing.T) {
	// ⚠️ 资产描述后面跟着构图规范（四视图拼版），那是给**资产出图**用的。
	// 塞进镜头提示词只会让模型去画一张多视图拼版。
	got := ComposeTemplate(PromptImage, baseUpstream())
	if strings.Contains(got, "构图规范：四视图") {
		t.Fatalf("不该把资产的版式规范带进镜头提示词: %s", got)
	}
	if !strings.Contains(got, "三十岁男性，短发") {
		t.Fatalf("应带上资产的外观描述: %s", got)
	}
}

func TestComposeTemplate_识别信息排在审美词之前(t *testing.T) {
	// 反过来的话模型会先照着风格画，把用户真正要的内容挤掉。
	got := ComposeTemplate(PromptImage, baseUpstream())
	iAssets := strings.Index(got, "参考图对应关系")
	iContent := strings.Index(got, "画面内容")
	iSpec := strings.Index(got, "参考图使用规则")
	if !(iAssets < iContent && iContent < iSpec) {
		t.Fatalf("顺序应为 参考图关系 → 画面内容 → 使用规则:\n%s", got)
	}
}

func TestComposeTemplate_图片与视频要的东西不同(t *testing.T) {
	img := ComposeTemplate(PromptImage, baseUpstream())
	vid := ComposeTemplate(PromptVideo, baseUpstream())
	// 图片取起始状态那一帧（分镜图是这一镜的第一画面）。
	if !strings.Contains(img, "定格于") {
		t.Fatalf("图片提示词应定格在起始状态: %s", img)
	}
	if strings.Contains(img, "运动边界") {
		t.Fatalf("图片不该带运动边界: %s", img)
	}
	// 视频要运动边界与时长。
	if !strings.Contains(vid, "运动边界") || !strings.Contains(vid, "6 秒") {
		t.Fatalf("视频提示词应带运动边界与时长: %s", vid)
	}
	// 两套禁止项不同：图片禁分屏，视频禁转场。
	if !strings.Contains(img, "分屏") || !strings.Contains(vid, "转场") {
		t.Fatal("两种提示词的禁止项应当不同")
	}
}

func TestComposeTemplate_没有资产也能出词(t *testing.T) {
	u := Upstream{Description: "空镜：雨打在窗上", DurationSec: 4}
	got := ComposeTemplate(PromptVideo, u)
	if !strings.Contains(got, "雨打在窗上") {
		t.Fatalf("纯空镜也要能合成: %s", got)
	}
	if strings.Contains(got, "参考图对应关系") {
		t.Fatalf("没有资产时不该出现空的对应关系段: %s", got)
	}
}

// ---------------------------------------------------------------- 批量报价

func TestPlanCompose_模板模式零积分(t *testing.T) {
	// ⚠️ 模板完全由代码拼，没有调用上游，收费没有任何依据（AV-15）。
	items := []ComposeItem{{ShotID: 1, Status: PromptPending}, {ShotID: 2, Status: PromptPending}}
	_, quote := PlanCompose(items, SourcePromptTemplate, 6, false)
	if quote.Total != 0 {
		t.Fatalf("模板组装应为 0 积分，实际 %d", quote.Total)
	}
	if quote.TemplateCount != 2 || quote.AICount != 0 {
		t.Fatalf("计数不对: %+v", quote)
	}
}

func TestPlanCompose_AI模式按单价乘条数(t *testing.T) {
	// ⚠️ GAP-21：老系统前端用「选中数 × 6」这种写死的估算，后端按定价目录算，
	// 两边对不上 —— 用户看到的价和实际扣的钱不是一回事。
	// 单价必须由服务端报价给出，这里只负责乘。
	items := []ComposeItem{
		{ShotID: 1, Status: PromptPending}, {ShotID: 2, Status: PromptStale},
		{ShotID: 3, Status: PromptFailed},
	}
	_, quote := PlanCompose(items, SourcePromptAI, 4, false)
	if quote.Total != 12 {
		t.Fatalf("3 条 × 4 应为 12，实际 %d", quote.Total)
	}
}

func TestPlanCompose_已就绪的跳过且不计费(t *testing.T) {
	// 重算要再花一次钱，而用户点"批量合成"的意思是"把没有的和过期的补上"。
	items := []ComposeItem{
		{ShotID: 1, Status: PromptReady}, {ShotID: 2, Status: PromptStale},
	}
	planned, quote := PlanCompose(items, SourcePromptAI, 5, false)
	if !planned[0].Skip || !strings.Contains(planned[0].Reason, "跳过") {
		t.Fatalf("已就绪应跳过: %+v", planned[0])
	}
	if planned[1].Skip {
		t.Fatalf("stale 的应当重算: %+v", planned[1])
	}
	if quote.SkipCount != 1 || quote.Total != 5 {
		t.Fatalf("跳过的不该计费: %+v", quote)
	}
}

func TestPlanCompose_force时不跳过(t *testing.T) {
	// 用户显式点了"重新生成"。
	items := []ComposeItem{{ShotID: 1, Status: PromptReady}}
	planned, quote := PlanCompose(items, SourcePromptAI, 5, true)
	if planned[0].Skip {
		t.Fatal("显式重算不该跳过")
	}
	if quote.Total != 5 {
		t.Fatalf("重算要计费: %+v", quote)
	}
}

func TestSortComposeItems_按镜号与种类稳定(t *testing.T) {
	// 不排的话每次返回顺序都不同，前端的进度列表会乱跳。
	items := []ComposeItem{
		{ShotNo: 2, Kind: PromptVideo}, {ShotNo: 1, Kind: PromptVideo},
		{ShotNo: 2, Kind: PromptImage}, {ShotNo: 1, Kind: PromptImage},
	}
	SortComposeItems(items)
	want := []struct {
		no   int
		kind PromptKind
	}{{1, PromptImage}, {1, PromptVideo}, {2, PromptImage}, {2, PromptVideo}}
	for i, w := range want {
		if items[i].ShotNo != w.no || items[i].Kind != w.kind {
			t.Fatalf("排序不对: %+v", items)
		}
	}
}

func TestMaxComposeConcurrency(t *testing.T) {
	// ⚠️ 上限是给上游留余地的：同步 LLM 一次并发太多会撞账号级限流，
	// 换回来一批 429 —— 而那时每一条都已经扣过费了。
	if MaxComposeConcurrency != 5 {
		t.Fatalf("AV-17 要求最多 5 个并发，实际 %d", MaxComposeConcurrency)
	}
}
