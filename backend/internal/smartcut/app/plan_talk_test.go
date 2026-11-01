package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// talkMaterial 口播 / 解说用的素材事实：识别结果、「识别过却不能用」的原因与配音原稿由用例给，其余为空。
type talkMaterial struct {
	MaterialFacts
	have   map[int64]domain.Transcript
	why    map[int64]string
	need   []TranscriptNeed
	spoken map[int64]string
	// spokenAsked 问过几次配音原稿（有文本素材时不该问）。
	spokenAsked int
}

func (m *talkMaterial) VoiceScripts(context.Context, int64, []int64) (map[int64]string, error) {
	m.spokenAsked++
	return m.spoken, nil
}

func (m *talkMaterial) Descriptions(context.Context, int64, int64, []int64, string) (map[int64]string, error) {
	return nil, nil
}
func (m *talkMaterial) Peaks(context.Context, int64, []int64) (map[int64]domain.Peaks, error) {
	return nil, nil
}
func (m *talkMaterial) Transcripts(_ context.Context, _ int64, _ []int64, need TranscriptNeed) (
	map[int64]domain.Transcript, map[int64]string, error) {
	m.need = append(m.need, need)
	return m.have, m.why, nil
}

func talkItems() []domain.Input {
	d := func(ms int) *int { return &ms }
	return []domain.Input{
		{No: "1", Kind: domain.InputVideo, Status: domain.InputReady, MediaID: "11", DurationMs: d(10500)},
		{No: "2", Kind: domain.InputVideo, Status: domain.InputReady, MediaID: "12", DurationMs: d(4000)},
	}
}

func bizOf(t *testing.T, err error) *httpx.BizError {
	t.Helper()
	var be *httpx.BizError
	if !errors.As(err, &be) {
		t.Fatalf("期望业务错误，实际 %v", err)
	}
	return be
}

// SCT-34：口播素材还没有识别结果 → 42217 asr_missing，说清是哪几份；再补上「先识别」要的报价。
// 判断用的是口播的要求（保留口癖 + 字级时间），在发起规划之前（Plan 里先于限流与调用记录，见集成测试）。
func TestTalk_无识别结果回42217带报价(t *testing.T) {
	m := &talkMaterial{have: map[int64]domain.Transcript{12: {Granularity: "char",
		Utterances: []domain.Utterance{{Text: "好", StartMs: 0, EndMs: 300}}}}}
	s := &Service{}
	s.plan.Material = m
	s.transcribe.Catalog = quoteCatalog{models: []ModelChoice{{Code: "volc", Granularity: "char"}}}
	items := talkItems()
	_, err := s.gatherFacts(context.Background(), 1, 2, domain.ModeTalk, items, domain.Spec{}, "")
	be := bizOf(t, err)
	if be.Code != errcode.SmartcutInputUnsuitable || be.Details["reason"] != domain.UnsuitableASRMissing ||
		be.Message != "口播精剪要先识别语音（编号 1：还没有识别）" {
		t.Fatalf("42217 并说清缺哪一份：%v %v", be.Message, be.Details)
	}
	if len(m.need) != 1 || m.need[0] != talkNeed {
		t.Fatalf("按口播的要求取识别结果：%+v", m.need)
	}
	err = s.withTranscribeQuote(context.Background(), PlanRequest{Mode: domain.ModeTalk}, items, err)
	be = bizOf(t, err)
	q, _ := be.Details["quote"].(map[string]any)
	// 只报缺的那一份：10.5 秒向上取整 11 秒 × 2 积分；保留口癖。
	if !reflect.DeepEqual(be.Details["missing"], []string{"1"}) || q["credits"] != 22 || q["keepFillers"] != true {
		t.Fatalf("报价：%v", be.Details)
	}
}

// SCT-34（SC-F-122、SCK-5）：段级 / 整段文字的结果、顺滑过（去掉口癖）的结果、静音的结果都不能用于口播；
// 新的不能用、旧的能用时用旧的；解说不要求字级时间与口癖。原因进 42217 的说明。
func TestTalk_段级结果不能用于口播(t *testing.T) {
	res := func(gran string, n int) json.RawMessage {
		us := make([]domain.Utterance, n)
		for i := range us {
			us[i] = domain.Utterance{Text: "好", StartMs: i * 1000, EndMs: i*1000 + 300}
		}
		b, _ := json.Marshal(domain.Transcript{Granularity: gran, Utterances: us})
		return b
	}
	cases := []struct {
		name  string
		cands []TranscriptCandidate
		need  TranscriptNeed
		ok    bool
		why   string
	}{
		{"段级", []TranscriptCandidate{{"f1.zh-CN", res("segment", 1)}}, talkNeed, false, "识别结果只有整句或整段文字"},
		{"整段文字", []TranscriptCandidate{{"f1.auto", res("text", 1)}}, talkNeed, false, "识别结果只有整句或整段文字"},
		{"去掉了口癖", []TranscriptCandidate{{"f0.zh-CN", res("char", 1)}}, talkNeed, false, "识别时去掉了口癖"},
		{"静音", []TranscriptCandidate{{"f1.zh-CN", res("char", 0)}}, talkNeed, false, "识别结果里没有语音"},
		{"字级保留口癖", []TranscriptCandidate{{"f1.zh-CN", res("char", 1)}}, talkNeed, true, ""},
		{"词级", []TranscriptCandidate{{"f1.en", res("word", 1)}}, talkNeed, true, ""},
		{"新的不能用用旧的", []TranscriptCandidate{{"f0.zh-CN", res("char", 2)}, {"f1.zh-CN", res("char", 1)}}, talkNeed, true, ""},
		{"都不能用报最新那份", []TranscriptCandidate{{"f1.zh-CN", res("text", 1)}, {"f0.zh-CN", res("char", 1)}}, talkNeed, false,
			"识别结果只有整句或整段文字"},
		{"解说可用段级", []TranscriptCandidate{{"f0.zh-CN", res("segment", 1)}}, TranscriptNeed{}, true, ""},
		{"坏数据跳过", []TranscriptCandidate{{"f1.zh-CN", json.RawMessage(`{`)}, {"f1.zh-CN", res("char", 1)}}, talkNeed, true, ""},
	}
	for _, c := range cases {
		tr, why, ok := PickTranscript(c.cands, c.need)
		if ok != c.ok || (c.why == "" && why != "") || !strings.Contains(why, c.why) {
			t.Errorf("%s：ok=%v why=%q", c.name, ok, why)
		}
		if ok && c.name == "新的不能用用旧的" && len(tr.Utterances) != 1 {
			t.Errorf("%s：挑的应是保留了口癖的那一份：%+v", c.name, tr)
		}
	}

	// 上游没回语言时取识别时指定的语言（字幕轨用它）；自动判断的不补。
	if tr, _, _ := PickTranscript([]TranscriptCandidate{{"f1.zh-CN", res("char", 1)}}, talkNeed); tr.Language != "zh-CN" {
		t.Fatalf("补上指定的语言：%q", tr.Language)
	}
	if tr, _, _ := PickTranscript([]TranscriptCandidate{{"f1.auto", res("char", 1)}}, talkNeed); tr.Language != "" {
		t.Fatalf("自动判断的不补：%q", tr.Language)
	}

	// 原因进 42217 的说明（两份都缺，一份是段级、一份没识别过）。
	s := &Service{}
	s.plan.Material = &talkMaterial{why: map[int64]string{11: "识别结果只有整句或整段文字（没有字级时间），换一个字级或词级的识别模型重新识别"}}
	_, err := s.gatherFacts(context.Background(), 1, 2, domain.ModeTalk, talkItems(), domain.Spec{}, "")
	be := bizOf(t, err)
	if !strings.Contains(be.Message, "编号 1：识别结果只有整句或整段文字") || !strings.Contains(be.Message, "编号 2：还没有识别") {
		t.Fatalf("说明：%s", be.Message)
	}
}

// SCT-34：口播用素材的哪几段取自时间线 —— 用户剪过的照片段（同一份素材接得上的并成一段），已经被 AI 剪过的
// 整段重剪，不在时间线上的整段排在后面；与缺省一样时不存。
func TestTalkSpansFrom_按时间线片段取区间(t *testing.T) {
	items := talkItems()
	clip := func(media int64, in, out int, origin string) timelinedomain.ContentClip {
		return timelinedomain.ContentClip{MediaID: media, SourceInMs: in, SourceOutMs: out, Origin: origin}
	}
	base := func(clips ...timelinedomain.ContentClip) timelinedomain.Content {
		return timelinedomain.Content{Tracks: []timelinedomain.ContentTrack{{Kind: timelinedomain.TrackVideo, Clips: clips}}}
	}
	text := func(spans []domain.TalkSpan) string {
		var out []string
		for _, sp := range spans {
			out = append(out, fmt.Sprintf("%d:%d-%d", sp.MediaID, sp.InMs, sp.OutMs))
		}
		return strings.Join(out, " ")
	}
	cases := []struct {
		name string
		base timelinedomain.Content
		want string
	}{
		{"自动铺好的整段 = 缺省", base(clip(11, 0, 10500, "sync"), clip(12, 0, 4000, "sync")), ""},
		{"空时间线 = 缺省", base(), ""},
		{"剪掉了开头", base(clip(11, 1000, 10500, "user"), clip(12, 0, 4000, "sync")), "11:1000-10500 12:0-4000"},
		{"中间切一刀并回一段", base(clip(11, 0, 3000, "user"), clip(11, 3000, 10500, "user"), clip(12, 0, 4000, "sync")), ""},
		{"顺序换了", base(clip(12, 0, 4000, "sync"), clip(11, 0, 10500, "sync")), "12:0-4000 11:0-10500"},
		{"AI 剪过的整段重剪", base(clip(11, 300, 1200, "ai"), clip(11, 2000, 3100, "ai"), clip(12, 500, 900, "user")),
			"11:0-10500 12:500-900"},
		{"不在时间线上的排后面", base(clip(12, 0, 2000, "user")), "12:0-2000 11:0-10500"},
		{"同一份素材分两处用", base(clip(11, 0, 2000, "user"), clip(12, 0, 4000, "sync"), clip(11, 5000, 8000, "user")),
			"11:0-2000 12:0-4000 11:5000-8000"},
	}
	for _, c := range cases {
		if got := text(talkSpansFrom(c.base, items)); got != c.want {
			t.Errorf("%s：期望 %q，实际 %q", c.name, c.want, got)
		}
	}
	// 时长未知的素材：整段到「无穷远」，由 TalkSentences 按识别结果自己收住。
	unknown := []domain.Input{{No: "1", Kind: domain.InputVideo, Status: domain.InputReady, MediaID: "11"}}
	if got := talkSpansFrom(base(clip(11, 1000, 5000, "user")), unknown); len(got) != 1 || got[0].OutMs != 5000 {
		t.Fatalf("%+v", got)
	}
	if got := talkSpansFrom(base(), unknown); got != nil {
		t.Fatalf("没上轨的时长未知素材仍是缺省：%+v", got)
	}
}

// chunkText 按请求回固定的规划（分段时每段一次调用，按「第 k 段」分辨）。
type chunkText struct {
	mu      sync.Mutex
	reqs    []TextRequest
	respond func(user string, n int) string
}

func (c *chunkText) Complete(_ context.Context, in TextRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, in)
	return c.respond(in.User, len(c.reqs)), nil
}

func longTalkFacts() planFacts {
	sents := []domain.Sentence{
		{Ref: "s1", CardNo: "1", Text: strings.Repeat("甲", 1800)},
		{Ref: "s2", CardNo: "1", Text: strings.Repeat("乙", 1800)},
		{Ref: "s3", CardNo: "1", Text: strings.Repeat("丙", 1800)},
	}
	return planFacts{in: domain.PlanInput{Mode: domain.ModeTalk, Cards: []domain.Card{{No: "1", Kind: domain.InputVideo, MediaID: 11}}},
		sentences: sents}
}

// SCT-34：识别稿超过 3000 字时分段规划 —— 每段一次调用、只看到自己的句子、结果按句号拼起来；
// 单独一段一句不留是合法的，拼起来一句都没有才算不合规；任何一段修过仍不合规，整套按不合规处理。
func TestTalk_长稿分段规划并按句号拼接(t *testing.T) {
	newService := func(text *chunkText) *Service {
		s := &Service{}
		s.plan.Text = text
		s.plan.Config = PlanConfig{}.withDefaults()
		return s
	}
	req := PlanRequest{Mode: domain.ModeTalk}
	text := &chunkText{respond: func(user string, _ int) string {
		switch {
		case strings.Contains(user, "这是第 1 段（s1~s1）"):
			return `{"keep":[{"from":"s1","to":"s1"}]}`
		case strings.Contains(user, "这是第 2 段（s2~s2）"):
			return `{"keep":[],"drop":[{"sentence":"s2","reason":"重录"}]}`
		case strings.Contains(user, "这是第 3 段（s3~s3）"):
			return `{"keep":[{"sentence":"s3"}],"subtitle":false}`
		}
		return "?"
	}}
	run := newService(text).runTalkVariant(context.Background(), "m", "sys", req, longTalkFacts(), nil, nil, 0, 1)
	if !run.ok || run.invalid != nil || run.callErr != nil {
		t.Fatalf("分段规划应成功：%+v", run)
	}
	d := run.parsed.Draft
	if fmt.Sprint(d.Keep) != "[{s1 s1} {s3 s3}]" || len(d.Drop) != 1 || d.Drop[0].Sentence != "s2" || d.Subtitle == nil || *d.Subtitle {
		t.Fatalf("拼接：%+v", d)
	}
	if len(text.reqs) != 3 {
		t.Fatalf("三段各调一次：%d", len(text.reqs))
	}
	for i, r := range text.reqs {
		// 每段只看到自己的句子（第 i+1 句）。
		for k, ch := range []string{"甲", "乙", "丙"} {
			if strings.Contains(r.User, strings.Repeat(ch, 20)) != (k == i) {
				t.Fatalf("第 %d 段的稿子只放自己的句子：%q 出现与否不对", i+1, ch)
			}
		}
	}

	// 某一段引用了别的段的句子：回灌修一次，仍不合规 → 整套不合规（不拼一半）。
	bad := &chunkText{respond: func(user string, _ int) string {
		if strings.Contains(user, "这是第 2 段") {
			return `{"keep":[{"from":"s1","to":"s1"}]}`
		}
		return `{"keep":[{"from":"s1","to":"s3"}]}`
	}}
	run = newService(bad).runTalkVariant(context.Background(), "m", "sys", req, longTalkFacts(), nil, nil, 0, 1)
	if run.ok || run.invalid == nil {
		t.Fatalf("有一段不合规时整套不合规：%+v", run)
	}

	// 每段都一句不留：拼起来是空的 → 不合规（走兜底，不写出一条空时间线）。
	empty := &chunkText{respond: func(string, int) string { return `{"keep":[],"drop":[]}` }}
	run = newService(empty).runTalkVariant(context.Background(), "m", "sys", req, longTalkFacts(), nil, nil, 0, 1)
	if run.ok || run.invalid == nil || !strings.Contains(run.invalid.Reason, "各段都是空的") {
		t.Fatalf("各段都空：%+v", run)
	}

	// 不长的稿子不分段，也不放宽「一句不留」。
	short := longTalkFacts()
	short.sentences = short.sentences[:1]
	short.sentences[0].Text = "短"
	once := &chunkText{respond: func(string, int) string { return `{"keep":[]}` }}
	run = newService(once).runTalkVariant(context.Background(), "m", "sys", req, short, nil, nil, 0, 1)
	if run.ok || len(once.reqs) != 2 || strings.Contains(once.reqs[0].User, "## 分段") {
		t.Fatalf("短稿一次调用（不合规时修一次），且一句不留不合规：%+v %d", run, len(once.reqs))
	}
}

// 契约（SCT-34）：42217 的 details 与 api/errors.yaml 里登记的形状一致 —— 发出去的键都在登记里（带 ? 的是可选）。
// errors.yaml 的 details 只写在注释里，没有代码生成；不核的话多发一个键、改一个名字都没人发现。
func TestTalk_42217详情与错误码表一致(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "api", "errors.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	at := strings.Index(text, "code: 42217")
	if at < 0 {
		t.Fatal("errors.yaml 里没有 42217")
	}
	block := text[strings.LastIndex(text[:at], "- { code:"):at]
	m := regexp.MustCompile(`details:\{([^}]*)\}`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("42217 的注释里没写 details：%s", block)
	}
	declared := map[string]bool{}
	for _, k := range strings.Split(m[1], ",") {
		declared[strings.TrimSuffix(strings.TrimSpace(k), "?")] = true
	}
	s := &Service{}
	s.plan.Material = &talkMaterial{}
	s.transcribe.Catalog = quoteCatalog{models: []ModelChoice{{Code: "volc", Granularity: "char"}}}
	_, err = s.gatherFacts(context.Background(), 1, 2, domain.ModeTalk, talkItems(), domain.Spec{}, "")
	be := bizOf(t, s.withTranscribeQuote(context.Background(), PlanRequest{Mode: domain.ModeTalk}, talkItems(), err))
	for k := range be.Details {
		if !declared[k] {
			t.Errorf("42217 发出了没登记的 details 键 %q（登记的是 %v）", k, m[1])
		}
	}
	if be.Details["quote"] == nil || be.Details["missing"] == nil {
		t.Fatalf("口播缺识别结果时带 missing 与 quote：%v", be.Details)
	}
}
