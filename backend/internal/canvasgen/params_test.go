package canvasgen

import (
	"encoding/json"
	"reflect"
	"testing"
)

// 自 agent/tools/generate_test.go 原样搬入（MCPT-08，用例表不改）。

// ---------------------------------------------------------------- 参数归一（纯函数）

func TestNormalizeGenerateParams_画布编码(t *testing.T) {
	videoCodes := []string{"video_mode", "prompt", "model", "duration", "resolution", "aspect_ratio", "effect_ref", "seed"}
	cases := []struct {
		name               string
		nodeType, op       string
		params             map[string]any
		codes              []string
		dur, count         int
		want               map[string]any
		wantDur, wantCount int
	}{
		{
			name: "驼峰画幅改下划线 + durationSec 写进 duration（字符串）", nodeType: "video", op: "text2video",
			params: map[string]any{"model": "sd", "aspectRatio": "9:16", "resolution": "720p"},
			codes:  videoCodes, dur: 15,
			want:    map[string]any{"model": "sd", "aspect_ratio": "9:16", "resolution": "720p", "duration": "15", "mode": "text2video"},
			wantDur: 15,
		},
		{
			name: "画布编码已给时以它为准、丢掉别名", nodeType: "video", op: "img2video",
			params: map[string]any{"aspect_ratio": "16:9", "aspectRatio": "9:16", "ratio": "1:1"},
			codes:  videoCodes,
			want:   map[string]any{"aspect_ratio": "16:9", "mode": "img2video"},
		},
		{
			name: "只给 params.duration 时反补 durationSec（计价与上游一致）", nodeType: "video", op: "text2video",
			params: map[string]any{"duration": json.Number("10")},
			codes:  videoCodes,
			want:   map[string]any{"duration": "10", "mode": "text2video"}, wantDur: 10,
		},
		{
			name: "两者都给以 durationSec 为准", nodeType: "video", op: "text2video",
			params: map[string]any{"durationSec": json.Number("5"), "duration": "10"},
			codes:  videoCodes, dur: 15,
			want: map[string]any{"duration": "15", "mode": "text2video"}, wantDur: 15,
		},
		{
			name: "不认识的键原样保留（配置可以比代码新）", nodeType: "video", op: "text2video",
			params: map[string]any{"presetOptions": map[string]any{"a": "b"}, "fooBar": 1},
			codes:  videoCodes,
			want:   map[string]any{"presetOptions": map[string]any{"a": "b"}, "fooBar": 1, "mode": "text2video"},
		},
		{
			name: "图片：张数写进 count，节点没有 duration 时不写", nodeType: "image", op: "text2img",
			params: map[string]any{"aspectRatio": "1:1", "negativePrompt": "糊"},
			codes:  []string{"prompt", "model", "aspect_ratio", "count", "negative_prompt"}, dur: 5, count: 2,
			want:    map[string]any{"aspect_ratio": "1:1", "negative_prompt": "糊", "count": "2"},
			wantDur: 5, wantCount: 2,
		},
		{
			name: "目录取不到时退回 seed 副本", nodeType: "video", op: "text2video",
			params: map[string]any{"aspectRatio": "9:16"}, dur: 15,
			want:    map[string]any{"aspect_ratio": "9:16", "duration": "15", "mode": "text2video"},
			wantDur: 15,
		},
		{
			name: "音频没有画幅参数：驼峰键不动", nodeType: "audio", op: "tts",
			params: map[string]any{"aspectRatio": "9:16", "text": "你好"},
			codes:  []string{"prompt", "voice", "model", "speed"},
			want:   map[string]any{"aspectRatio": "9:16", "text": "你好"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, _ := json.Marshal(c.params)
			got, dur, count := NormalizeGenerateParams(c.nodeType, c.op, c.params, c.codes, c.dur, c.count)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("参数：得到 %v，期望 %v", got, c.want)
			}
			if dur != c.wantDur || count != c.wantCount {
				t.Errorf("受理参数：dur=%d count=%d，期望 %d %d", dur, count, c.wantDur, c.wantCount)
			}
			after, _ := json.Marshal(c.params)
			if string(before) != string(after) {
				t.Error("输入 map 不应被修改")
			}
		})
	}
}

// 会话 107398945801502720：Agent 建的视频节点没有画幅，上游按自己的默认处理而该型号不支持（400）。
// 新建节点按画布配置的 defaultValue 补齐，与参数面板挂载时补的是同一份。
func TestFillNodeDefaults_只补缺的键并同步时长张数(t *testing.T) {
	videoDefaults := map[string]string{"video_mode": "text2video", "duration": "5", "resolution": "720p",
		"aspect_ratio": "16:9", "seed": ""}
	in := map[string]any{"model": "sd", "mode": "ref2video", "aspect_ratio": "9:16"}
	got, dur, cnt := FillNodeDefaults(in, videoDefaults, 0, 0)
	want := map[string]any{"model": "sd", "mode": "ref2video", "aspect_ratio": "9:16", "resolution": "720p",
		"duration": "5"}
	if !reflect.DeepEqual(got, want) || dur != 5 || cnt != 0 {
		t.Fatalf("只补缺的键（模型给的画幅、操作决定的 mode 不动；空默认值不补）：%v dur=%d cnt=%d", got, dur, cnt)
	}
	if len(in) != 3 {
		t.Fatal("输入 map 不应被修改")
	}
	// 时长已给（受理值）时不被默认值改掉。
	if got, dur, _ := FillNodeDefaults(map[string]any{"duration": "15"}, videoDefaults, 15, 0); got["duration"] != "15" || dur != 15 {
		t.Fatalf("给了的时长不变：%v %d", got, dur)
	}
	// 图片：张数补上后同步回受理值；没有 duration 参数的节点类型不动时长。
	imageDefaults := map[string]string{"quality": "standard", "aspect_ratio": "16:9", "count": "1"}
	got, dur, cnt = FillNodeDefaults(map[string]any{"duration": "8"}, imageDefaults, 0, 0)
	if got["count"] != "1" || cnt != 1 || dur != 0 || got["quality"] != "standard" {
		t.Fatalf("图片默认值：%v dur=%d cnt=%d", got, dur, cnt)
	}
	// 没有默认值（目录没接 / 取不到）：原样返回。
	if got, dur, cnt := FillNodeDefaults(map[string]any{"a": 1}, nil, 3, 2); len(got) != 1 || dur != 3 || cnt != 2 {
		t.Fatalf("没有默认值时原样返回：%v", got)
	}
}

// 已有节点上生成：受理参数 = 节点当前参数 + 这次给的（前端「从节点生成」发的就是节点的全部 params）。
func TestOverlayNodeParams_节点参数打底这次给的覆盖(t *testing.T) {
	node := map[string]any{"model": "old", "prompt": "旧描述", "aspect_ratio": "9:16", "duration": "15", "resolution": "1080p"}
	given := map[string]any{"model": "sd", "prompt": "新描述", "mode": "ref2video"}
	keys := map[string]bool{"duration": true, "aspect_ratio": true}
	got, dur, cnt := OverlayNodeParams(node, given, keys, 0, 0)
	want := map[string]any{"model": "sd", "prompt": "新描述", "mode": "ref2video", "aspect_ratio": "9:16",
		"duration": "15", "resolution": "1080p"}
	if !reflect.DeepEqual(got, want) || dur != 15 || cnt != 0 {
		t.Fatalf("节点打底、这次给的覆盖，时长取节点上的：%v dur=%d", got, dur)
	}
	if node["model"] != "old" || len(given) != 3 {
		t.Fatal("输入 map 不应被修改")
	}
	// 这次给了时长：以这次的为准（计价按它算）。
	if _, dur, _ := OverlayNodeParams(node, map[string]any{"duration": "10"}, keys, 10, 0); dur != 10 {
		t.Fatalf("这次给的时长优先：%d", dur)
	}
	// 该类节点没有 duration 参数时不从节点取时长。
	if _, dur, _ := OverlayNodeParams(node, given, map[string]bool{}, 0, 0); dur != 0 {
		t.Fatalf("没有该参数的节点类型不取时长：%d", dur)
	}
	// 读不到节点参数：只用这次给的（与原先一致）。
	if got, _, _ := OverlayNodeParams(nil, given, keys, 0, 0); !reflect.DeepEqual(got, given) {
		t.Fatalf("没有节点参数时原样：%v", got)
	}
}

func TestParamsByNode_取各节点参数(t *testing.T) {
	got := ParamsByNode(json.RawMessage(`{"nodes":[{"id":"a","params":{"aspect_ratio":"9:16"}},{"id":"b"},{"params":{"x":1}}]}`))
	if len(got) != 1 || got["a"]["aspect_ratio"] != "9:16" {
		t.Fatalf("只收有 id 且有 params 的节点：%v", got)
	}
	if ParamsByNode(json.RawMessage(`坏`)) != nil || ParamsByNode(nil) != nil {
		t.Fatal("解不开返回 nil")
	}
}
