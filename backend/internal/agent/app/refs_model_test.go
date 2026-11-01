package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/li904338203/tabtv/backend/internal/agent/repo"
)

// 会话 107398945801502720：首条消息的引用原样附给模型，模型把商品库商品的 id 当成 referenceId
// 写进 generate 的 inputs，被判 plan_item_mismatch。商品库 / 角色库的 id 不再给模型看。
func TestToChatMessages_商品库角色库引用不带ID(t *testing.T) {
	msgs := []repo.Message{
		{ID: 1, Role: repo.RoleUser, Content: "做一支带货视频", Refs: json.RawMessage(
			`[{"id": "107396993168441344", "kind": "marketing_product", "slot": "product", "label": "沙发", "order": 1}, ` +
				`{"id": "900", "kind": "marketing_character", "slot": "character", "label": "小美", "order": 1}, ` +
				`{"id": "55", "kind": "media", "slot": "character", "order": 2}]`)},
		{ID: 2, Role: repo.RoleUser, Content: "用这个节点",
			Refs: json.RawMessage(`[{"id": "n1", "kind": "node", "label": "图片"}]`)},
	}
	chat := toChatMessages(msgs)
	first := chat[0].Content
	for _, bad := range []string{"107396993168441344", `"900"`} {
		if strings.Contains(first, bad) {
			t.Fatalf("商品库 / 角色库的 id 不该给模型看（%s）：%s", bad, first)
		}
	}
	for _, want := range []string{`"kind":"marketing_product"`, `"label":"沙发"`, `"kind":"marketing_character"`,
		`"label":"小美"`, `"id":"55"`} {
		if !strings.Contains(first, want) {
			t.Fatalf("其余字段与别的引用照常保留（缺 %s）：%s", want, first)
		}
	}
	// 没有商品库 / 角色库引用的消息逐字不变：通用会话的节点 / 媒体引用要靠 id 指认。
	if want := "用这个节点\n\n[引用] " + string(msgs[1].Refs); chat[1].Content != want {
		t.Fatalf("其余引用应逐字不变：\n got %q\nwant %q", chat[1].Content, want)
	}
}
