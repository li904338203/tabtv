package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func img(order int, node string) ResolvedInput {
	return ResolvedInput{Kind: InputGeneration, Order: order, SourceNodeID: node,
		MediaID: int64(order), MediaKind: "image"}
}

func ref(order int, kind string) ResolvedInput {
	return ResolvedInput{Kind: InputReference, Order: order, MediaID: int64(100 + order),
		MediaKind: kind}
}

func TestOrderInputs_按顺序排列(t *testing.T) {
	in := []DeclaredInput{
		{Kind: InputGeneration, Order: 3, SourceNodeID: "c"},
		{Kind: InputGeneration, Order: 1, SourceNodeID: "a"},
		{Kind: InputGeneration, Order: 2, SourceNodeID: "b"},
	}
	out, issues := OrderInputs(in)
	if len(issues) != 0 {
		t.Fatalf("不该有问题: %+v", issues)
	}
	var got []string
	for _, d := range out {
		got = append(got, d.SourceNodeID)
	}
	if strings.Join(got, "") != "abc" {
		t.Fatalf("顺序应为 a,b,c，实际 %v", got)
	}
}

// 重复时无从知道用户想要的顺序，自动排一个出来就是替用户做了一个他看不见的决定 ——
// 首尾帧会反过来。
func TestOrderInputs_顺序冲突报错而不是自动重排(t *testing.T) {
	in := []DeclaredInput{
		{Kind: InputGeneration, Order: 1, SourceNodeID: "a"},
		{Kind: InputGeneration, Order: 1, SourceNodeID: "b"},
	}
	_, issues := OrderInputs(in)
	if len(issues) != 1 || issues[0].Code != IssueOrderConflict {
		t.Fatalf("应报 order 冲突，实际 %+v", issues)
	}
	// 错误要指名道姓，否则用户不知道去改哪个节点。
	if !strings.Contains(issues[0].Message, "a") || !strings.Contains(issues[0].Message, "b") {
		t.Errorf("错误信息应指出是哪两个节点冲突，实际 %q", issues[0].Message)
	}
}

// 素材与音色不参与媒体排序，它们没有"第几个"的概念。
func TestOrderInputs_素材与音色不占位(t *testing.T) {
	in := []DeclaredInput{
		{Kind: InputMaterial, Order: 1, MaterialCode: "s1"},
		{Kind: InputMaterial, Order: 1, MaterialCode: "s2"},
		{Kind: InputVoice, Order: 1, VoiceCode: "v1"},
		{Kind: InputGeneration, Order: 1, SourceNodeID: "a"},
	}
	if _, issues := OrderInputs(in); len(issues) != 0 {
		t.Fatalf("素材/音色同号不该报冲突: %+v", issues)
	}
}

func TestValidateForOperation_媒体数量(t *testing.T) {
	cases := []struct {
		name, op string
		inputs   []ResolvedInput
		wantCode string
		wantText string
	}{
		{"首尾帧两张图", "first_last_frame", []ResolvedInput{img(1, "a"), img(2, "b")}, "", ""},
		{"首尾帧只有一张", "first_last_frame", []ResolvedInput{img(1, "a")},
			IssueCountMismatch, "至少 2 个"},
		{"首尾帧三张", "first_last_frame", []ResolvedInput{img(1, "a"), img(2, "b"), img(3, "c")},
			IssueCountMismatch, "最多接受 2 个"},
		{"图生视频一张", "img2video", []ResolvedInput{img(1, "a")}, "", ""},
		{"图生视频两张", "img2video", []ResolvedInput{img(1, "a"), img(2, "b")},
			IssueCountMismatch, "最多接受 1 个"},
		{"文生视频不接受媒体", "text2video", []ResolvedInput{img(1, "a")},
			IssueCountMismatch, "不接受媒体输入"},
		{"文生视频只有文本", "text2video",
			[]ResolvedInput{{Kind: InputText, Order: 1, Text: "一只猫"}}, "", ""},
		{"全能参考至少一个", "ref2video", nil, IssueCountMismatch, "至少 1 个"},
		{"全能参考图+音频", "ref2video", []ResolvedInput{ref(1, "image"), ref(2, "audio")}, "", ""},
		{"视频生视频要视频", "video2video", []ResolvedInput{ref(1, "video")}, "", ""},
	}
	for _, c := range cases {
		issues := ValidateForOperation(c.op, c.inputs)
		if c.wantCode == "" {
			if len(issues) != 0 {
				t.Errorf("%s 应通过，实际 %+v", c.name, issues)
			}
			continue
		}
		if len(issues) == 0 {
			t.Errorf("%s 应被拒", c.name)
			continue
		}
		if issues[0].Code != c.wantCode || !strings.Contains(issues[0].Message, c.wantText) {
			t.Errorf("%s 期望 %s/%q，实际 %s/%q",
				c.name, c.wantCode, c.wantText, issues[0].Code, issues[0].Message)
		}
	}
}

// 这正是 GAP-22：通用连线规则允许的组合，具体 operation 未必接受。
//
// ⚠️ 原先用「全能参考不收 video」做示范；DRK-12（3D 导演台）起全能参考收参考视频，
// 示范改用一个**仍然**不收视频的操作：图生视频（它要的是一张首帧图）。
func TestValidateForOperation_图生视频不收视频(t *testing.T) {
	issues := ValidateForOperation("img2video", []ResolvedInput{ref(1, "video")})
	if len(issues) == 0 || issues[0].Code != IssueTypeMismatch {
		t.Fatalf("img2video 不该接受 video，实际 %+v", issues)
	}
	if !strings.Contains(issues[0].Message, "第 1 个输入") {
		t.Errorf("应指出是第几个输入，实际 %q", issues[0].Message)
	}

	// 图生视频拿到音频同理。
	if issues := ValidateForOperation("img2video", []ResolvedInput{ref(1, "audio")}); len(issues) == 0 {
		t.Error("img2video 不该接受 audio")
	}
}

// DRK-12：操作层放开，某个模型收不收参考视频交给能力矩阵（报价阶段拦）。
func TestValidateForOperation_全能参考收视频由能力矩阵把关(t *testing.T) {
	if issues := ValidateForOperation("ref2video", []ResolvedInput{ref(1, "image"), ref(2, "video")}); len(issues) != 0 {
		t.Fatalf("全能参考应接受 图 + 参考视频，实际 %+v", issues)
	}
	if issues := ValidateForOperation("ref2video", []ResolvedInput{ref(1, "video")}); len(issues) != 0 {
		t.Fatalf("全能参考应接受只有一段参考视频，实际 %+v", issues)
	}
	// 其他操作的媒体规则不变。
	if issues := ValidateForOperation("first_last_frame", []ResolvedInput{ref(1, "image"), ref(2, "video")}); len(issues) == 0 {
		t.Error("首尾帧仍然只收图片")
	}
}

// 素材只是修饰，不计入媒体数量 —— 否则首尾帧连一个风格就变成"三张图"。
func TestValidateForOperation_素材不计入数量(t *testing.T) {
	inputs := []ResolvedInput{
		img(1, "a"), img(2, "b"),
		{Kind: InputMaterial, MaterialCode: "style_ink"},
		{Kind: InputText, Text: "补充描述"},
	}
	if issues := ValidateForOperation("first_last_frame", inputs); len(issues) != 0 {
		t.Fatalf("素材与文本不该被算作媒体输入: %+v", issues)
	}
}

// 未知 operation 不在这里拦：能力矩阵那层会拒，
// 这里瞎猜一个规则反而会把新加的玩法挡在门外。
func TestValidateForOperation_未知操作放行(t *testing.T) {
	if issues := ValidateForOperation("some_new_op", []ResolvedInput{img(1, "a")}); len(issues) != 0 {
		t.Fatalf("未知操作应放行给能力矩阵判定，实际 %+v", issues)
	}
}

func TestMediaKindFor(t *testing.T) {
	for _, k := range []string{"image", "audio", "video"} {
		if MediaKindFor(k) != k {
			t.Errorf("%s 应原样返回", k)
		}
	}
	if MediaKindFor("text") != "" {
		t.Error("text 不是媒体类型")
	}
}

// ---------------------------------------------------------------- 蒙版

func maskInput(order, w, h int) ResolvedInput {
	return ResolvedInput{
		Kind: InputReference, Order: order, MediaID: int64(order),
		MediaKind: "image", Role: RoleMask, Width: w, Height: h,
	}
}

func srcImage(order, w, h int) ResolvedInput {
	return ResolvedInput{
		Kind: InputReference, Order: order, MediaID: int64(order),
		MediaKind: "image", Width: w, Height: h,
	}
}

// ⚠️ 尺寸对不上的话上游会把蒙版拉伸后贴上去 —— 用户圈的是人脸，
// 改掉的是半张背景，而任务是"成功"的。
func TestValidateForOperation_蒙版尺寸对不上要拒(t *testing.T) {
	issues := ValidateForOperation("img2img", []ResolvedInput{
		srcImage(1, 1024, 768), maskInput(2, 512, 384),
	})
	if len(issues) == 0 {
		t.Fatal("尺寸不一致应被拒绝")
	}
	msg := issues[0].Message
	for _, want := range []string{"1024", "768", "512", "384"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误要说清两边尺寸，实际 %s", msg)
		}
	}
}

func TestValidateForOperation_蒙版同尺寸放行(t *testing.T) {
	issues := ValidateForOperation("img2img", []ResolvedInput{
		srcImage(1, 1024, 768), maskInput(2, 1024, 768),
	})
	for _, i := range issues {
		if strings.Contains(i.Message, "蒙版") {
			t.Fatalf("同尺寸不该报蒙版问题: %s", i.Message)
		}
	}
}

// ⚠️ 拿不到数字就没法比较。拒绝等于把一批本来能用的格式
// （比如 WebP，标准库没有解码器）挡在门外。
func TestValidateForOperation_尺寸未知时放行(t *testing.T) {
	issues := ValidateForOperation("img2img", []ResolvedInput{
		srcImage(1, 0, 0), maskInput(2, 512, 384),
	})
	for _, i := range issues {
		if strings.Contains(i.Message, "蒙版尺寸") {
			t.Fatalf("尺寸未知不该报错: %s", i.Message)
		}
	}
}

// ⚠️ 蒙版说的是"改哪一块"，只有它没有原图时无从下手。
func TestValidateForOperation_只有蒙版没有原图(t *testing.T) {
	issues := ValidateForOperation("img2img", []ResolvedInput{maskInput(1, 512, 512)})
	found := false
	for _, i := range issues {
		if strings.Contains(i.Message, "只有蒙版") {
			found = true
		}
	}
	if !found {
		t.Fatalf("只有蒙版时应说清缺什么: %+v", issues)
	}
}

// ⚠️ 蒙版永远是配角：画什么由 order 最小的那张非蒙版图决定。
func TestValidateForOperation_原图取第一张非蒙版(t *testing.T) {
	issues := ValidateForOperation("ref2video", []ResolvedInput{
		srcImage(1, 800, 600),
		maskInput(2, 800, 600),
		srcImage(3, 1920, 1080), // 另一张参考图，尺寸不同也不该影响蒙版判定
	})
	for _, i := range issues {
		if strings.Contains(i.Message, "蒙版尺寸") {
			t.Fatalf("应按第一张非蒙版图比对: %s", i.Message)
		}
	}
}

// 没有蒙版时这段逻辑完全不介入。
func TestValidateForOperation_没有蒙版时不受影响(t *testing.T) {
	issues := ValidateForOperation("img2img", []ResolvedInput{srcImage(1, 100, 100)})
	for _, i := range issues {
		if strings.Contains(i.Message, "蒙版") {
			t.Fatalf("没有蒙版不该报蒙版问题: %s", i.Message)
		}
	}
}

// ⚠️ 涂抹重绘吃两张图：原图 + 蒙版。写成 {1,1} 的话带蒙版的调用
// 一律报"只接受 1 个媒体输入"，这个工具根本用不了。
func TestValidateForOperation_涂抹重绘接受原图加蒙版(t *testing.T) {
	issues := ValidateForOperation("inpaint", []ResolvedInput{
		srcImage(1, 800, 600), maskInput(2, 800, 600),
	})
	if len(issues) != 0 {
		t.Fatalf("原图 + 蒙版应放行: %+v", issues)
	}
}

func TestValidateForOperation_抠图只吃一张图(t *testing.T) {
	issues := ValidateForOperation("remove_bg", []ResolvedInput{
		srcImage(1, 800, 600), srcImage(2, 800, 600),
	})
	if len(issues) == 0 {
		t.Fatal("抠图只接受 1 张图")
	}
}

// CD-80：分镜图要带上这一镜引用的全部资产图（一镜常有五到八个），图生图在操作这一层不设张数上限 ——
// 写成「最多 1 个」的话每一张分镜图都在受理时被拒。具体一个模型收几张由它的能力矩阵决定。
func TestValidateForOperation_图生图收多张参考图(t *testing.T) {
	inputs := make([]ResolvedInput, 0, 8)
	for i := 1; i <= 8; i++ {
		inputs = append(inputs, srcImage(i, 1024, 1024))
	}
	if issues := ValidateForOperation("img2img", inputs); len(issues) != 0 {
		t.Fatalf("8 张参考图的图生图不该被拒: %+v", issues)
	}
	// 一张都没有仍然要拒（那是文生图）
	if issues := ValidateForOperation("img2img", nil); len(issues) == 0 {
		t.Fatal("没有图的图生图应当被拒")
	}
}

// 输入区间（docs/agent/smartcut SCD-34）：只对音视频、形状对、不越界；时长未知时只校验形状。
func TestCheckRange(t *testing.T) {
	d := func(r []int) DeclaredInput { return DeclaredInput{Order: 2, SourceNodeID: "n1", RangeMs: r} }
	cases := []struct {
		name     string
		in       DeclaredInput
		kind     string
		duration int
		wantMsg  string // 空 = 通过
	}{
		{"没有区间", d(nil), "image", 0, ""},
		{"视频正常", d([]int{2000, 5000}), "video", 60000, ""},
		{"音频到结尾", d([]int{0, 60000}), "audio", 60000, ""},
		{"时长未知只看形状", d([]int{2000, 900000}), "video", 0, ""},
		{"图片", d([]int{0, 1000}), "image", 0, "只对视频或音频有效"},
		{"文本", d([]int{0, 1000}), "", 0, "只对视频或音频有效"},
		{"少一个数", d([]int{1000}), "video", 0, "[起点, 终点]"},
		{"颠倒", d([]int{5000, 2000}), "video", 60000, "起点要小于终点"},
		{"相等", d([]int{2000, 2000}), "video", 60000, "起点要小于终点"},
		{"负数", d([]int{-1, 2000}), "video", 60000, "不能为负"},
		{"太短", d([]int{2000, 2050}), "video", 60000, "至少 100 毫秒"},
		{"越界", d([]int{2000, 61000}), "video", 60000, "超出了素材时长"},
	}
	for _, c := range cases {
		iss := CheckRange(c.in, c.kind, c.duration)
		switch {
		case c.wantMsg == "" && iss != nil:
			t.Errorf("%s：不该拦，却报了 %s", c.name, iss.Message)
		case c.wantMsg != "" && iss == nil:
			t.Errorf("%s：应拦下", c.name)
		case c.wantMsg != "" && (!strings.Contains(iss.Message, c.wantMsg) || iss.Code != IssueRangeInvalid ||
			iss.Order != 2 || iss.NodeID != "n1"):
			t.Errorf("%s：问题要指名第几个输入并说清原因: %+v", c.name, iss)
		}
	}
}

func TestRangeLengthMs_取最长那段(t *testing.T) {
	in := []ResolvedInput{
		{Kind: InputText, Text: "x"},
		{Kind: InputReference, MediaKind: "video", RangeMs: []int{2000, 5000}},
		{Kind: InputGeneration, MediaKind: "audio", RangeMs: []int{0, 4500}},
		{Kind: InputReference, MediaKind: "video"},
	}
	if got := RangeLengthMs(in); got != 4500 {
		t.Fatalf("取最长那段（两段参考出来的仍是一段）：%d", got)
	}
	if got := RangeLengthMs(in[:1]); got != 0 {
		t.Fatalf("没有区间时为 0：%d", got)
	}
}

// 快照形状：没有区间的输入与改动前逐字相同（omitempty），有区间的带上 rangeMs。
func TestResolvedInput_区间进快照(t *testing.T) {
	plain, _ := json.Marshal(ResolvedInput{Kind: InputReference, Order: 1, ReferenceID: 31, MediaID: 31, MediaKind: "video"})
	if strings.Contains(string(plain), "rangeMs") {
		t.Fatalf("没有区间不该多出字段: %s", plain)
	}
	ranged, _ := json.Marshal(ResolvedInput{Kind: InputReference, Order: 1, MediaID: 31, MediaKind: "video", RangeMs: []int{2000, 5000}})
	if !strings.Contains(string(ranged), `"rangeMs":[2000,5000]`) {
		t.Fatalf("区间要进快照: %s", ranged)
	}
}
