package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	tl "github.com/li904338203/tabtv/backend/internal/timeline/domain"
)

// 共享 fixture（dev/02 §6）：api/fixtures/smartcut/draft（解析与校验）与 compile（编译，含兜底）。
// 期望值是按规则手算写死的，不拿实现去生成 —— 见各文件的 why。

func fixtureFiles(t *testing.T, sub string) []string {
	t.Helper()
	dir := filepath.Join("..", "..", "..", "..", "api", "fixtures", "smartcut", sub)
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("没有找到 %s 下的 fixture：%v", dir, err)
	}
	sort.Strings(files)
	return files
}

func readFixture(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		t.Fatalf("%s：%v", filepath.Base(path), err)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func jsonEqual(t *testing.T, got any, want json.RawMessage) (bool, string) {
	t.Helper()
	g, _ := json.Marshal(got)
	var gv, wv any
	_ = json.Unmarshal(g, &gv)
	_ = json.Unmarshal(want, &wv)
	return reflect.DeepEqual(gv, wv), string(g)
}

// ---------------------------------------------------------------- draft/

type draftFixture struct {
	Why  string `json:"why"`
	Mode string `json:"mode"`
	Refs struct {
		Cards     []Card   `json:"cards"`
		Shots     []string `json:"shots"`
		Sentences []string `json:"sentences"`
		Clips     []string `json:"clips"`
		Texts     []string `json:"texts"`
		Items     int      `json:"items"`
	} `json:"refs"`
	// User 假上游收到的 user 消息（fake-reply-*：假上游据此拼出 raw，它的测试钉住两者逐字一致）。
	User   string `json:"user"`
	Raw    string `json:"raw"`
	Expect struct {
		Invalid string          `json:"invalid"`
		Draft   json.RawMessage `json:"draft"`
		Dropped []string        `json:"dropped"`
		Ignored []string        `json:"ignored"`
	} `json:"expect"`
}

func TestDraftFixtures(t *testing.T) {
	for _, path := range fixtureFiles(t, "draft") {
		name := filepath.Base(path)
		var f draftFixture
		readFixture(t, path, &f)
		refs := Refs{Cards: f.Refs.Cards, Shots: f.Refs.Shots, Sentences: f.Refs.Sentences, Clips: f.Refs.Clips,
			Texts: f.Refs.Texts, Items: f.Refs.Items}
		got, err := ParseDraft(f.Mode, f.Raw, refs)
		if f.Expect.Invalid != "" {
			var di *ErrDraftInvalid
			if !errors.As(err, &di) || di.Reason != f.Expect.Invalid {
				t.Errorf("%s：期望不合规「%s」，实际 %v", name, f.Expect.Invalid, err)
				continue
			}
			if di.Snippet == "" {
				t.Errorf("%s：不合规要带原文片段", name)
			}
			if f.Expect.Dropped != nil && !sameStrings(got.Dropped, f.Expect.Dropped) {
				t.Errorf("%s：丢弃说明\n期望 %q\n实际 %q", name, f.Expect.Dropped, got.Dropped)
			}
			if hint := RepairHint(got.Dropped, err); !strings.Contains(hint, di.Reason) {
				t.Errorf("%s：修复提示要带原因：%s", name, hint)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：%v", name, err)
			continue
		}
		if ok, g := jsonEqual(t, got.Draft, f.Expect.Draft); !ok {
			t.Errorf("%s：方案\n期望 %s\n实际 %s", name, f.Expect.Draft, g)
		}
		if !sameStrings(got.Dropped, f.Expect.Dropped) {
			t.Errorf("%s：丢弃说明\n期望 %q\n实际 %q", name, f.Expect.Dropped, got.Dropped)
		}
		if !sameStrings(got.Ignored, f.Expect.Ignored) {
			t.Errorf("%s：忽略说明\n期望 %q\n实际 %q", name, f.Expect.Ignored, got.Ignored)
		}
		if len(got.Dropped) > 0 {
			hint := RepairHint(got.Dropped, nil)
			for _, d := range got.Dropped {
				if !strings.Contains(hint, d) {
					t.Errorf("%s：修复提示要列出每一处问题：缺「%s」", name, d)
				}
			}
		}
	}
}

// ---------------------------------------------------------------- compile/

type fixturePiece struct {
	Chars   string `json:"chars"`
	Text    string `json:"text"`
	StartMs int    `json:"startMs"`
	EndMs   int    `json:"endMs"`
	CharMs  int    `json:"charMs"`
}

type fixtureTranscript struct {
	Language    string           `json:"language"`
	Granularity string           `json:"granularity"`
	Utterances  [][]fixturePiece `json:"utterances"`
}

func (f fixtureTranscript) build() Transcript {
	tr := Transcript{Language: f.Language, Granularity: f.Granularity}
	for _, pieces := range f.Utterances {
		var words []Word
		for _, p := range pieces {
			if p.Chars != "" {
				words = append(words, chars(p.Chars, p.StartMs, p.CharMs)...)
			} else {
				words = append(words, Word{Text: p.Text, StartMs: p.StartMs, EndMs: p.EndMs})
			}
		}
		tr.Utterances = append(tr.Utterances, utter(words))
	}
	return tr
}

type fixturePeaks struct {
	IntervalMs int     `json:"intervalMs"`
	LengthMs   int     `json:"lengthMs"`
	Base       int     `json:"base"`
	Set        [][]int `json:"set"`
}

func (f fixturePeaks) build() Peaks {
	vals := make([]byte, f.LengthMs/f.IntervalMs)
	for i := range vals {
		vals[i] = byte(f.Base)
	}
	for _, s := range f.Set {
		for t := s[0]; t < s[1]; t += f.IntervalMs {
			vals[t/f.IntervalMs] = byte(s[2])
		}
	}
	return Peaks{IntervalMs: f.IntervalMs, Values: vals}
}

type compileFixture struct {
	Why      string      `json:"why"`
	Mode     string      `json:"mode"`
	Base     *tl.Content `json:"base"`
	Cards    []Card      `json:"cards"`
	Spec     Spec        `json:"spec"`
	Raw      string      `json:"raw"`
	Fallback bool        `json:"fallback"`
	BeatGrid map[string]struct {
		IntervalMs    int `json:"intervalMs"`
		UntilMs       int `json:"untilMs"`
		DownbeatEvery int `json:"downbeatEvery"`
	} `json:"beatGrid"`
	Shots       []BoardShot                  `json:"shots"`
	Transcripts map[string]fixtureTranscript `json:"transcripts"`
	Peaks       map[string]fixturePeaks      `json:"peaks"`
	Script      string                       `json:"script"`
	Expect      struct {
		Video        []string     `json:"video"`
		VideoRole    string       `json:"videoRole"`
		TotalMs      *int         `json:"totalMs"`
		UsedClips    []string     `json:"usedClips"`
		Reasons      []ClipReason `json:"reasons"`
		Warnings     *[]string    `json:"warnings"`
		Notes        *[]string    `json:"notes"`
		Subtitles    []string     `json:"subtitles"`
		Music        string       `json:"music"`
		Voice        string       `json:"voice"`
		Title        string       `json:"title"`
		UsedFallback bool         `json:"usedFallback"`
		Summary      string       `json:"summary"`
	} `json:"expect"`
}

func (f compileFixture) input() PlanInput {
	in := PlanInput{Mode: f.Mode, Base: testBase(), Cards: f.Cards, Spec: f.Spec, Script: f.Script,
		Shots: NumberShots(f.Shots), Beats: map[string]Beats{}, Transcripts: map[string]Transcript{}, Peaks: map[string]Peaks{}}
	if f.Base != nil {
		in.Base = *f.Base
	}
	for no, g := range f.BeatGrid {
		var b Beats
		for i, ms := 0, 0; ms <= g.UntilMs; i, ms = i+1, ms+g.IntervalMs {
			b.BeatsMs = append(b.BeatsMs, ms)
			if g.DownbeatEvery > 0 && i%g.DownbeatEvery == 0 {
				b.DownbeatsMs = append(b.DownbeatsMs, ms)
			}
		}
		in.Beats[no] = b
	}
	for no, tr := range f.Transcripts {
		in.Transcripts[no] = tr.build()
	}
	for no, p := range f.Peaks {
		in.Peaks[no] = p.build()
	}
	return in
}

// refsOf 解析这个模式的方案时能引用的编号（与应用层组 user 消息时同一份编号）。
func refsOf(in PlanInput) Refs {
	refs := Refs{Cards: in.Cards, Shots: ShotRefs(in.Shots)}
	switch in.Mode {
	case ModeTalk:
		refs.Sentences = SentenceRefs(TalkSentences(in))
	case ModeNarr:
		refs.Sentences = SentenceRefs(NarrSentences(in))
	}
	return refs
}

func clipText(c tl.ContentClip, audio bool) string {
	s := fmt.Sprintf("%d %d~%d", c.MediaID, c.SourceInMs, c.SourceOutMs)
	if c.Speed != nil && *c.Speed != tl.DefaultSpeed {
		s += fmt.Sprintf(" ×%d", *c.Speed)
	}
	if c.TransitionIn != nil && c.TransitionIn.IsActive() {
		s += fmt.Sprintf(" ←%s%d", c.TransitionIn.Kind, c.TransitionIn.DurationMs)
	}
	switch {
	case audio && c.FadeOutMs > 0:
		s += fmt.Sprintf(" fadeOut%d", c.FadeOutMs)
	case !audio && (c.FadeInMs > 0 || c.FadeOutMs > 0):
		s += fmt.Sprintf(" fade%d/%d", c.FadeInMs, c.FadeOutMs)
	}
	return s
}

func TestCompileFixtures(t *testing.T) {
	for _, path := range fixtureFiles(t, "compile") {
		name := filepath.Base(path)
		var f compileFixture
		readFixture(t, path, &f)
		in := f.input()
		run := func() (Result, error) {
			if f.Fallback {
				return Fallback(in)
			}
			pr, err := ParseDraft(f.Mode, f.Raw, refsOf(in))
			if err != nil {
				return Result{}, err
			}
			return Compile(in, pr.Draft)
		}
		r, err := run()
		if err != nil {
			t.Errorf("%s：%v", name, err)
			continue
		}
		again, _ := run()
		ja, _ := json.Marshal(r)
		jb, _ := json.Marshal(again)
		if string(ja) != string(jb) {
			t.Errorf("%s：同样的输入应得到逐字节相同的输出", name)
		}
		lay := mustValid(t, r)
		e := f.Expect

		var vids []string
		role := ""
		for _, tr := range r.Content.Tracks {
			if tr.Kind == tl.TrackVideo {
				role = tr.Role
				for _, c := range tr.Clips {
					vids = append(vids, clipText(c, false))
				}
			}
		}
		if e.Video != nil && !sameStrings(vids, e.Video) {
			t.Errorf("%s：画面轨\n期望 %q\n实际 %q", name, e.Video, vids)
		}
		if role != e.VideoRole {
			t.Errorf("%s：画面轨用途期望 %q，实际 %q", name, e.VideoRole, role)
		}
		if e.TotalMs != nil && r.TotalMs != *e.TotalMs {
			t.Errorf("%s：总长期望 %d，实际 %d", name, *e.TotalMs, r.TotalMs)
		}
		if e.UsedClips != nil && !sameStrings(r.UsedClips, e.UsedClips) {
			t.Errorf("%s：用到的编号期望 %q，实际 %q", name, e.UsedClips, r.UsedClips)
		}
		if e.Reasons != nil && !reflect.DeepEqual(r.Reasons, e.Reasons) {
			t.Errorf("%s：理由期望 %+v，实际 %+v", name, e.Reasons, r.Reasons)
		}
		if e.Warnings != nil && !sameStrings(r.Warnings, *e.Warnings) {
			t.Errorf("%s：警告\n期望 %q\n实际 %q", name, *e.Warnings, r.Warnings)
		}
		if e.Notes != nil && !sameStrings(r.Notes, *e.Notes) {
			t.Errorf("%s：说明\n期望 %q\n实际 %q", name, *e.Notes, r.Notes)
		}
		var subs []string
		for _, s := range r.Content.Subtitles {
			p := lay.Subtitles[s.ID]
			subs = append(subs, fmt.Sprintf("%d~%d %s", p.StartMs, p.EndMs, s.Text))
		}
		if e.Subtitles != nil && !sameStrings(subs, e.Subtitles) {
			t.Errorf("%s：字幕\n期望 %q\n实际 %q", name, e.Subtitles, subs)
		}
		audio := func(role string) string {
			for _, tr := range r.Content.Tracks {
				if tr.Kind == tl.TrackAudio && tr.Role == role && len(tr.Clips) > 0 {
					return clipText(tr.Clips[0], true)
				}
			}
			return ""
		}
		if got := audio(RoleMusic); got != e.Music {
			t.Errorf("%s：配乐期望 %q，实际 %q", name, e.Music, got)
		}
		if got := audio(RoleVoice); got != e.Voice {
			t.Errorf("%s：配音期望 %q，实际 %q", name, e.Voice, got)
		}
		title := ""
		for _, tx := range r.Content.Texts {
			if tx.ID == "tmp-title" {
				title = fmt.Sprintf("%d~%d %s", tx.StartMs, tx.EndMs, tx.Text)
			}
		}
		if title != e.Title {
			t.Errorf("%s：标题期望 %q，实际 %q", name, e.Title, title)
		}
		if r.UsedFallback != e.UsedFallback {
			t.Errorf("%s：是否兜底期望 %v", name, e.UsedFallback)
		}
		if e.Summary != "" && r.Summary != e.Summary {
			t.Errorf("%s：摘要期望 %q，实际 %q", name, e.Summary, r.Summary)
		}
	}
}
