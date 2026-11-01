package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// 推理模型看图（MKT-T-19，dev/01 §5.4）：view_media 之后的下一次请求，在**那一批**工具结果之后紧跟一条
// 带图的 user 消息。两家协议对这个形状的约束（核实结论写在 dev/01 修订块 R-3）：
//
//   - OpenAI Chat：助手的每个 tool_call 必须由紧随其后的 tool 消息回应；回应完之后接 user 消息是常规多轮形状，
//     user 的 content 数组可含 image_url（data: 地址）。⚠️ user 消息不能夹在同一批 tool 消息中间（Runner 负责）。
//   - Anthropic Messages：工具结果本身就是 user 消息里的 tool_result 块；紧跟的 user 内容被合并进**同一条**
//     user 消息（角色必须交替），tool_result 块必须排在最前，之后的 image / text 块是官方允许的形状 ——
//     所以不需要退化成「图放进 tool_result 的 content」。
//
// 图一律是 data: 内联（MKT-AC-05：请求体里不出现媒体外链）。

const viewImg = "data:image/jpeg;base64,/9j/4AAQSkZJRgABAQ=="

func viewMediaMessages() []ChatMessage {
	return []ChatMessage{
		{Role: RoleUser, Content: "给这款冷萃做个带货视频", Images: []string{"data:image/png;base64,iVBORw0KGgo="}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "call_1", Name: "view_media", Arguments: json.RawMessage(`{"slot":"product","index":7}`)},
			{ID: "call_2", Name: "read_doc", Arguments: json.RawMessage(`{"path":"/plan.md"}`)},
		}},
		{Role: RoleTool, ToolCallID: "call_1", Content: `{"ok":true,"label":"商品图 #7"}`},
		{Role: RoleTool, ToolCallID: "call_2", Content: `{"content":""}`},
		{Role: RoleUser, Content: "【系统通知，非用户发言】\n以下是你用 view_media 请求查看的图：商品图 #7",
			Images: []string{viewImg}},
	}
}

func TestProtocol_工具结果后带图_openai(t *testing.T) {
	req, err := mustChat(t, "openai_chat").BuildChat(ChatJob{UpstreamModel: "gpt-x", System: "sys",
		Messages: viewMediaMessages()})
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeBody(t, req)["messages"].([]any)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	// system + user + assistant + tool×2（紧跟在助手之后，中间不夹任何消息）+ 带图 user。
	if got := strings.Join(roles, ","); got != "system,user,assistant,tool,tool,user" {
		t.Fatalf("消息角色序列不对：%s", got)
	}
	last := msgs[5].(map[string]any)["content"].([]any)
	if len(last) != 2 || last[0].(map[string]any)["type"] != "text" {
		t.Fatalf("带图 user 消息应为 text + image_url：%v", last)
	}
	img := last[1].(map[string]any)
	if img["type"] != "image_url" || img["image_url"].(map[string]any)["url"] != viewImg {
		t.Fatalf("图应以 data: 内联发出：%v", img)
	}
	// 首条消息的图同样是 content 数组里的 image_url。
	first := msgs[1].(map[string]any)["content"].([]any)
	if len(first) != 2 || first[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("首条消息的附图形状不对：%v", first)
	}
	if strings.Contains(string(req.Body), "http://") || strings.Contains(string(req.Body), "https://") {
		t.Errorf("请求体里不应出现媒体外链（MKT-AC-05）")
	}
}

func TestProtocol_工具结果后带图_anthropic(t *testing.T) {
	req, err := mustChat(t, "anthropic_messages").BuildChat(ChatJob{UpstreamModel: "claude-x", System: "sys",
		Messages: viewMediaMessages()})
	if err != nil {
		t.Fatal(err)
	}
	msgs := decodeBody(t, req)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应为 user / assistant / user 三条（工具结果与紧跟的带图消息合并），实际 %d：%v", len(msgs), msgs)
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("第三条应为 user：%v", last)
	}
	blocks := last["content"].([]any)
	types := []string{}
	for _, b := range blocks {
		types = append(types, b.(map[string]any)["type"].(string))
	}
	// ⚠️ tool_result 在最前（上游硬性要求），之后是图（在文字前）与说明文字。
	if got := strings.Join(types, ","); got != "tool_result,tool_result,image,text" {
		t.Fatalf("块顺序不对：%s", got)
	}
	src := blocks[2].(map[string]any)["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/jpeg" || src["data"] != "/9j/4AAQSkZJRgABAQ==" {
		t.Fatalf("图应以 base64 source 发出：%v", src)
	}
	for i, id := range []string{"call_1", "call_2"} {
		if b := blocks[i].(map[string]any); b["tool_use_id"] != id {
			t.Fatalf("第 %d 块应回应 %s：%v", i, id, b)
		}
	}
	firstBlocks := msgs[0].(map[string]any)["content"].([]any)
	if firstBlocks[0].(map[string]any)["type"] != "image" {
		t.Fatalf("首条消息的图应在文字前：%v", firstBlocks)
	}
	if strings.Contains(string(req.Body), `"type":"url"`) {
		t.Errorf("请求体里不应出现 url 形态的图片源（MKT-AC-05）")
	}
}
