package wiring

import (
	"errors"
	"reflect"
	"testing"

	agentdomain "github.com/li904338203/tabtv/backend/internal/agent/domain"
	mktdomain "github.com/li904338203/tabtv/backend/internal/marketing/domain"
)

// agent 不能 import marketing，所以选商品图的序号校验在 agent/domain 有一份（submit_plan 与确认卡换图共用）。
// 这张表把它与 marketing/domain.SelectImages 钉在一起：改一边另一边红（MKT-T-20）。
func TestSelectProductIndexes_与营销域SelectImages一致(t *testing.T) {
	cases := []struct {
		total, max int
		idx        []int
	}{
		{20, 3, nil}, {2, 3, nil}, {5, 0, nil}, {0, 3, nil}, {5, -1, nil},
		{5, 3, []int{4, 2}}, {5, 3, []int{6}}, {5, 3, []int{0}}, {5, 3, []int{1, 1}},
		{5, 1, []int{1, 2}}, {5, 0, []int{1}}, {5, 3, []int{1, 2, 3}}, {5, 2, []int{9, 9, 9}},
	}
	for _, c := range cases {
		a, aerr := agentdomain.SelectProductIndexes(c.total, c.idx, c.max)
		m, merr := mktdomain.SelectImages(c.total, c.idx, c.max)
		if (aerr == nil) != (merr == nil) {
			t.Errorf("%+v：agent err=%v，marketing err=%v", c, aerr, merr)
			continue
		}
		if aerr != nil {
			var ae *agentdomain.ProductIndexError
			var me *mktdomain.ImageSelectError
			if !errors.As(aerr, &ae) || !errors.As(merr, &me) || ae.Code != me.Code || ae.Index != me.Index ||
				ae.Max != me.Max || ae.Total != me.Total {
				t.Errorf("%+v：错误分类不一致 agent=%+v marketing=%+v", c, aerr, merr)
			}
			continue
		}
		if len(a) == 0 && len(m) == 0 {
			continue
		}
		if !reflect.DeepEqual(a, m) {
			t.Errorf("%+v：结果不一致 agent=%v marketing=%v", c, a, m)
		}
	}
}

func TestAgentPlanScene_口播提示与阻塞说明(t *testing.T) {
	s := AgentPlanScene{}
	if s.TalkNotice([]string{"口播"}) != mktdomain.TalkDisclaimer || s.TalkNotice([]string{" 口播 "}) != mktdomain.TalkDisclaimer {
		t.Error("带口播标签应给营销域的提示句")
	}
	if s.TalkNotice([]string{"带货"}) != "" || s.TalkNotice(nil) != "" {
		t.Error("不带口播标签不给提示")
	}
	o := mktdomain.Option{Kind: mktdomain.KindHook, Name: "抓耳音效"}
	for code, want := range map[string]string{
		mktdomain.BlockNeedsAudio:     "Hook「抓耳音效」需要能出声的视频模型，当前所选模型不出声",
		mktdomain.BlockOutputMismatch: "Hook「抓耳音效」不适用于当前产出",
		"needs_source_video":          "Hook「抓耳音效」需要源视频",
	} {
		if got := planBlockText(o, code); got != want {
			t.Errorf("%s：%q", code, got)
		}
	}
}
