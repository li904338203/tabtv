package app

import (
	"context"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/script/repo"
)

type fixedPrompt string

func (p fixedPrompt) RenderPrompt(context.Context, string, string, map[string]string) (string, error) {
	return string(p), nil
}

// 用户只填一个标题时，模型必须被明确告知「先补全、直接出 JSON、不许反问」。
//
// ⚠️ 起因（2026-10-14 实测）：节点里只填了「三打白骨精」，系统提示词写着「依据给定的剧情原文」、
// user 消息的标签也是「剧情原文：」，模型于是回了一句「请提供具体剧情原文」——
// 同步调用里没有人能回答这个问题，结果是扣费、解析失败、退款，用户什么也没拿到。
func TestBuildStoryboardPrompt_只给标题也要直接出分镜(t *testing.T) {
	for name, src := range map[string]PromptSource{
		"没配模板走内置兜底": nil,
		// 运营模板怎么写由运营决定；「不许反问」在代码固定追加的输出契约里，模板写成「依据原文」也照样生效。
		"运营模板写成依据原文": fixedPrompt("你是分镜师。请依据给定的剧情原文生成分镜。"),
	} {
		s := New(nil, func() int64 { return 1 })
		if src != nil {
			s.SetPrompts(src)
		}
		system, user, err := s.buildStoryboardPrompt(context.Background(),
			repo.Script{RawInput: "三打白骨精"}, StoryboardInput{ModelCode: "gpt-x"})
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		if !strings.Contains(system, "不要反问") || !strings.Contains(system, "补全") {
			t.Errorf("%s：系统提示词里必须写明短输入要先补全、不许反问:\n%s", name, system)
		}
		// CD-78：描述里的实体写成 @名称、与 entities 一一对应 —— 漏了这条，工作台的画面描述里资产不高亮、@ 没有候选
		if !strings.Contains(system, "@名称") || !strings.Contains(system, "一一对应") {
			t.Errorf("%s：输出契约里必须要求描述用 @名称 标记实体:\n%s", name, system)
		}
		if strings.Contains(user, "原文") {
			t.Errorf("%s：user 消息不该把输入标成「原文」（模型会据此认定没拿到原文）:\n%s", name, user)
		}
		if !strings.Contains(user, "三打白骨精") {
			t.Errorf("%s：user 消息必须带上用户的输入:\n%s", name, user)
		}
	}
}
