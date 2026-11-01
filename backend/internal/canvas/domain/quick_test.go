package domain

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOperationFor_按模式与图片数选操作码(t *testing.T) {
	cases := []struct {
		name     string
		mode     QuickMode
		images   int
		explicit string
		want     string
	}{
		{"视频无图", QuickVideo, 0, "", "text2video"},
		{"视频一图", QuickVideo, 1, "", "img2video"},
		{"视频两图默认首尾帧", QuickVideo, 2, "", "first_last_frame"},
		{"视频三图全参考", QuickVideo, 3, "", "ref2video"},
		{"视频两图选全参考", QuickVideo, 2, "ref2video", "ref2video"},
		{"视频一图选全参考", QuickVideo, 1, "ref2video", "ref2video"},
		{"视频显式文生", QuickVideo, 0, "text2video", "text2video"},
		{"视频显式图生", QuickVideo, 1, "img2video", "img2video"},
		{"图片无图", QuickImage, 0, "", "text2img"},
		{"图片有图", QuickImage, 2, "", "img2img"},
		{"图片显式一致", QuickImage, 1, "img2img", "img2img"},
		{"音频", QuickAudio, 0, "", "text2audio"},
		{"音频显式", QuickAudio, 0, "text2audio", "text2audio"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := OperationFor(c.mode, c.images, c.explicit)
			if err != nil || got != c.want {
				t.Fatalf("OperationFor(%s,%d,%q) = %q, %v；期望 %q", c.mode, c.images, c.explicit, got, err, c.want)
			}
		})
	}

	bad := []struct {
		name     string
		mode     QuickMode
		images   int
		explicit string
	}{
		{"文生视频带图", QuickVideo, 1, "text2video"},
		{"图生视频没图", QuickVideo, 0, "img2video"},
		{"全参考没图", QuickVideo, 0, "ref2video"},
		{"视频未知操作", QuickVideo, 0, "video2video"},
		{"图片操作与图数不符", QuickImage, 0, "img2img"},
		{"音频带图", QuickAudio, 1, ""},
		{"音频错操作", QuickAudio, 0, "text2img"},
		{"未知模式", QuickMode("agent"), 0, ""},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := OperationFor(c.mode, c.images, c.explicit); !errors.Is(err, ErrQuickMode) {
				t.Fatalf("应拒绝，得到 %v", err)
			}
		})
	}
}

func TestOperationFor_首尾帧要恰好两张图(t *testing.T) {
	for _, n := range []int{0, 1, 3, 6} {
		_, err := OperationFor(QuickVideo, n, "first_last_frame")
		if !errors.Is(err, ErrQuickMode) {
			t.Fatalf("%d 张图选首尾帧应拒绝，得到 %v", n, err)
		}
		// ⚠️ 错误要说清当前几张：一句「输入不符」用户不知道该加还是该删。
		if err != nil && !strings.Contains(err.Error(), "恰好 2 张") {
			t.Fatalf("错误应说明要恰好 2 张：%v", err)
		}
	}
	if got, err := OperationFor(QuickVideo, 2, "first_last_frame"); err != nil || got != "first_last_frame" {
		t.Fatalf("两张图选首尾帧应通过：%q %v", got, err)
	}
}

// TestBuildQuickNode_形状与前端document一致 对照 testdata 里的前端存储节点
// （`frontend/utils/quickGenerate.test.ts` 读同一份文件，验证它在前端往返不变）。
func TestBuildQuickNode_形状与前端document一致(t *testing.T) {
	raw, err := os.ReadFile("testdata/quick-node.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(want["params"])
	got, err := BuildQuickNode("n_kz1x2y3_1", QuickVideo, QuickNodeTitle(QuickVideo), params)
	if err != nil {
		t.Fatal(err)
	}
	var gotMap map[string]any
	if err := json.Unmarshal(got, &gotMap); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotMap, want) {
		t.Fatalf("节点形状与前端不一致：\n得到 %s\n期望 %s", got, raw)
	}
	// ⚠️ 不能是 Vue Flow 运行时形状
	if _, ok := gotMap["data"]; ok {
		t.Fatal("持久化节点不该有 data 键")
	}

	// 图片/音频宽 330
	img, _ := BuildQuickNode("n_1", QuickImage, "图片节点 1", nil)
	var m struct {
		Size   struct{ W, H int } `json:"size"`
		Params map[string]any     `json:"params"`
	}
	_ = json.Unmarshal(img, &m)
	if m.Size.W != 330 || m.Size.H != 180 || m.Params == nil {
		t.Fatalf("图片节点尺寸/参数不对：%s", img)
	}
	if _, err := BuildQuickNode("n_1", QuickImage, "x", json.RawMessage(`[1]`)); err == nil {
		t.Fatal("params 不是对象应拒绝")
	}

	doc := QuickCanvas(got)
	if err := ValidateCanvas(doc); err != nil {
		t.Fatalf("包出来的画布应能通过画布校验：%v", err)
	}
}

func TestQuickProjectName(t *testing.T) {
	cases := map[string]string{
		"":       "未命名创作",
		"   \n ": "未命名创作",
		"三打白骨精，悟空视角，悲壮一点":          "三打白骨精，悟空视角，悲壮一点",
		"第一行\n\n第二行":               "第一行 第二行",
		"一二三四五六七八九十一二三四五六七八九十多出来的": "一二三四五六七八九十一二三四五六七八九十",
	}
	for in, want := range cases {
		if got := QuickProjectName(in); got != want {
			t.Fatalf("QuickProjectName(%q) = %q，期望 %q", in, got, want)
		}
	}
	if _, err := ValidateProjectName(QuickProjectName(strings.Repeat("长", 500))); err != nil {
		t.Fatalf("截出来的名字应能通过项目名校验：%v", err)
	}
}

func TestQuickQuantities(t *testing.T) {
	c, d := QuickQuantities(QuickVideo, json.RawMessage(`{"count":"2","duration":"10"}`))
	if c != 2 || d != 10 {
		t.Fatalf("得到 %d %d", c, d)
	}
	c, d = QuickQuantities(QuickImage, json.RawMessage(`{"count":4,"duration":"10"}`))
	if c != 4 || d != 0 {
		t.Fatalf("图片不该带时长：%d %d", c, d)
	}
	c, d = QuickQuantities(QuickAudio, nil)
	if c != 1 || d != 0 {
		t.Fatalf("缺省应为 1/0：%d %d", c, d)
	}
	if c, _ = QuickQuantities(QuickImage, json.RawMessage(`{"count":"abc"}`)); c != 1 {
		t.Fatalf("非法数量应回落 1：%d", c)
	}
}

func TestParseQuickMode(t *testing.T) {
	for _, s := range []string{"video", "image", "audio"} {
		if _, err := ParseQuickMode(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"", "agent", "text"} {
		if _, err := ParseQuickMode(s); err == nil {
			t.Fatalf("%q 应拒绝", s)
		}
	}
}
