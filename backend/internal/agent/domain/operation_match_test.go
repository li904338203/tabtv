package domain

import (
	"strings"
	"testing"
)

// seedanceFits 照本地目录里 seedance_2_5 的实际配置（04 文档里的失败案例用的就是它）。
func seedanceFits() []OperationFit {
	return []OperationFit{
		{Code: "text2video", Output: "video", Roles: map[string]RoleRange{"text": {0, 8}}},
		{Code: "img2video", Output: "video", Roles: map[string]RoleRange{"image": {1, 20}, "text": {0, 8}},
			Limits: "单次时长 4~30 秒"},
		{Code: "ref2video", Output: "video", Roles: map[string]RoleRange{"image": {0, 30}, "video": {0, 10},
			"audio": {0, 10}, "text": {0, 8}}},
		{Code: "first_last_frame", Output: "video", Roles: map[string]RoleRange{"image": {2, 2}, "text": {0, 8}}},
		{Code: "video2video", Output: "video", Roles: map[string]RoleRange{"video": {1, 1}, "text": {0, 8}}},
	}
}

func TestMatchOperation_带图的文生视频换成收得下图的方式(t *testing.T) {
	cases := []struct {
		name         string
		current      string
		counts       map[string]int
		want         string
		wantSwitched bool
	}{
		{"没写操作 + 1 张图 → 图生视频", "", map[string]int{"image": 1}, "img2video", false},
		{"写了文生视频 + 1 张商品图 → 换成图生视频", "text2video", map[string]int{"image": 1}, "img2video", true},
		{"3 张图 → 参考生视频", "text2video", map[string]int{"image": 3}, "ref2video", true},
		{"2 张图默认按参考图用", "", map[string]int{"image": 2}, "ref2video", false},
		{"写了首尾帧且收得下就不换", "first_last_frame", map[string]int{"image": 2}, "first_last_frame", false},
		{"源视频 → 视频生视频", "", map[string]int{"video": 1}, "video2video", false},
		{"没有媒体 → 文生视频", "", nil, "text2video", false},
		{"写了图生视频但没有图 → 文生视频", "img2video", nil, "text2video", true},
	}
	for _, c := range cases {
		got, switched, ok := MatchOperation("video", c.current, c.counts, seedanceFits())
		if !ok || got != c.want || switched != c.wantSwitched {
			t.Errorf("%s：得到 %s switched=%v ok=%v", c.name, got, switched, ok)
		}
	}
}

func TestMatchOperation_模型不支持就不换并报不匹配(t *testing.T) {
	textOnly := []OperationFit{{Code: "text2video", Output: "video", Roles: map[string]RoleRange{"text": {0, 8}}}}
	got, switched, ok := MatchOperation("video", "text2video", map[string]int{"image": 1}, textOnly)
	if ok || switched || got != "text2video" {
		t.Fatalf("只有文生视频的模型收不下图：%s %v %v", got, switched, ok)
	}
	// 可灵 3.0 没有参考生视频：两张图退到首尾帧。
	kling := []OperationFit{
		{Code: "text2video", Output: "video", Roles: map[string]RoleRange{"text": {0, 8}}},
		{Code: "img2video", Output: "video", Roles: map[string]RoleRange{"image": {1, 1}}},
		{Code: "first_last_frame", Output: "video", Roles: map[string]RoleRange{"image": {2, 2}}},
	}
	if got, _, ok := MatchOperation("video", "", map[string]int{"image": 2}, kling); !ok || got != "first_last_frame" {
		t.Fatalf("没有参考生视频时两张图走首尾帧：%s %v", got, ok)
	}
	// 没配能力矩阵的生成方式不选（报价会拦）。
	noCaps := []OperationFit{{Code: "img2video", Output: "video"}}
	if _, _, ok := MatchOperation("video", "", map[string]int{"image": 1}, noCaps); ok {
		t.Fatal("没配能力矩阵的不选")
	}
}

func TestMatchOperation_图片产出(t *testing.T) {
	fits := []OperationFit{
		{Code: "text2img", Output: "image", Roles: map[string]RoleRange{"text": {0, 8}}},
		{Code: "img2img", Output: "image", Roles: map[string]RoleRange{"image": {1, 6}, "text": {0, 8}}},
		{Code: "text2video", Output: "video", Roles: map[string]RoleRange{"text": {0, 8}}},
	}
	if got, switched, ok := MatchOperation("image", "text2img", map[string]int{"image": 2}, fits); !ok || got != "img2img" || !switched {
		t.Fatalf("带参考图的文生图换成图生图：%s %v %v", got, switched, ok)
	}
	if got, _, _ := MatchOperation("image", "", nil, fits); got != "text2img" {
		t.Fatalf("没有图就是文生图：%s", got)
	}
}

func TestDescribeFits_列出能用的方式与输入(t *testing.T) {
	got := DescribeFits("video", seedanceFits())
	for _, want := range []string{"- text2video：只收文字", "- img2video：图片 1~20 个；单次时长 4~30 秒",
		"- first_last_frame：图片恰好 2 个", "- video2video：视频恰好 1 个"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q：\n%s", want, got)
		}
	}
}
