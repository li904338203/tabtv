package app

import (
	"context"
	"testing"
)

type stubClipModels map[string][]ModelChoice

func (s stubClipModels) ClipModels(_ context.Context, op string, _ int64, _ map[string]int) ([]ModelChoice, error) {
	return s[op], nil
}

// 补镜两种操作的模型合在一起：同一个模型只列一次（先列文生视频的），去字幕只列去字幕的。
func TestClipModelChoices_补镜合并两种操作并去重(t *testing.T) {
	s := &Service{}
	s.SetClipToolPorts(ClipToolPorts{Models: stubClipModels{
		"text2video":       {{Code: "kling", UnitPrice: 3}, {Code: "seedance", UnitPrice: 4}},
		"img2video":        {{Code: "seedance", UnitPrice: 9}, {Code: "uniart", UnitPrice: 5}},
		"video_desubtitle": {{Code: "erase", UnitPrice: 2}},
	}})
	fill, err := s.clipModelChoices(context.Background(), "fill", 0)
	if err != nil || len(fill) != 3 || fill[0].Code != "kling" || fill[1].Code != "seedance" || fill[1].UnitPrice != 4 ||
		fill[2].Code != "uniart" {
		t.Fatalf("补镜模型: %+v %v", fill, err)
	}
	erase, _ := s.clipModelChoices(context.Background(), "video_desubtitle", 0)
	if len(erase) != 1 || erase[0].Code != "erase" {
		t.Fatalf("去字幕模型: %+v", erase)
	}
	if none, _ := s.clipModelChoices(context.Background(), "text2music", 0); none == nil || len(none) != 0 {
		t.Fatalf("没有模型时是空数组不是 nil（契约里是数组）: %#v", none)
	}
}

// 配乐的缺省长度（dev/01 R-20 #8）：从播放头到成片结尾，播放头在 0 时就是成片时长；播放头已在结尾之后取成片时长；
// 空时间线 30 秒；显式秒数原样；都夹到 3~300 秒。与前端 smartcutMusicDuration 同一条规则。
func TestMusicWantMs_缺省从播放头到成片结尾(t *testing.T) {
	for _, c := range []struct {
		name                     string
		effective, at, sec, want int
	}{
		{"播放头在 0：成片时长", 9000, 0, 0, 9000},
		{"播放头在 1.5 秒：到结尾的 7.5 秒（按毫秒，不取整）", 9000, 1500, 0, 7500},
		{"播放头在结尾：成片时长", 9000, 9000, 0, 9000},
		{"播放头越过结尾：成片时长", 9000, 12000, 0, 9000},
		{"离结尾不到 3 秒：夹到 3 秒", 9000, 8000, 0, 3000},
		{"空时间线：30 秒", 0, 0, 0, 30000},
		{"显式 12 秒：原样", 9000, 1500, 12, 12000},
		{"超过 300 秒的成片：夹到 300 秒", 400000, 0, 0, 300000},
	} {
		if got := musicWantMs(c.effective, c.at, c.sec); got != c.want {
			t.Errorf("%s：musicWantMs(%d, %d, %d) = %d，应为 %d", c.name, c.effective, c.at, c.sec, got, c.want)
		}
	}
}
