package domain

import (
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/provider/protocol"
)

// ⚠️ 这条钉住 GAP-09 的根因：协议名单**只有一处**。
// 目录域若自己再维护一份，漏加一行就是「后端支持但运营选不到」。
func TestProtocols_名单来自适配器注册表(t *testing.T) {
	want := protocol.All()
	got := Protocols()
	if len(got) != len(want) {
		t.Fatalf("协议数量对不上：目录 %d，注册表 %d", len(got), len(want))
	}
	for i, d := range want {
		if got[i].Code != d.Code || got[i].IsAsync != d.Async ||
			got[i].RequiresQueryPath != d.RequiresQueryPath {
			t.Errorf("第 %d 个协议对不上：%+v vs %+v", i, got[i], d)
		}
	}
}

func TestProtocols_每个协议都有中文名(t *testing.T) {
	// 漏写名字只会让下拉显示成协议码（难看但不影响功能），
	// 这条测试是为了别让它悄悄发生。
	for _, p := range Protocols() {
		if p.Name == p.Code {
			t.Errorf("协议 %s 没有中文名，请在 displayNames 里补上", p.Code)
		}
	}
}

// ⚠️ AC-10：协议表达不了的组合要在**配置时**就拒，而不是等 Worker 构造请求。
// 不拦的话，这个配置能存进去、能启用、能报价、能扣费 —— 直到任务跑起来。
func TestCheckProtocolOperation_中转不支持首尾帧(t *testing.T) {
	err := CheckProtocolOperation("bblabu_video", "first_last_frame")
	if err == nil {
		t.Fatal("中转视频接口没有首帧/尾帧的位置，应被拒")
	}
	// 错误里要列出它**支持什么**，否则运营只知道错了不知道该配什么。
	if !strings.Contains(err.Error(), "text2video") {
		t.Fatalf("要列出支持的操作: %v", err)
	}
	if err := CheckProtocolOperation("kling_video", "first_last_frame"); err != nil {
		t.Fatalf("可灵支持首尾帧: %v", err)
	}
	// 未声明能力的协议不拦 —— 通用协议不知道下游挂的是什么。
	if err := CheckProtocolOperation("generic_async", "first_last_frame"); err != nil {
		t.Fatalf("通用协议不该拦: %v", err)
	}
}

func TestCheckProtocolRole_同一操作在不同协议下能力不同(t *testing.T) {
	// 通用 ref2video 连线规则允许 audio，而四家里只有 Grok 收得了（GAP-22）。
	if err := CheckProtocolRole("grok_video", "ref2video", RoleAudio); err != nil {
		t.Fatalf("Grok 支持音频参考: %v", err)
	}
	err := CheckProtocolRole("seedance_video", "ref2video", RoleAudio)
	if err == nil {
		t.Fatal("Seedance 不收音频参考，应被拒")
	}
	if !strings.Contains(err.Error(), "audio") {
		t.Fatalf("要指名是哪种输入: %v", err)
	}
}

func TestCheckProtocolRole_首帧折成图片(t *testing.T) {
	// ⚠️ 矩阵的角色比协议层细：矩阵要能表达「首帧」这个位置，
	// 协议层只关心"收不收得了图片"。折叠不能把首帧判成不支持。
	if err := CheckProtocolRole("kling_video", "first_last_frame", RoleFirstFrame); err != nil {
		t.Fatalf("首帧应折成 image: %v", err)
	}
	if err := CheckProtocolRole("kling_video", "first_last_frame", RoleLastFrame); err != nil {
		t.Fatalf("尾帧应折成 image: %v", err)
	}
	// reference 既可能是图也可能是视频，任一支持即可。
	if err := CheckProtocolRole("bblabu_video", "ref2video", RoleReference); err != nil {
		t.Fatalf("中转的参考输入应被接受: %v", err)
	}
}

func TestCheckPublishable_协议不支持的操作是阻塞项(t *testing.T) {
	blockers := CheckPublishable(PublishInput{
		ProviderReady: true, Protocol: "bblabu_video",
		SubmitPath: "/v1/video", QueryPath: "/v1/video/{taskId}",
		EnabledOps:     []string{"first_last_frame"},
		OpsWithPricing: map[string]bool{"first_last_frame": true},
		OpsWithCaps:    map[string]bool{"first_last_frame": true},
		MemberLevelOK:  true,
	})
	var found bool
	for _, b := range blockers {
		if b.Code == "protocol_operation_unsupported" {
			found = true
		}
	}
	if !found {
		t.Fatalf("绑了协议做不了的操作应当是发布阻塞项: %+v", blockers)
	}
}

func TestCheckProtocolPath_端点变体也要对得上(t *testing.T) {
	// ⚠️ 可灵的 ref2video 只有 omni 端点做得了。普通端点的模型也能绑上它，
	// 于是任务受理成功、扣了费，到 Worker 构造请求时才失败退款 ——
	// 用户看到"生成失败"，而真正的问题是操作绑到了错的端点上（联调矩阵抓到的）。
	if err := CheckProtocolPath("kling_video", "ref2video",
		"/v1/videos/text-to-video/generations"); err == nil {
		t.Fatal("非 omni 端点绑 ref2video 应当是发布阻塞项")
	}
	if err := CheckProtocolPath("kling_video", "ref2video",
		"/v1/videos/omni-video/generations"); err != nil {
		t.Fatalf("omni 端点支持 ref2video: %v", err)
	}
	// 不实现 PathRouter 的协议一律放行。
	if err := CheckProtocolPath("generic_async", "text2video", "/submit"); err != nil {
		t.Fatalf("通用协议不该拦: %v", err)
	}
}

func TestCheckPublishable_端点不支持也是阻塞项(t *testing.T) {
	blockers := CheckPublishable(PublishInput{
		ProviderReady: true, Protocol: "kling_video",
		SubmitPath:     "/v1/videos/text-to-video/generations",
		QueryPath:      "/v1/videos/{taskId}",
		EnabledOps:     []string{"ref2video"},
		OpsWithPricing: map[string]bool{"ref2video": true},
		OpsWithCaps:    map[string]bool{"ref2video": true},
		MemberLevelOK:  true,
	})
	var found bool
	for _, b := range blockers {
		if b.Code == "protocol_path_unsupported" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应当报端点不支持: %+v", blockers)
	}
}

// 04 文档 P-61：报价 / 受理在扣费前按协议范围校验视频参数。
func TestCheckVideoParams_扣费前按协议范围拦截(t *testing.T) {
	if err := CheckVideoParams("uniart_video", "img2video", 3, nil); err == nil {
		t.Fatal("UniArt 最短 4 秒：3 秒应在报价时就拒")
	}
	if err := CheckVideoParams("uniart_video", "img2video", 0, map[string]any{"duration": 2}); err == nil {
		t.Fatal("参数里的时长同样要查")
	}
	if err := CheckVideoParams("kling_video", "text2video", 10, map[string]any{"resolution": "480p"}); err == nil {
		t.Fatal("可灵没有 480p")
	}
	if err := CheckVideoParams("kling_video", "img2video", 10, map[string]any{"aspect_ratio": "4:3"}); err != nil {
		t.Fatalf("可灵图生视频不带画幅，不该因画幅拦下：%v", err)
	}
	if err := CheckVideoParams("generic_async", "text2video", 600, nil); err != nil {
		t.Fatalf("中转协议不知道下游范围，放行：%v", err)
	}
	if err := CheckVideoParams("seedance_video", "text2video", 10, map[string]any{"resolution": "1080p",
		"aspect_ratio": "9:16"}); err != nil {
		t.Fatalf("合法参数应放行：%v", err)
	}
}

// 会话 107398945801502720：UniArt 不带画幅 → 上游 auto → seedance-2.5 不支持（400），那时已经扣费。
// 受理（带参数）在扣费前要求画幅；规划报价 / 前端估价不带参数，照常放行。
func TestCheckVideoParams_UniArt受理要求画幅(t *testing.T) {
	if err := CheckVideoParams("uniart_video", "ref2video", 15, nil); err != nil {
		t.Fatalf("没有参数的报价（规划、估价）不查画幅：%v", err)
	}
	err := CheckVideoParams("uniart_video", "ref2video", 15, map[string]any{"model": "seedance_2_5", "duration": "15"})
	if err == nil || !strings.Contains(err.Error(), "需要指定画幅") {
		t.Fatalf("受理参数里没有画幅应在扣费前拒绝：%v", err)
	}
	if err := CheckVideoParams("uniart_video", "ref2video", 15, map[string]any{"duration": "15",
		"aspect_ratio": "9:16"}); err != nil {
		t.Fatalf("给了画幅应放行：%v", err)
	}
}
