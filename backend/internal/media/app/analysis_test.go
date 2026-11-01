package app

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/media/repo"
)

func TestIDList_读写(t *testing.T) {
	b, _ := json.Marshal(IDList{9007199254740993, 1})
	if string(b) != `["9007199254740993","1"]` {
		t.Fatalf("写成字符串数组：%s", b)
	}
	for _, in := range []string{`["9007199254740993","1"]`, `[9007199254740993,1]`} {
		var l IDList
		if err := json.Unmarshal([]byte(in), &l); err != nil || !slices.Equal(l, IDList{9007199254740993, 1}) {
			t.Fatalf("%s → %v %v", in, l, err)
		}
	}
	var l IDList
	if json.Unmarshal([]byte(`["x"]`), &l) == nil {
		t.Fatal("非法 ID 要报错")
	}
}

// 取「当前」那一行：当前引擎版本的排队 / 进行中 / 成功优先，没有再取最近一次失败；旧版本不算。
func TestPickAnalysis(t *testing.T) {
	rows := []repo.Analysis{ // 新的在前
		{ID: 5, Kind: "peaks", Engine: "peaks-v1", Status: "failed"},
		{ID: 4, Kind: "peaks", Engine: "peaks-v0", Status: "success"},
		{ID: 3, Kind: "filmstrip", Engine: "filmstrip-v1", Status: "failed"},
		{ID: 2, Kind: "peaks", Engine: "peaks-v1", Status: "queued"},
		{ID: 1, Kind: "filmstrip", Engine: "filmstrip-v1", Status: "failed"},
	}
	if r := pickAnalysis(rows, "peaks", "peaks-v1"); r == nil || r.ID != 2 {
		t.Fatalf("排队中的优先于更新的失败行：%+v", r)
	}
	if r := pickAnalysis(rows, "filmstrip", "filmstrip-v1"); r == nil || r.ID != 3 {
		t.Fatalf("只有失败时取最近一次：%+v", r)
	}
	if r := pickAnalysis(rows, "preview_proxy", "proxy-v1"); r != nil {
		t.Fatal("没有就是 nil（status 回 none）")
	}
	if r := pickAnalysis(rows[1:2], "peaks", "peaks-v1"); r != nil {
		t.Fatal("旧引擎版本的结果不算（升级后要重做）")
	}
	if r := pickAnalysis(rows, "peaks", ""); r == nil || r.ID != 4 {
		t.Fatalf("引擎不限时（识别）取最新的非失败行：%+v", r)
	}
}
