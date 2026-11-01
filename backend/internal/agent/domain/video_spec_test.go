package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func answered(t *testing.T, payload QuestionsPayload, answers ...Answer) AnsweredQuestions {
	t.Helper()
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := json.Marshal(QuestionsSubmission{Answers: answers})
	if err != nil {
		t.Fatal(err)
	}
	return AnsweredQuestions{Payload: p, Submission: s}
}

// 规格三问按固定题目 id 认，其余题目不管；同一题以最新一次回答为准。
func TestSpecAnswers_按固定题目id取回答(t *testing.T) {
	other := "竖屏就行"
	older := answered(t, QuestionsPayload{Questions: []Question{
		{ID: SpecRatio, Type: QuestionSingle, Options: []Option{{ID: "1", Label: "16:9 横屏"}}},
		{ID: SpecDuration, Type: QuestionText},
	}}, Answer{QuestionID: SpecRatio, Selected: []string{"1"}}, Answer{QuestionID: SpecDuration, Text: "10 秒"})
	newer := answered(t, QuestionsPayload{Questions: []Question{
		{ID: "selling_point", Type: QuestionText},
		{ID: SpecRatio, Type: QuestionSingle, AllowOther: true, Options: []Option{{ID: "1", Label: "9:16 竖屏（推荐）"}}},
		{ID: SpecResolution, Type: QuestionSingle, Options: []Option{{ID: "a", Label: "720p"}, {ID: "b", Label: "1080P 超清"}}},
	}}, Answer{QuestionID: "selling_point", Text: "性价比"},
		Answer{QuestionID: SpecRatio, Other: &other},
		Answer{QuestionID: SpecResolution, Selected: []string{"b"}})
	bad := AnsweredQuestions{Payload: json.RawMessage(`坏`), Submission: json.RawMessage(`{}`)}

	got := SpecAnswers([]AnsweredQuestions{newer, bad, older})
	want := map[string]string{SpecRatio: "竖屏就行", SpecResolution: "1080P 超清", SpecDuration: "10 秒"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSpecValues_从回答里取值(t *testing.T) {
	for in, want := range map[string]string{"9:16 竖屏（推荐）": "9:16", "16：9": "16:9", "竖屏就行": "9:16", "横屏": "16:9",
		"正方形": "1:1", "随便": ""} {
		if got := NormalizeRatio(in); got != want {
			t.Errorf("NormalizeRatio(%q) = %q，want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"720p": "720p", "1080P 超清": "1080p", "高清 720 P": "720p", "4K": "4k", "高清": ""} {
		if got := NormalizeResolution(in); got != want {
			t.Errorf("NormalizeResolution(%q) = %q，want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"15 秒（推荐）": 15, "10秒": 10, "30": 30, "一分钟": 0, "0 秒": 0, "900 秒": 0} {
		if got := ParseDurationSec(in); got != want {
			t.Errorf("ParseDurationSec(%q) = %d，want %d", in, got, want)
		}
	}
}
