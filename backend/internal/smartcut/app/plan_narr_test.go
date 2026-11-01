package app

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	"github.com/li904338203/tabtv/backend/internal/smartcut/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
)

// narrItems 一条 6 秒的配音（编号 1）+ 两段视频（编号 2、3）；withText 时再连一段解说稿（编号 4）。
func narrItems(withText string) []domain.Input {
	d := func(ms int) *int { return &ms }
	items := []domain.Input{
		{No: "1", Kind: domain.InputAudio, Status: domain.InputReady, MediaID: "21", DurationMs: d(6000), AudioRole: domain.RoleVoice},
		{No: "2", Kind: domain.InputVideo, Status: domain.InputReady, MediaID: "22", DurationMs: d(5000)},
		{No: "3", Kind: domain.InputVideo, Status: domain.InputReady, MediaID: "23", DurationMs: d(5000)},
	}
	if withText != "" {
		items = append(items, domain.Input{No: "4", Kind: domain.InputText, Status: domain.InputReady, Text: withText})
	}
	return items
}

// narrHeard 配音识别出来的（字级，没有标点）：「第一句话」0~800、「第二句话」3000~3800。
func narrHeard() domain.Transcript {
	words := func(text string, at int) []domain.Word {
		var out []domain.Word
		for _, r := range text {
			out = append(out, domain.Word{Text: string(r), StartMs: at, EndMs: at + 200})
			at += 200
		}
		return out
	}
	return domain.Transcript{Language: "zh-CN", Granularity: "char", Utterances: []domain.Utterance{
		{Text: "第一句话", StartMs: 0, EndMs: 800, Words: words("第一句话", 0)},
		{Text: "第二句话", StartMs: 3000, EndMs: 3800, Words: words("第二句话", 3000)},
	}}
}

func narrService(m *talkMaterial) *Service {
	s := &Service{}
	s.plan.Material = m
	s.transcribe.Catalog = quoteCatalog{models: []ModelChoice{{Code: "volc", Granularity: "char"}}}
	return s
}

// SCT-35（SC-F-104）：素材里没有配音 → 42217 no_voice，出路是连一个音频节点；不去取识别结果。
// 「不计限流」在 Plan 里由它先于调用记录保证（集成测试 TestNarrPlan_没有配音不发起规划 核对）。
func TestNarr_没有配音回42217不计限流(t *testing.T) {
	m := &talkMaterial{}
	items := narrItems("解说稿。")[1:]
	_, err := narrService(m).gatherFacts(context.Background(), 1, 2, domain.ModeNarr, items, domain.Spec{}, "")
	be := bizOf(t, err)
	if be.Code != errcode.SmartcutInputUnsuitable || be.Details["reason"] != domain.UnsuitableNoVoice ||
		!reflect.DeepEqual(be.Details["options"], []string{"connect_audio"}) || !strings.Contains(be.Message, "音频节点生成配音") {
		t.Fatalf("%v %v", be.Message, be.Details)
	}
	if len(m.need) != 0 {
		t.Fatal("没有配音时不去取识别结果")
	}
}

// SCT-35：配音还没有识别（或只有整段文字）→ 42217 asr_missing，说清是哪一条，带按字级模型的识别报价（不要求保留口癖）。
func TestNarr_没有识别结果带报价(t *testing.T) {
	m := &talkMaterial{why: map[int64]string{}}
	s := narrService(m)
	items := narrItems("")
	_, err := s.gatherFacts(context.Background(), 1, 2, domain.ModeNarr, items, domain.Spec{}, "")
	be := bizOf(t, err)
	if be.Details["reason"] != domain.UnsuitableASRMissing || be.Message != "解说成片要先识别配音（编号 1：还没有识别）" {
		t.Fatalf("%v %v", be.Message, be.Details)
	}
	if len(m.need) != 1 || m.need[0] != narrNeed {
		t.Fatalf("按解说的要求取识别结果（字级，不要求口癖）：%+v", m.need)
	}
	be = bizOf(t, s.withTranscribeQuote(context.Background(), PlanRequest{Mode: domain.ModeNarr}, items, err))
	q, _ := be.Details["quote"].(map[string]any)
	if !reflect.DeepEqual(be.Details["missing"], []string{"1"}) || q["credits"] != 12 || q["keepFillers"] != false {
		t.Fatalf("报价（6 秒 × 2）：%v", be.Details)
	}

	m.why[21] = "识别结果只有整句或整段文字（没有字级时间），换一个字级或词级的识别模型重新识别"
	_, err = s.gatherFacts(context.Background(), 1, 2, domain.ModeNarr, items, domain.Spec{}, "")
	if be := bizOf(t, err); !strings.Contains(be.Message, "编号 1：识别结果只有整句或整段文字") {
		t.Fatalf("说清为什么不能用：%s", be.Message)
	}
}

// SCT-35（SC-F-18）：连进来的文本素材就是解说稿，不再去还原配音的原稿；字幕用原稿的文字与标点。
func TestNarr_原稿优先取文本素材(t *testing.T) {
	m := &talkMaterial{have: map[int64]domain.Transcript{21: narrHeard()}, spoken: map[int64]string{21: "快照里的另一份稿子。"}}
	f, err := narrService(m).gatherFacts(context.Background(), 1, 2, domain.ModeNarr, narrItems("第一句话，开场。第二句话！"),
		domain.Spec{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.spokenAsked != 0 || f.in.Script != "第一句话，开场。第二句话！" {
		t.Fatalf("文本素材优先：问了 %d 次快照，稿子 %q", m.spokenAsked, f.in.Script)
	}
	if len(f.sentences) != 2 || f.sentences[0].Text != "第一句话，开场。" || f.sentences[1].Text != "第二句话！" {
		t.Fatalf("按原稿断句：%+v", f.sentences)
	}
}

// SCT-35：没有文本素材时从配音那次文字转语音的快照还原原稿（不读画布上节点现在的文字）；快照也没有时用识别文字。
func TestNarr_从配音快照还原原稿(t *testing.T) {
	m := &talkMaterial{have: map[int64]domain.Transcript{21: narrHeard()}, spoken: map[int64]string{21: "第一句话。第二句话？"}}
	f, err := narrService(m).gatherFacts(context.Background(), 1, 2, domain.ModeNarr, narrItems(""), domain.Spec{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.spokenAsked != 1 || f.in.VoiceScripts["1"] != "第一句话。第二句话？" {
		t.Fatalf("按配音的编号记下原稿：%+v", f.in.VoiceScripts)
	}
	if len(f.sentences) != 2 || f.sentences[1].Text != "第二句话？" || f.sentences[1].StartMs != 3000 {
		t.Fatalf("字幕用快照里的原稿、时间取识别：%+v", f.sentences)
	}

	m.spoken = nil
	f, err = narrService(m).gatherFacts(context.Background(), 1, 2, domain.ModeNarr, narrItems(""), domain.Spec{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sentences) != 2 || f.sentences[0].Text != "第一句话" {
		t.Fatalf("快照也没有时用识别文字：%+v", f.sentences)
	}
}

// SCT-35（SC-AC-43）：成片长度等于配音（画面铺满整段配音，配音之外不铺）。
func TestNarr_成片长度等于配音(t *testing.T) {
	m := &talkMaterial{have: map[int64]domain.Transcript{21: narrHeard()}}
	f, err := narrService(m).gatherFacts(context.Background(), 1, 2, domain.ModeNarr, narrItems("第一句话。第二句话。"),
		domain.Spec{WithSubtitles: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	f.in.Base = testContent()
	r, err := domain.Compile(f.in, domain.Draft{Mode: domain.ModeNarr, Lines: []domain.NarrLine{
		{Sentence: "s1", Clips: []string{"2"}, Take: domain.Take{Kind: domain.TakeHead}},
		{Sentence: "s2", Clips: []string{"3"}, Take: domain.Take{Kind: domain.TakeHead}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if r.TotalMs != 6000 || r.Shortage != nil {
		t.Fatalf("成片 = 配音 6 秒：%d %+v", r.TotalMs, r.Shortage)
	}
	var subs []string
	for _, s := range r.Content.Subtitles {
		subs = append(subs, s.Text)
	}
	if strings.Join(subs, "|") != "第一句话。|第二句话。" {
		t.Fatalf("字幕与原稿逐字一致：%v", subs)
	}
}

// SCT-35：模型两次都不合规 → 兜底按句均分画面（两句、两段画面各配一句），时间线照样写入，调用记录记失败。
func TestNarr_兜底按句均分(t *testing.T) {
	m := &talkMaterial{have: map[int64]domain.Transcript{21: narrHeard()}}
	f, err := narrService(m).gatherFacts(context.Background(), 1, 2, domain.ModeNarr, narrItems("第一句话。第二句话。"),
		domain.Spec{}, "")
	if err != nil {
		t.Fatal(err)
	}
	f.in.Base = testContent()
	r := &planRun{}
	out, applied, err := (&Service{}).compileRuns(r, f, []variantRun{{invalid: &domain.ErrDraftInvalid{Reason: "模型输出无法解析为 JSON",
		Snippet: "我觉得"}}})
	if err != nil {
		t.Fatal(err)
	}
	v := out.Variants[applied]
	if !v.Fallback || !r.fallback || v.Status != repo.StatusSuccess || v.TotalMs != 6000 ||
		!reflect.DeepEqual(v.UsedClips, []string{"2", "3"}) || !strings.Contains(r.errDetail, "我觉得") {
		t.Fatalf("兜底：%+v %+v", v.VariantResult, r)
	}
}
