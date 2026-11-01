package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func choiceCard(t *testing.T, multi, allowOther bool) Card {
	return Card{Kind: CardChoice, Payload: mustJSON(t, ChoicePayload{
		Title: "方向", Multi: multi, AllowOther: allowOther, ResumeState: StatePlanning,
		Options: []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}, {ID: "c", Label: "C"}},
	})}
}

func TestValidateSubmission_单选恰好一项(t *testing.T) {
	card := choiceCard(t, false, true)
	long := strings.Repeat("想", MaxFreeTextRunes+1)
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"选一项", `{"selected":["a"]}`, true},
		{"只填别的想法", `{"selected":[],"other":"换成水墨风"}`, true},
		{"一个都没选", `{"selected":[]}`, false},
		{"选两项", `{"selected":["a","b"]}`, false},
		{"选一项又填想法", `{"selected":["a"],"other":"x"}`, false},
		{"选项不存在", `{"selected":["z"]}`, false},
		{"想法为空白", `{"selected":[],"other":"   "}`, false},
		{"想法超长", `{"selected":[],"other":"` + long + `"}`, false},
		{"想法恰好 500 字", `{"selected":[],"other":"` + strings.Repeat("想", MaxFreeTextRunes) + `"}`, true},
		{"键名写错", `{"selcted":["a"]}`, false},
		{"不是 JSON", `abc`, false},
	}
	for _, c := range cases {
		err := ValidateSubmission(card, json.RawMessage(c.body))
		if (err == nil) != c.ok {
			t.Errorf("%s：err=%v，期望 ok=%v", c.name, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalidSubmission) {
			t.Errorf("%s：错误应包装 ErrInvalidSubmission，得到 %v", c.name, err)
		}
	}
	// 不允许「别的想法」的卡。
	if err := ValidateSubmission(choiceCard(t, false, false),
		json.RawMessage(`{"selected":[],"other":"x"}`)); err == nil {
		t.Error("不允许别的想法时应拒绝")
	}
}

func TestValidateSubmission_多选至少一项(t *testing.T) {
	card := choiceCard(t, true, true)
	for body, ok := range map[string]bool{
		`{"selected":["a","b"]}`:           true,
		`{"selected":["a"],"other":"x"}`:   true,
		`{"selected":[]}`:                  false,
		`{"selected":["a","a"]}`:           false,
		`{"selected":["a","b","c","b"]}`:   false,
		`{"selected":["c"],"other":"   "}`: false,
	} {
		if err := ValidateSubmission(card, json.RawMessage(body)); (err == nil) != ok {
			t.Errorf("%s：err=%v，期望 ok=%v", body, err, ok)
		}
	}
}

func questionsCard(t *testing.T) Card {
	return Card{Kind: CardQuestions, Payload: mustJSON(t, QuestionsPayload{
		Title: "细节", ResumeState: StatePlanning,
		Questions: []Question{
			{ID: "q1", Text: "情绪", Type: QuestionSingle, AllowOther: true,
				Options: []Option{{ID: "sad", Label: "悲壮"}, {ID: "open", Label: "留白"}}},
			{ID: "q2", Text: "元素", Type: QuestionMulti,
				Options: []Option{{ID: "x", Label: "雨"}, {ID: "y", Label: "火"}}},
			{ID: "q3", Text: "补充", Type: QuestionText},
		},
	})}
}

func TestValidateSubmission_问答卡未答完拒绝(t *testing.T) {
	card := questionsCard(t)
	full := `{"answers":[{"questionId":"q1","selected":["sad"]},{"questionId":"q2","selected":["x","y"]},{"questionId":"q3","text":"多一点雨"}]}`
	if err := ValidateSubmission(card, json.RawMessage(full)); err != nil {
		t.Fatalf("全部答完应通过：%v", err)
	}
	bad := map[string]string{
		"缺第三题":       `{"answers":[{"questionId":"q1","selected":["sad"]},{"questionId":"q2","selected":["x"]}]}`,
		"单选题选两项":     `{"answers":[{"questionId":"q1","selected":["sad","open"]},{"questionId":"q2","selected":["x"]},{"questionId":"q3","text":"a"}]}`,
		"多选题一项没选":    `{"answers":[{"questionId":"q1","selected":["sad"]},{"questionId":"q2","selected":[]},{"questionId":"q3","text":"a"}]}`,
		"文本题为空":      `{"answers":[{"questionId":"q1","selected":["sad"]},{"questionId":"q2","selected":["x"]},{"questionId":"q3","text":" "}]}`,
		"重复作答":       `{"answers":[{"questionId":"q1","selected":["sad"]},{"questionId":"q1","selected":["open"]},{"questionId":"q2","selected":["x"]},{"questionId":"q3","text":"a"}]}`,
		"多余的题":       `{"answers":[{"questionId":"q1","selected":["sad"]},{"questionId":"q2","selected":["x"]},{"questionId":"q3","text":"a"},{"questionId":"q9","text":"a"}]}`,
		"多选题不支持别的想法": `{"answers":[{"questionId":"q1","selected":["sad"]},{"questionId":"q2","other":"x"},{"questionId":"q3","text":"a"}]}`,
	}
	for name, body := range bad {
		if err := ValidateSubmission(card, json.RawMessage(body)); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	// 单选题用「别的想法」作答。
	other := `{"answers":[{"questionId":"q1","other":"反转"},{"questionId":"q2","selected":["y"]},{"questionId":"q3","text":"a"}]}`
	if err := ValidateSubmission(card, json.RawMessage(other)); err != nil {
		t.Errorf("单选题用别的想法作答应通过：%v", err)
	}
}

func TestValidateSubmission_调整必须带意见(t *testing.T) {
	plan := Card{Kind: CardPlanConfirm, Payload: json.RawMessage(`{"title":"t","items":[],"total":60,"balance":100}`)}
	cases := map[string]bool{
		`{"decision":"approve"}`:                 true,
		`{"decision":"reject"}`:                  true,
		`{"decision":"adjust","comment":"改成竖屏"}`: true,
		`{"decision":"adjust"}`:                  false,
		`{"decision":"adjust","comment":"   "}`:  false,
		`{"decision":"maybe"}`:                   false,
		`{}`:                                     false,
		`{"decision":"approve","amount":999}`:    false, // 金额不收客户端的
	}
	for body, ok := range cases {
		if err := ValidateSubmission(plan, json.RawMessage(body)); (err == nil) != ok {
			t.Errorf("%s：err=%v，期望 ok=%v", body, err, ok)
		}
	}
	long := `{"decision":"adjust","comment":"` + strings.Repeat("改", MaxCommentRunes+1) + `"}`
	if err := ValidateSubmission(plan, json.RawMessage(long)); err == nil {
		t.Error("修改意见超长应拒绝")
	}
	// 追加额度卡没有 adjust。
	budget := Card{Kind: CardBudget, Payload: json.RawMessage(`{"reason":"r","requested":20}`)}
	if err := ValidateSubmission(budget, json.RawMessage(`{"decision":"adjust","comment":"x"}`)); err == nil {
		t.Error("追加额度卡不接受 adjust")
	}
	if err := ValidateSubmission(budget, json.RawMessage(`{"decision":"approve"}`)); err != nil {
		t.Errorf("追加额度卡 approve 应通过：%v", err)
	}
	if err := ValidateSubmission(Card{Kind: "bogus"}, json.RawMessage(`{}`)); err == nil {
		t.Error("未知卡片类型应拒绝")
	}
}

func TestSubmissionEvent_卡片提交对应迁移(t *testing.T) {
	choice := choiceCard(t, false, false)
	ev, err := SubmissionEvent(choice, json.RawMessage(`{"selected":["a"]}`))
	if err != nil || ev != EvInputResumePlanning {
		t.Errorf("选择卡应回发卡前状态，得到 %s %v", ev, err)
	}
	del := Card{Kind: CardDeleteConfirm, Payload: mustJSON(t, DeleteConfirmPayload{
		NodeIDs: []string{"n1"}, ResumeState: StateExecuting})}
	ev, err = SubmissionEvent(del, json.RawMessage(`{"decision":"reject"}`))
	if err != nil || ev != EvInputResumeExecuting {
		t.Errorf("删除确认卡应回发卡前状态，得到 %s %v", ev, err)
	}
	budget := Card{Kind: CardBudget, Payload: json.RawMessage(`{}`)}
	for body, want := range map[string]Event{
		`{"decision":"approve"}`: EvApprove,
		`{"decision":"reject"}`:  EvReject,
	} {
		if ev, err := SubmissionEvent(budget, json.RawMessage(body)); err != nil || ev != want {
			t.Errorf("%s 得到 %s %v，期望 %s", body, ev, err, want)
		}
	}
	bad := Card{Kind: CardChoice, Payload: json.RawMessage(`{"resumeState":"idle"}`)}
	if _, err := SubmissionEvent(bad, nil); !errors.Is(err, ErrBadResumeState) {
		t.Errorf("恢复状态非法应报错，得到 %v", err)
	}
}
