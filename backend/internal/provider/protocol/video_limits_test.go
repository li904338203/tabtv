package protocol

import (
	"strings"
	"testing"
)

// 04 文档 P-61：报价用的范围与 BuildSubmit 必须是同一份 —— 两边对不上的表现是
// 「报价说能做、Worker 里失败退款」或者反过来「报价拦下了本来能做的」。
func TestVideoLimits_与构造请求的判断一致(t *testing.T) {
	codes := []string{"grok_video", "kling_video", "seedance_video"}
	for _, code := range codes {
		lim, ok := VideoLimitsOf(code, "text2video")
		if !ok {
			t.Fatalf("%s 应声明文生视频的参数范围", code)
		}
		if lim.MinSeconds <= 0 || lim.MaxSeconds < lim.MinSeconds || len(lim.Resolutions) == 0 {
			t.Fatalf("%s 的范围不完整：%+v", code, lim)
		}
		for _, sec := range []int{lim.MinSeconds - 1, lim.MaxSeconds + 1} {
			if err := lim.CheckDuration(sec); err == nil {
				t.Errorf("%s：%d 秒应被报价拒绝", code, sec)
			}
			if msg := buildErr(t, code, videoJob("text2video", map[string]any{"duration": sec})); !strings.Contains(msg, "时长") {
				t.Errorf("%s：%d 秒构造请求应同样拒绝，实际 %s", code, sec, msg)
			}
		}
		for _, sec := range []int{lim.MinSeconds, lim.MaxSeconds} {
			if err := lim.CheckDuration(sec); err != nil {
				t.Errorf("%s：%d 秒应放行：%v", code, sec, err)
			}
			buildBody(t, code, videoJob("text2video", map[string]any{"duration": sec}))
		}
		bad := map[string]any{"resolution": "8k"}
		if err := lim.CheckParams(bad); err == nil || !strings.Contains(buildErr(t, code, videoJob("text2video", bad)), "分辨率") {
			t.Errorf("%s：8k 应在报价与构造请求两边都被拒：%v", code, err)
		}
		// 画幅给一个各家都认的。
		if err := lim.CheckParams(map[string]any{"resolution": lim.Resolutions[0], "aspect_ratio": "16:9"}); err != nil {
			t.Errorf("%s：声明里的清晰度应放行：%v", code, err)
		}
	}
	// 缺省时长按 5 秒算（与计价一致），各家都在范围内。
	for _, code := range codes {
		lim, _ := VideoLimitsOf(code, "text2video")
		if err := lim.CheckDuration(0); err != nil {
			t.Errorf("%s：缺省 5 秒应放行：%v", code, err)
		}
	}
}

func TestVideoLimits_未声明的协议与操作不检查(t *testing.T) {
	if _, ok := VideoLimitsOf("generic_async", "text2video"); ok {
		t.Fatal("中转协议不知道下游的范围，不该声明")
	}
	if _, ok := VideoLimitsOf("grok_video", "first_last_frame"); ok {
		t.Fatal("协议不支持的操作不声明范围")
	}
	if _, ok := VideoLimitsOf("not_a_protocol", "text2video"); ok {
		t.Fatal("未注册的协议不声明范围")
	}
}

func TestVideoLimits_说明文字(t *testing.T) {
	lim, _ := VideoLimitsOf("seedance_video", "img2video")
	got := lim.Describe()
	if !strings.Contains(got, "单次时长 3~30 秒") || !strings.Contains(got, "1080p") || !strings.Contains(got, "9:16") ||
		strings.Contains(got, "adaptive") {
		t.Fatalf("说明文字不对：%s", got)
	}
}

func TestVideoLimits_参数里的时长同样检查(t *testing.T) {
	lim, _ := VideoLimitsOf("kling_video", "text2video")
	if err := lim.CheckParams(map[string]any{"duration": "2"}); err == nil {
		t.Fatal("参数里写了 2 秒（最短 3 秒）也要拦：Worker 读的是参数里的时长")
	}
	if err := lim.CheckParams(map[string]any{"duration": 10}); err != nil {
		t.Fatalf("10 秒应放行：%v", err)
	}
}
