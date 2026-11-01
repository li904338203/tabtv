package domain

import (
	"reflect"
	"strings"
	"testing"

	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

func transcribeContent() timelinedomain.Content {
	clip := func(id string, media int64) timelinedomain.ContentClip {
		return timelinedomain.ContentClip{ID: id, MediaID: media, SourceOutMs: 1000}
	}
	return timelinedomain.Content{Tracks: []timelinedomain.ContentTrack{
		{ID: "a1", Kind: timelinedomain.TrackAudio, Role: timelinedomain.RoleMusic, Clips: []timelinedomain.ContentClip{clip("m1", 50)}},
		{ID: "v", Kind: timelinedomain.TrackVideo, Clips: []timelinedomain.ContentClip{clip("c1", 10), clip("c2", 11), clip("c3", 10)}},
		{ID: "a2", Kind: timelinedomain.TrackAudio, Role: timelinedomain.RoleVoice, Clips: []timelinedomain.ContentClip{clip("vo1", 30)}},
		{ID: "a3", Kind: timelinedomain.TrackAudio, Role: timelinedomain.RoleSfx, Clips: []timelinedomain.ContentClip{clip("s1", 40)}},
		{ID: "t", Kind: timelinedomain.TrackSubtitle},
	}}
}

func TestTranscribeSources(t *testing.T) {
	c := transcribeContent()
	ids, missing := TranscribeSources(c, nil)
	// 画面轨在前（去重），再是配音轨；配乐与音效不识别。
	if !reflect.DeepEqual(ids, []int64{10, 11, 30}) || missing != nil {
		t.Fatalf("整条时间线: %v %v", ids, missing)
	}
	ids, missing = TranscribeSources(c, []string{"m1", "c3", "nope"})
	// 明确选了配乐也照识别；不存在的片段原样报出来。
	if !reflect.DeepEqual(ids, []int64{10, 50}) || !reflect.DeepEqual(missing, []string{"nope"}) {
		t.Fatalf("指定片段: %v %v", ids, missing)
	}
}

func TestASRKeys(t *testing.T) {
	if ASRParamsKey(true, "zh-CN") != "f1.zh-CN" || ASRParamsKey(false, "") != "f0.auto" {
		t.Fatalf("参数键: %q %q", ASRParamsKey(true, "zh-CN"), ASRParamsKey(false, ""))
	}
	k1 := ASRIdempotencyKey(108860340061601792, strings.Repeat("m", 64), "f1.zh-CN", 1)
	k2 := ASRIdempotencyKey(108860340061601792, strings.Repeat("m", 64), "f1.zh-CN", 2)
	if k1 == k2 || len(k1) > 128 || !strings.HasPrefix(k1, "asr:108860340061601792:") || !strings.HasSuffix(k1, ":1") {
		t.Fatalf("幂等键: %q %q", k1, k2)
	}
	if ASRIdempotencyKey(1, "a", "f0.auto", 1) == ASRIdempotencyKey(1, "a", "f1.auto", 1) {
		t.Fatal("参数不同要是不同的键")
	}
	for _, c := range [][2]int{{0, 1}, {1, 1}, {1000, 1}, {1001, 2}, {60000, 60}} {
		if got := BillableSeconds(c[0]); got != c[1] {
			t.Errorf("BillableSeconds(%d) = %d，期望 %d", c[0], got, c[1])
		}
	}
}

// 报价与扣费同一个时长（SCT-42 实测：60 秒的素材抽出 60.03 秒的音轨，报 60 扣 61）：取小的那个，一个未知就用另一个。
func TestSpeechBillMs_素材与音轨取小(t *testing.T) {
	for _, c := range [][3]int{
		{60000, 60032, 60000}, // 编码补的尾巴不收费
		{60000, 30000, 30000}, // 后半段没声音：按音轨
		{0, 4200, 4200},       // 素材时长未知
		{5000, 0, 5000},       // 音轨未知
		{0, 0, 0},
	} {
		if got := SpeechBillMs(c[0], c[1]); got != c[2] {
			t.Errorf("SpeechBillMs(%d, %d) = %d，期望 %d", c[0], c[1], got, c[2])
		}
	}
	if BillableSeconds(SpeechBillMs(60000, 60032)) != 60 {
		t.Fatal("60 秒的素材应按 60 秒收")
	}
}
