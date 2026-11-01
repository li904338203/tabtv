package domain

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestTimelineIDOf_只认正整数(t *testing.T) {
	cases := map[string]int64{
		`{"timelineId":"123"}`:              123,
		`{"timelineId":" 77 "}`:             77,
		`{"timelineId":123}`:                123,
		`{"timelineId":"0"}`:                0,
		`{"timelineId":"-5"}`:               0,
		`{"timelineId":"abc"}`:              0,
		`{"timelineId":1.5}`:                0,
		`{"timelineId":108612084723351552}`: 0, // 超出 JS 安全整数：已经丢了精度
		`{}`:                                0,
	}
	for raw, want := range cases {
		var p map[string]any
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if got := TimelineIDOf(p); got != want {
			t.Errorf("%s：期望 %d，实际 %d", raw, want, got)
		}
	}
	if NodeParams(9)["timelineId"] != "9" {
		t.Error("写回节点的是字符串")
	}
}

func TestRebindTimelineIDs_只改智能剪辑节点的时间线(t *testing.T) {
	canvas := json.RawMessage(`{"nodes":[
		{"id":"a","type":"smartcut","params":{"timelineId":"11","prompt":"x"}},
		{"id":"b","type":"smartcut","params":{"timelineId":"99"}},
		{"id":"c","type":"video","params":{"timelineId":"11"}},
		{"id":"d","type":"smartcut"}],"edges":[],"future":{"keep":1}}`)
	out, n, err := RebindTimelineIDs(canvas, map[int64]int64{11: 21})
	if err != nil || n != 1 {
		t.Fatalf("应改 1 处：%d %v", n, err)
	}
	var tree struct {
		Nodes []struct {
			ID     string         `json:"id"`
			Params map[string]any `json:"params"`
		} `json:"nodes"`
		Future map[string]any `json:"future"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, nd := range tree.Nodes {
		got[nd.ID] = nd.Params
	}
	if got["a"]["timelineId"] != "21" || got["a"]["prompt"] != "x" {
		t.Errorf("节点 a 应改指 21 并保留别的参数：%v", got["a"])
	}
	if got["b"]["timelineId"] != "99" || got["c"]["timelineId"] != "11" {
		t.Errorf("表里没有的、不是智能剪辑的节点不动：%v / %v", got["b"], got["c"])
	}
	if tree.Future["keep"] != float64(1) {
		t.Error("未知字段原样保留")
	}
	same, n, err := RebindTimelineIDs(canvas, map[int64]int64{5: 6})
	if err != nil || n != 0 || string(same) != string(canvas) {
		t.Fatal("没有可改的时原样返回（字节不变）")
	}
	if _, _, err := RebindTimelineIDs(json.RawMessage(`[1,2]`), map[int64]int64{1: 2}); err == nil {
		t.Fatal("画布不是对象应报错")
	}
}

func TestRenderStale(t *testing.T) {
	if RenderStale(0, 5) {
		t.Error("没导出过不算过期")
	}
	if !RenderStale(3, 5) || RenderStale(5, 5) {
		t.Error("导出后又改过才算过期")
	}
}

func TestStrip_上限与配额(t *testing.T) {
	mk := func(kind string, n int) []StripItem {
		out := make([]StripItem, n)
		for i := range out {
			out[i] = StripItem{Kind: kind, StartMs: i * 1000, DurationMs: 1000}
		}
		return out
	}
	cases := []struct {
		video, audio, wantVideo, wantAudio int
	}{
		{5, 2, 5, 2},
		{60, 3, 37, 3},
		{60, 20, 30, 10},
		{10, 50, 10, 30},
		{0, 0, 0, 0},
	}
	for _, c := range cases {
		got := Strip(mk("video", c.video), mk("audio", c.audio))
		v, a := 0, 0
		for _, it := range got {
			if it.Kind == "audio" {
				a++
			} else {
				v++
			}
		}
		if v != c.wantVideo || a != c.wantAudio || len(got) > MaxStripItems {
			t.Errorf("画面 %d + 音频 %d：期望 %d+%d，实际 %d+%d", c.video, c.audio, c.wantVideo, c.wantAudio, v, a)
		}
	}
}

func TestCover_成片优先其次第一段(t *testing.T) {
	video := []StripItem{{Kind: "video", PosterMediaID: 7}}
	if Cover(99, video, 1) != 99 {
		t.Error("有成片封面用成片的")
	}
	if Cover(0, video, 1) != 7 {
		t.Error("没有成片用第一段的封面帧")
	}
	if Cover(0, []StripItem{{Kind: "image"}}, 5) != 5 {
		t.Error("第一段是图片时它就是封面")
	}
	if Cover(0, nil, 0) != 0 {
		t.Error("空时间线没有封面")
	}
}

// TestDomain_不依赖数据库与网络：编译、规划、素材条都在这里，必须是纯函数（同导演台 / 时间线的绊线）。
func TestDomain_不依赖数据库与网络(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Skipf("go list 不可用：%v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "database/sql", dep == "net", dep == "net/http", strings.Contains(dep, "jackc/pgx"),
			strings.HasPrefix(dep, "github.com/li904338203/tabtv/backend/internal/") && !strings.HasPrefix(dep, "github.com/li904338203/tabtv/backend/internal/smartcut/domain") &&
				dep != "github.com/li904338203/tabtv/backend/internal/timeline/domain" && dep != "github.com/li904338203/tabtv/backend/internal/platform/lenientjson":
			t.Errorf("smartcut/domain 依赖了 %s", dep)
		}
	}
}
