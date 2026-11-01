package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/agent/app"
	"github.com/li904338203/tabtv/backend/internal/agent/domain"
	"github.com/li904338203/tabtv/backend/internal/agent/repo"
	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
)

// ask_user（dev/01 §5.3、design/03 §3.4 A/B）：发选择卡或分页问答卡并挂起。
//
// ⚠️ 卡片里记 resumeState（发卡前的状态）：提交后会话回到这里（domain.SubmissionEvent）。
// 不记的话执行中问一句，答完会被带回规划态 —— 下发的工具变了，已批准的额度也用不了。
// ⚠️ 卡片结构在发卡时就校验到位：一张选项为空的单选卡用户永远提交不了（必须恰好选一项），
// 会话就卡死在 awaiting_input 直到过期。

const (
	maxAskOptions   = 10
	maxAskQuestions = 10
	maxAskTitle     = 100
	maxOptionLabel  = 100
	maxOptionDetail = 500
	maxOptionIDLen  = 32
)

// AskUser 是 ask_user 工具。
type AskUser struct{ d PlanningDeps }

func (AskUser) Name() domain.ToolName { return domain.ToolAskUser }
func (AskUser) Paid() bool            { return false }
func (AskUser) Action() collab.Action { return collab.ActionEdit }

func (AskUser) Description() string {
	return "向用户提问并暂停，等用户回答后继续。kind=choice 发一张选择卡（单选或多选，可带「我有别的想法」）；" +
		"kind=questions 发一组分页问题（每题 single / multi / text）。只在确实需要用户决定时使用。"
}

func (AskUser) Schema() json.RawMessage {
	opt := `{"type":"object","required":["label"],"properties":{"id":{"type":"string"},"label":{"type":"string"},"detail":{"type":"string"}}}`
	return json.RawMessage(`{"type":"object","required":["kind","title"],"properties":{` +
		`"kind":{"type":"string","enum":["choice","questions"]},` +
		`"title":{"type":"string"},` +
		`"multi":{"type":"boolean","description":"choice：是否多选"},` +
		`"allowOther":{"type":"boolean","description":"choice：是否带「我有别的想法」"},` +
		`"options":{"type":"array","items":` + opt + `,"minItems":1,"maxItems":10},` +
		`"questions":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"object","required":["text","type"],"properties":{` +
		`"id":{"type":"string"},"text":{"type":"string"},"type":{"type":"string","enum":["single","multi","text"]},` +
		`"allowOther":{"type":"boolean"},"options":{"type":"array","items":` + opt + `}}}}` +
		`},"additionalProperties":false}`)
}

type askArgs struct {
	Kind       string            `json:"kind"`
	Title      string            `json:"title"`
	Multi      bool              `json:"multi"`
	AllowOther bool              `json:"allowOther"`
	Options    []domain.Option   `json:"options"`
	Questions  []domain.Question `json:"questions"`
}

// buildAskPayload 校验参数并组装卡片内容。
func buildAskPayload(a askArgs, resume domain.State) (domain.CardKind, any, error) {
	if _, err := domain.InputResumeEvent(resume); err != nil {
		return "", nil, fmt.Errorf("当前阶段（%s）不能向用户提问", resume)
	}
	title := ptTrim(a.Title)
	if title == "" {
		return "", nil, fmt.Errorf("缺少 title")
	}
	title, _ = ptCutRunes(title, maxAskTitle)
	switch a.Kind {
	case string(domain.CardChoice):
		if len(a.Questions) > 0 {
			return "", nil, fmt.Errorf("choice 卡不接受 questions")
		}
		opts, err := normOptions("", a.Options)
		if err != nil {
			return "", nil, err
		}
		return domain.CardChoice, domain.ChoicePayload{Title: title, Multi: a.Multi, Options: opts,
			AllowOther: a.AllowOther, ResumeState: resume}, nil
	case string(domain.CardQuestions):
		// 只有一道题而选项写在了卡上：意思是明确的，挪进那道题（04 文档：实测模型连错两次才写对，每次都是一次模型调用）。
		if len(a.Options) > 0 && len(a.Questions) == 1 && len(a.Questions[0].Options) == 0 {
			q := a.Questions[0]
			q.Options, q.AllowOther = a.Options, q.AllowOther || a.AllowOther
			if q.Type == "" || q.Type == domain.QuestionText {
				q.Type = domain.QuestionSingle
				if a.Multi {
					q.Type = domain.QuestionMulti
				}
			}
			a.Questions[0], a.Options, a.Multi, a.AllowOther = q, nil, false, false
		}
		if len(a.Options) > 0 || a.Multi || a.AllowOther {
			return "", nil, fmt.Errorf("questions 卡的选项写在每道题里（questions[i].options），卡片顶层不放 options / multi / allowOther")
		}
		if len(a.Questions) == 0 {
			return "", nil, fmt.Errorf("questions 不能为空")
		}
		if len(a.Questions) > maxAskQuestions {
			return "", nil, fmt.Errorf("一次最多 %d 道题", maxAskQuestions)
		}
		seen := map[string]bool{}
		qs := make([]domain.Question, 0, len(a.Questions))
		for i, q := range a.Questions {
			prefix := fmt.Sprintf("第 %d 题：", i+1)
			q.ID = ptTrim(q.ID)
			if q.ID == "" {
				q.ID = "q" + strconv.Itoa(i+1)
			}
			if len(q.ID) > maxOptionIDLen || seen[q.ID] {
				return "", nil, fmt.Errorf("%sid 重复或过长", prefix)
			}
			seen[q.ID] = true
			q.Text = ptTrim(q.Text)
			if q.Text == "" {
				return "", nil, fmt.Errorf("%s缺少题目文字", prefix)
			}
			q.Text, _ = ptCutRunes(q.Text, maxOptionDetail)
			switch q.Type {
			case domain.QuestionText:
				if len(q.Options) > 0 {
					// 文本题带了选项：模型想给几个参考答案。按「单选 + 可以自己写」出题，而不是退回让它重写
					// （04 文档：实测这一条被拒了好几次，每次都是一次模型调用）。
					q.Type, q.AllowOther = domain.QuestionSingle, true
					opts, err := normOptions(prefix, q.Options)
					if err != nil {
						return "", nil, err
					}
					q.Options = opts
				}
				q.AllowOther = q.AllowOther && len(q.Options) > 0
			case domain.QuestionSingle, domain.QuestionMulti:
				opts, err := normOptions(prefix, q.Options)
				if err != nil {
					return "", nil, err
				}
				q.Options = opts
			default:
				return "", nil, fmt.Errorf("%stype 只能是 single / multi / text", prefix)
			}
			qs = append(qs, q)
		}
		return domain.CardQuestions, domain.QuestionsPayload{Title: title, Questions: qs, ResumeState: resume}, nil
	}
	return "", nil, fmt.Errorf("kind 只能是 choice / questions")
}

// normOptions 校验选项：至少一项、ID 唯一（没给就按序号补）、标签非空。
func normOptions(prefix string, in []domain.Option) ([]domain.Option, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%s选项不能为空", prefix)
	}
	if len(in) > maxAskOptions {
		return nil, fmt.Errorf("%s选项最多 %d 个", prefix, maxAskOptions)
	}
	seen := map[string]bool{}
	out := make([]domain.Option, 0, len(in))
	for i, o := range in {
		o.ID = ptTrim(o.ID)
		if o.ID == "" {
			o.ID = strconv.Itoa(i + 1)
		}
		if len(o.ID) > maxOptionIDLen || seen[o.ID] {
			return nil, fmt.Errorf("%s选项 id %q 重复或过长", prefix, o.ID)
		}
		seen[o.ID] = true
		o.Label = ptTrim(o.Label)
		if o.Label == "" {
			return nil, fmt.Errorf("%s第 %d 个选项缺少 label", prefix, i+1)
		}
		o.Label, _ = ptCutRunes(o.Label, maxOptionLabel)
		o.Detail, _ = ptCutRunes(ptTrim(o.Detail), maxOptionDetail)
		out = append(out, o)
	}
	return out, nil
}

func (t AskUser) Run(ctx context.Context, rc RunContext, args json.RawMessage) (ToolResult, error) {
	var a askArgs
	if err := ptDecode(args, &a, false); err != nil {
		return ToolResult{}, err
	}
	kind, payload, err := buildAskPayload(a, rc.State)
	if err != nil {
		return ToolResult{}, err
	}
	card, err := t.d.Store.InsertCard(ctx, t.d.Store.Pool(), repo.Card{ID: t.d.NextID(),
		SessionID: rc.SessionID, StepID: rc.StepID, Kind: kind, Payload: ptJSON(payload)})
	if err != nil {
		return ToolResult{}, fmt.Errorf("发卡失败：%w", err)
	}
	ptPublishCard(ctx, t.d, rc.ProjectID, rc.SessionID, card.ID, kind, domain.CardPending)
	return ToolResult{
		Content: ptJSON(map[string]any{"cardId": fmt.Sprint(card.ID), "status": "awaiting_input",
			"note": "卡片已发给用户，本轮到此暂停；用户回答后会作为新消息发给你"}),
		Display: ptJSON(map[string]any{"cardId": fmt.Sprint(card.ID), "kind": kind, "title": ptTrim(a.Title)}),
		Suspend: &Suspension{Event: domain.EvAskUser, CardID: card.ID, Reason: "awaiting_input"},
	}, nil
}

var _ app.Tool = AskUser{}
