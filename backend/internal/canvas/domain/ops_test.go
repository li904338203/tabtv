package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 共享 fixture 与前端重放（AGT-11）同用：任一侧与 after 不一致即失败。
const fixtureDir = "../../../../api/fixtures/canvas-ops"

type opsFixture struct {
	Description string `json:"description"`
	Actor       struct {
		Kind       string `json:"kind"`
		UserID     string `json:"userId"`
		SessionID  string `json:"sessionId"`
		GrantID    string `json:"grantId"`
		ClientName string `json:"clientName"`
	} `json:"actor"`
	NewIDs      []string        `json:"newIds"`
	Before      json.RawMessage `json:"before"`
	Ops         []Op            `json:"ops"`
	After       json.RawMessage `json:"after"`
	Applied     json.RawMessage `json:"applied"`
	NodeIDs     []string        `json:"nodeIds"`
	ExpectError string          `json:"expectError"`
}

func (f opsFixture) actor(t *testing.T) Actor {
	t.Helper()
	uid, _ := strconv.ParseInt(f.Actor.UserID, 10, 64)
	sid, _ := strconv.ParseInt(f.Actor.SessionID, 10, 64)
	gid, _ := strconv.ParseInt(f.Actor.GrantID, 10, 64)
	return Actor{Kind: f.Actor.Kind, UserID: uid, SessionID: sid, GrantID: gid, ClientName: f.Actor.ClientName}
}

// seqIDs 按给定序列分配 ID；用完了就让测试失败（说明实现比 fixture 多分配了）。
func seqIDs(t *testing.T, ids []string) func() string {
	i := 0
	return func() string {
		if i >= len(ids) {
			t.Errorf("分配的 ID 多于 fixture 的 newIds（%d 个）", len(ids))
			return "a_overflow_" + strconv.Itoa(i)
		}
		i++
		return ids[i-1]
	}
}

func counter(prefix string) func() string {
	i := 0
	return func() string {
		i++
		return prefix + strconv.Itoa(i)
	}
}

// semantic 解析成 any（UseNumber，大整数按字面比较），键序不计。
func semantic(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("不是合法 JSON: %v\n%s", err, raw)
	}
	return v
}

func assertJSONEqual(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !reflect.DeepEqual(semantic(t, got), semantic(t, want)) {
		t.Errorf("%s 不一致\n得到: %s\n期望: %s", what, got, want)
	}
}

func errorName(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotOwned):
		return "not_owned"
	case errors.Is(err, ErrNodeNotFound):
		return "node_not_found"
	case errors.Is(err, ErrOpInvalid):
		return "invalid_op"
	case errors.Is(err, ErrCanvasInvalid):
		return "invalid_canvas"
	default:
		return "unknown: " + err.Error()
	}
}

func TestApplyOps_共享fixture(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixtureDir, "0*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 6 {
		t.Fatalf("共享 fixture 只找到 %d 个，期望至少 6 个（%s）", len(files), fixtureDir)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var f opsFixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("fixture 解析失败: %v", err)
			}
			a, err := Apply(f.Before, f.Ops, f.actor(t), seqIDs(t, f.NewIDs))
			if f.ExpectError != "" {
				if got := errorName(err); got != f.ExpectError {
					t.Fatalf("期望错误 %s，得到 %q（%v）", f.ExpectError, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应用失败: %v", err)
			}
			assertJSONEqual(t, "after", a.Doc, f.After)
			applied, _ := json.Marshal(a.Ops)
			assertJSONEqual(t, "applied", applied, f.Applied)
			if !reflect.DeepEqual(nonNil(a.NodeIDs), nonNil(f.NodeIDs)) {
				t.Errorf("nodeIds 得到 %v，期望 %v", a.NodeIDs, f.NodeIDs)
			}
		})
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestApplyOps_批内引用替换为真实ID(t *testing.T) {
	agent := Actor{Kind: ActorAgent, UserID: 1, SessionID: 42}
	ops := []Op{
		{Kind: OpAddNode, Ref: "ref:a", Node: json.RawMessage(`{"type":"text","title":"A"}`)},
		{Kind: OpAddNode, Ref: "ref:b", Node: json.RawMessage(`{"type":"image","title":"B"}`)},
		{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"ref:a","target":"ref:b"}`)},
		{Kind: OpUpdateNode, NodeID: "ref:b", Patch: json.RawMessage(`{"title":"B2"}`)},
	}
	a, err := Apply(nil, ops, agent, counter("a_t"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.NodeIDs, []string{"a_t1", "a_t2"}) {
		t.Fatalf("NodeIDs = %v", a.NodeIDs)
	}
	if strings.Contains(string(a.Doc), "ref:") {
		t.Errorf("落库文档里不该残留批内引用: %s", a.Doc)
	}
	applied, _ := json.Marshal(a.Ops)
	if strings.Contains(string(applied), "ref:") {
		t.Errorf("已解析的操作里不该残留批内引用（前端重放不认识它）: %s", applied)
	}
	if a.Ops[3].NodeID != "a_t2" {
		t.Errorf("update_node 的 nodeId 应换成真实 ID，得到 %q", a.Ops[3].NodeID)
	}

	// 引用未定义 / 引用了之后才创建的节点 / 重复定义：一律拒绝。
	bad := [][]Op{
		{{Kind: OpUpdateNode, NodeID: "ref:nope", Patch: json.RawMessage(`{"title":"x"}`)}},
		{
			{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"ref:a","target":"ref:a"}`)},
			{Kind: OpAddNode, Ref: "ref:a", Node: json.RawMessage(`{"type":"text"}`)},
		},
		{
			{Kind: OpAddNode, Ref: "ref:a", Node: json.RawMessage(`{"type":"text"}`)},
			{Kind: OpAddNode, Ref: "ref:a", Node: json.RawMessage(`{"type":"text"}`)},
		},
		{{Kind: OpAddNode, Ref: "plain", Node: json.RawMessage(`{"type":"text"}`)}},
	}
	for i, ops := range bad {
		if _, err := Apply(nil, ops, agent, counter("a_t")); !errors.Is(err, ErrOpInvalid) {
			t.Errorf("用例 %d：期望 ErrOpInvalid，得到 %v", i, err)
		}
	}
}

func TestApplyOps_不能改非本会话或用户改过的节点(t *testing.T) {
	doc := json.RawMessage(`{"nodes":[
		{"id":"user","type":"text","title":"u","params":{}},
		{"id":"other","type":"text","title":"o","params":{},"agent":{"sessionId":"43"}},
		{"id":"touched","type":"text","title":"t","params":{},"agent":{"sessionId":"42","userTouched":true}},
		{"id":"mine","type":"text","title":"m","params":{},"agent":{"sessionId":"42"}},
		{"id":"mineNum","type":"text","title":"m","params":{},"agent":{"sessionId":42}},
		{"id":"userFrame","type":"frame","title":"f","members":[]}
	]}`)
	agent := Actor{Kind: ActorAgent, UserID: 1, SessionID: 42}
	patch := json.RawMessage(`{"title":"x"}`)
	pos := &Point{X: 1, Y: 2}

	cases := []struct {
		name string
		op   Op
		want error
	}{
		{"用户节点 update", Op{Kind: OpUpdateNode, NodeID: "user", Patch: patch}, ErrNotOwned},
		{"别的会话 update", Op{Kind: OpUpdateNode, NodeID: "other", Patch: patch}, ErrNotOwned},
		{"用户改过 update", Op{Kind: OpUpdateNode, NodeID: "touched", Patch: patch}, ErrNotOwned},
		{"用户节点 move", Op{Kind: OpMoveNode, NodeID: "user", Position: pos}, ErrNotOwned},
		{"用户改过 move", Op{Kind: OpMoveNode, NodeID: "touched", Position: pos}, ErrNotOwned},
		{"往用户节点上接线", Op{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"mine","target":"user"}`)}, ErrNotOwned},
		{"改用户的画框", Op{Kind: OpSetFrameMembers, NodeID: "userFrame", Frame: json.RawMessage(`{"members":[]}`)}, ErrNotOwned},
		{"节点已被删", Op{Kind: OpUpdateNode, NodeID: "gone", Patch: patch}, ErrNodeNotFound},
		{"自己的节点 update", Op{Kind: OpUpdateNode, NodeID: "mine", Patch: patch}, nil},
		{"数字形态的会话 ID 也认", Op{Kind: OpMoveNode, NodeID: "mineNum", Position: pos}, nil},
		{"用户节点当输入", Op{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"user","target":"mine"}`)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Apply(doc, []Op{c.op}, agent, counter("a_x"))
			if c.want == nil {
				if err != nil {
					t.Fatalf("期望成功，得到 %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，得到 %v", c.want, err)
			}
		})
	}

	// add_node 带来的 agent 元数据不可信：会话 ID 被强制改成操作者的，userTouched 被去掉。
	a, err := Apply(nil, []Op{{Kind: OpAddNode,
		Node: json.RawMessage(`{"type":"text","agent":{"sessionId":"43","userTouched":true}}`)}},
		agent, counter("a_y"))
	if err != nil {
		t.Fatal(err)
	}
	var tree struct {
		Nodes []map[string]any `json:"nodes"`
	}
	_ = json.Unmarshal(a.Doc, &tree)
	if !OwnedBy(tree.Nodes[0], 42) {
		t.Errorf("新建节点应归操作者会话所有: %v", tree.Nodes[0]["agent"])
	}
}

func TestApplyOps_保留未知字段与大整数(t *testing.T) {
	doc := json.RawMessage(`{"schemaVersion":1,"x-top":{"k":[1,2]},"nodes":[
		{"id":"mine","type":"image","title":"m","params":{"mediaId":9007199254740993,"keep":"v"},
		 "agent":{"sessionId":"42","extra":1},"unknownNodeKey":{"deep":true}}],
		"edges":[],"tail":"t"}`)
	a, err := Apply(doc, []Op{
		{Kind: OpUpdateNode, NodeID: "mine", Patch: json.RawMessage(`{"params":{"keep":"w"}}`)},
	}, Actor{Kind: ActorAgent, SessionID: 42}, counter("a_z"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(a.Doc)
	for _, want := range []string{`"x-top":{"k":[1,2]}`, `"tail":"t"`, `"unknownNodeKey":{"deep":true}`,
		`"mediaId":9007199254740993`, `"extra":1`, `"keep":"w"`} {
		if !strings.Contains(s, want) {
			t.Errorf("结果里缺 %s: %s", want, s)
		}
	}
	// HTML 字符原样保留，不转义成 <。
	a, err = Apply(nil, []Op{{Kind: OpAddNode, Node: json.RawMessage(`{"type":"text","title":"a<b>&c"}`)}},
		Actor{Kind: ActorAgent, SessionID: 42}, counter("a_h"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.Doc), `"a<b>&c"`) {
		t.Errorf("标题被转义了: %s", a.Doc)
	}
}

func TestApplyOps_结果非法时整批拒绝(t *testing.T) {
	doc := json.RawMessage(`{"nodes":[
		{"id":"src","type":"image","title":"s","params":{}},
		{"id":"mine","type":"video","title":"m","params":{},"agent":{"sessionId":"42"}}],
		"edges":[{"id":"e1","source":"src","target":"mine","order":1}]}`)
	agent := Actor{Kind: ActorAgent, SessionID: 42}

	cases := []struct {
		name string
		ops  []Op
		want error
	}{
		{"入边 order 重复", []Op{{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"src","target":"mine","order":1}`)}}, ErrCanvasInvalid},
		{"自环", []Op{{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"mine","target":"mine"}`)}}, ErrCanvasInvalid},
		{"order 非法", []Op{{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"src","target":"mine","order":0}`)}}, ErrOpInvalid},
		{"未知种类", []Op{{Kind: "delete_node", NodeID: "src"}}, ErrOpInvalid},
		{"缺 type", []Op{{Kind: OpAddNode, Node: json.RawMessage(`{"title":"x"}`)}}, ErrOpInvalid},
		{"add_node 建画框", []Op{{Kind: OpAddNode, Node: json.RawMessage(`{"type":"frame"}`)}}, ErrOpInvalid},
		{"patch 改 type", []Op{{Kind: OpUpdateNode, NodeID: "mine", Patch: json.RawMessage(`{"type":"image"}`)}}, ErrOpInvalid},
		{"patch 改 result", []Op{{Kind: OpUpdateNode, NodeID: "mine", Patch: json.RawMessage(`{"result":{}}`)}}, ErrOpInvalid},
		{"move 缺 position", []Op{{Kind: OpMoveNode, NodeID: "mine"}}, ErrOpInvalid},
		{"画框成员不存在", []Op{{Kind: OpAddFrame, Frame: json.RawMessage(`{"members":["ghost"]}`)}}, ErrNodeNotFound},
		{"空批", nil, ErrOpInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 前面放一条合法的 add_node：整批拒绝意味着它也不生效（返回值里不该有文档）。
			ops := append([]Op{{Kind: OpAddNode, Node: json.RawMessage(`{"type":"text"}`)}}, c.ops...)
			if c.ops == nil {
				ops = nil
			}
			a, err := Apply(doc, ops, agent, counter("a_v"))
			if !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，得到 %v", c.want, err)
			}
			if a.Doc != nil || a.NodeIDs != nil {
				t.Errorf("失败时不该返回部分结果: %s %v", a.Doc, a.NodeIDs)
			}
		})
	}

	// 节点数上限同样由 ValidateCanvas 兜住。
	many := make([]Op, 0, MaxOpsPerBatch)
	for i := 0; i < MaxOpsPerBatch; i++ {
		many = append(many, Op{Kind: OpAddNode, Node: json.RawMessage(`{"type":"text"}`)})
	}
	var big bytes.Buffer
	big.WriteString(`{"nodes":[`)
	for i := 0; i < MaxNodes-100; i++ {
		if i > 0 {
			big.WriteByte(',')
		}
		big.WriteString(`{"id":"n` + strconv.Itoa(i) + `","type":"text"}`)
	}
	big.WriteString(`]}`)
	if _, err := Apply(big.Bytes(), many, agent, counter("a_m")); !errors.Is(err, ErrCanvasInvalid) {
		t.Errorf("超过节点上限应整批拒绝，得到 %v", err)
	}
	if _, err := Apply(nil, append(many, many[0]), agent, counter("a_m")); !errors.Is(err, ErrOpInvalid) {
		t.Errorf("超过一批的操作上限应拒绝，得到 %v", err)
	}
	if _, err := Apply(nil, many[:1], Actor{Kind: ActorAgent}, counter("a_m")); !errors.Is(err, ErrOpInvalid) {
		t.Errorf("Agent 操作缺会话 ID 应拒绝，得到 %v", err)
	}
}

func TestApplyOps_节点形状与前端addNode一致(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "addnode-shape.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			AddNode struct {
				NodeType string          `json:"nodeType"`
				Title    string          `json:"title"`
				Params   json.RawMessage `json:"params"`
			} `json:"addNode"`
			Node json.RawMessage `json:"node"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("addnode-shape.json 没有用例")
	}
	skip := map[string]bool{"id": true, "agent": true, "position": true}
	for _, c := range f.Cases {
		t.Run(c.AddNode.NodeType, func(t *testing.T) {
			node, _ := json.Marshal(map[string]any{
				"type": c.AddNode.NodeType, "title": c.AddNode.Title, "params": c.AddNode.Params,
			})
			a, err := Apply(nil, []Op{{Kind: OpAddNode, Node: node, Position: &Point{X: 7, Y: 8}}},
				Actor{Kind: ActorAgent, SessionID: 42}, counter("a_s"))
			if err != nil {
				t.Fatal(err)
			}
			var tree struct {
				Nodes []map[string]json.RawMessage `json:"nodes"`
			}
			if err := json.Unmarshal(a.Doc, &tree); err != nil {
				t.Fatal(err)
			}
			var want map[string]json.RawMessage
			if err := json.Unmarshal(c.Node, &want); err != nil {
				t.Fatal(err)
			}
			got := tree.Nodes[0]
			if g, w := keysExcept(got, skip), keysExcept(want, skip); !reflect.DeepEqual(g, w) {
				t.Fatalf("字段集合不一致：Agent 建的 %v，前端落库的 %v", g, w)
			}
			for _, k := range keysExcept(want, skip) {
				assertJSONEqual(t, "字段 "+k, got[k], want[k])
			}
			for _, k := range []string{"id", "position"} {
				if _, ok := got[k]; !ok {
					t.Errorf("Agent 建的节点缺 %s", k)
				}
			}
		})
	}
}

func keysExcept(m map[string]json.RawMessage, skip map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if !skip[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func TestApplyOps_LinkGeneration规则(t *testing.T) {
	doc := json.RawMessage(`{"nodes":[
		{"id":"user","type":"video","title":"u","params":{}},
		{"id":"other","type":"video","title":"o","params":{},"agent":{"sessionId":"43"}},
		{"id":"touched","type":"video","title":"t","params":{"x":1},"agent":{"sessionId":"42","userTouched":true}},
		{"id":"mine","type":"video","title":"m","params":{},"agent":{"sessionId":"42"},
		 "result":{"id":"1","status":"success","mediaId":"m1"}},
		{"id":"myFrame","type":"frame","title":"f","members":[],"agent":{"sessionId":"42"}}
	]}`)
	agent := Actor{Kind: ActorAgent, UserID: 1, SessionID: 42}
	link := func(node, patch string) Op {
		return Op{Kind: OpLinkGeneration, NodeID: node, Patch: json.RawMessage(patch)}
	}
	cases := []struct {
		name string
		op   Op
		want error
	}{
		{"用户节点", link("user", `{"generationId":"9"}`), ErrNotOwned},
		{"别的会话", link("other", `{"generationId":"9"}`), ErrNotOwned},
		{"节点已被删", link("gone", `{"generationId":"9"}`), ErrNodeNotFound},
		{"画框", link("myFrame", `{"generationId":"9"}`), ErrOpInvalid},
		{"缺 generationId", link("mine", `{}`), ErrOpInvalid},
		{"数字形态不收", link("mine", `{"generationId":9}`), ErrOpInvalid},
		{"零", link("mine", `{"generationId":"0"}`), ErrOpInvalid},
		{"非数字", link("mine", `{"generationId":"abc"}`), ErrOpInvalid},
		{"越界", link("mine", `{"generationId":"99999999999999999999"}`), ErrOpInvalid},
		{"夹带别的键", link("mine", `{"generationId":"9","status":"success"}`), ErrOpInvalid},
		{"缺 patch", Op{Kind: OpLinkGeneration, NodeID: "mine"}, ErrOpInvalid},
		{"用户改过也挂（只写事实不碰内容）", link("touched", `{"generationId":"9"}`), nil},
		{"自己的节点", link("mine", `{"generationId":"9"}`), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Apply(doc, []Op{c.op}, agent, counter("a_x"))
			if c.want == nil {
				if err != nil {
					t.Fatalf("期望成功，得到 %v", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，得到 %v", c.want, err)
			}
		})
	}

	// 用户改过的节点挂上任务后仍是「用户改过」，Agent 之后照样改不了它的内容。
	a, err := Apply(doc, []Op{link("touched", `{"generationId":"9"}`)}, agent, counter("a_y"))
	if err != nil {
		t.Fatal(err)
	}
	var tree struct {
		Nodes []map[string]any `json:"nodes"`
	}
	_ = json.Unmarshal(a.Doc, &tree)
	n := tree.Nodes[2]
	if OwnedBy(n, 42) {
		t.Error("link_generation 不能清掉 userTouched")
	}
	if r, _ := n["result"].(map[string]any); r["id"] != "9" || r["status"] != "queued" {
		t.Errorf("result 应为 queued 的新任务：%v", n["result"])
	}
	if p, _ := n["params"].(map[string]any); p["x"] != float64(1) {
		t.Errorf("参数不应被动：%v", n["params"])
	}
	// 用户（开发工具）可以挂任意节点。
	if _, err := Apply(doc, []Op{link("user", `{"generationId":"9"}`)}, Actor{Kind: ActorUser, UserID: 1},
		counter("a_z")); err != nil {
		t.Errorf("用户操作者不受会话所有权限制：%v", err)
	}
}

// ---------------------------------------------------------------- 来源泛化（MCPT-09）

func TestNodeOrigin_新旧字段兼容与userTouched取或(t *testing.T) {
	node := func(raw string) map[string]any {
		v, err := decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return v.(map[string]any)
	}
	cases := []struct {
		name string
		raw  string
		want Origin
		ok   bool
	}{
		{"没有来源", `{"id":"u"}`, Origin{}, false},
		{"坏值当没有", `{"agent":"x","origin":[1]}`, Origin{}, false},
		{"只有旧 agent", `{"agent":{"sessionId":"42","stepId":"s"}}`, Origin{Kind: ActorAgent, SessionID: "42"}, true},
		{"旧 agent 数字形态", `{"agent":{"sessionId":42}}`, Origin{Kind: ActorAgent, SessionID: "42"}, true},
		{"只有 origin", `{"origin":{"kind":"mcp","grantId":"5001","clientName":"Claude Code"}}`,
			Origin{Kind: ActorMCP, GrantID: "5001", ClientName: "Claude Code"}, true},
		{"origin 表达 agent 与旧字段一致", `{"origin":{"kind":"agent","sessionId":"42"}}`,
			Origin{Kind: ActorAgent, SessionID: "42"}, true},
		{"两个都在以 origin 为准", `{"agent":{"sessionId":"42"},"origin":{"kind":"mcp","grantId":"5001"}}`,
			Origin{Kind: ActorMCP, GrantID: "5001"}, true},
		{"旧字段标过 → touched", `{"agent":{"sessionId":"42","userTouched":true},"origin":{"kind":"mcp","grantId":"5001"}}`,
			Origin{Kind: ActorMCP, GrantID: "5001", UserTouched: true}, true},
		{"新字段标过 → touched", `{"agent":{"sessionId":"42"},"origin":{"kind":"agent","sessionId":"42","userTouched":true}}`,
			Origin{Kind: ActorAgent, SessionID: "42", UserTouched: true}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := NodeOrigin(node(c.raw))
			if ok != c.ok || got != c.want {
				t.Fatalf("得到 %+v,%v，期望 %+v,%v", got, ok, c.want, c.ok)
			}
		})
	}
	// 三种形态在所有权判定上一致：旧 agent / origin 表达的 agent 都归会话 42。
	agent := Actor{Kind: ActorAgent, SessionID: 42}
	for _, raw := range []string{`{"agent":{"sessionId":"42"}}`, `{"origin":{"kind":"agent","sessionId":"42"}}`,
		`{"agent":{"sessionId":"42"},"origin":{"kind":"agent","sessionId":"42"}}`} {
		if !OwnedByActor(node(raw), agent) {
			t.Errorf("%s 应归会话 42 所有", raw)
		}
	}
	// 两个都在且任一处标过 userTouched：谁都不能再改。
	if OwnedByActor(node(`{"agent":{"sessionId":"42","userTouched":true},"origin":{"kind":"agent","sessionId":"42"}}`), agent) {
		t.Error("旧字段标过 userTouched 的节点不能被重新判成可改")
	}
}

func TestApplyOps_MCP操作必须带授权ID(t *testing.T) {
	ops := []Op{{Kind: OpAddNode, Node: json.RawMessage(`{"type":"text"}`)}}
	if _, err := Apply(nil, ops, Actor{Kind: ActorMCP, UserID: 1}, counter("a_g")); !errors.Is(err, ErrOpInvalid) {
		t.Fatalf("没有授权 ID 应拒绝，得到 %v", err)
	}
	if _, err := Apply(nil, ops, Actor{Kind: ActorMCP, UserID: 1, GrantID: -1}, counter("a_g")); !errors.Is(err, ErrOpInvalid) {
		t.Fatalf("授权 ID 非正应拒绝，得到 %v", err)
	}
	if _, err := Apply(nil, ops, Actor{Kind: ActorMCP, UserID: 1, GrantID: 5001}, counter("a_g")); err != nil {
		t.Fatalf("带授权 ID 应成功：%v", err)
	}
	if _, err := Apply(nil, ops, Actor{Kind: "robot", UserID: 1}, counter("a_g")); !errors.Is(err, ErrOpInvalid) {
		t.Fatalf("未知操作者种类应拒绝，得到 %v", err)
	}
}

func TestApplyOps_MCP不能伪造来源(t *testing.T) {
	forged := json.RawMessage(`{"type":"text","agent":{"sessionId":"42","stepId":"s"},
		"origin":{"kind":"mcp","grantId":"9","clientName":"冒充","userTouched":true}}`)
	addOne := func(actor Actor) map[string]any {
		t.Helper()
		a, err := Apply(nil, []Op{{Kind: OpAddNode, Node: forged}}, actor, counter("a_f"))
		if err != nil {
			t.Fatal(err)
		}
		var tree struct {
			Nodes []map[string]any `json:"nodes"`
		}
		if err := json.Unmarshal(a.Doc, &tree); err != nil {
			t.Fatal(err)
		}
		return tree.Nodes[0]
	}

	mcp := Actor{Kind: ActorMCP, UserID: 1, GrantID: 5001, ClientName: "Claude Code"}
	n := addOne(mcp)
	if _, has := n["agent"]; has {
		t.Errorf("MCP 建的节点不该带 agent：%v", n)
	}
	want := map[string]any{"kind": "mcp", "grantId": "5001", "clientName": "Claude Code"}
	if !reflect.DeepEqual(n["origin"], want) {
		t.Errorf("origin 应由服务端强写为 %v，得到 %v", want, n["origin"])
	}
	// 伪造的会话 42 拿不到这个节点；伪造的授权 9 也拿不到。
	if CreatedByActor(n, Actor{Kind: ActorAgent, SessionID: 42}) || CreatedByActor(n, Actor{Kind: ActorMCP, GrantID: 9}) {
		t.Error("伪造的来源不能生效")
	}
	// 客户端名为空时不写 clientName（不编造）。
	if o := addOne(Actor{Kind: ActorMCP, UserID: 1, GrantID: 5001})["origin"].(map[string]any); o["clientName"] != nil {
		t.Errorf("没有客户端名时不该写 clientName：%v", o)
	}

	// Agent 新建节点：保留原有行为（只写 agent、保留 stepId），额外剥掉 origin。
	n = addOne(Actor{Kind: ActorAgent, UserID: 1, SessionID: 7})
	if _, has := n["origin"]; has {
		t.Errorf("Agent 建的节点不该带 origin：%v", n)
	}
	if !reflect.DeepEqual(n["agent"], map[string]any{"sessionId": "7", "stepId": "s"}) {
		t.Errorf("Agent 元数据得到 %v", n["agent"])
	}

	// user：两种来源都剥。
	n = addOne(Actor{Kind: ActorUser, UserID: 1})
	if _, has := n["agent"]; has {
		t.Errorf("user 建的节点不该带 agent：%v", n)
	}
	if _, has := n["origin"]; has {
		t.Errorf("user 建的节点不该带 origin：%v", n)
	}
}

func TestApplyOps_保留origin里的未知字段(t *testing.T) {
	doc := json.RawMessage(`{"nodes":[
		{"id":"a_m1","type":"text","title":"t","params":{},
		 "origin":{"kind":"mcp","grantId":"5001","clientName":"Claude Code","future":{"n":9007199254740993}}}]}`)
	mcp := Actor{Kind: ActorMCP, UserID: 1, GrantID: 5001}
	a, err := Apply(doc, []Op{
		{Kind: OpUpdateNode, NodeID: "a_m1", Patch: json.RawMessage(`{"title":"新"}`)},
		{Kind: OpMoveNode, NodeID: "a_m1", Position: &Point{X: 1, Y: 2}},
		{Kind: OpLinkGeneration, NodeID: "a_m1", Patch: json.RawMessage(`{"generationId":"9"}`)},
	}, mcp, counter("a_u"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.Doc),
		`"origin":{"clientName":"Claude Code","future":{"n":9007199254740993},"grantId":"5001","kind":"mcp"}`) {
		t.Errorf("origin 应逐键原样保留（含大整数）：%s", a.Doc)
	}
	// update_node 不能改来源字段。
	for _, k := range []string{"origin", "agent"} {
		_, err := Apply(doc, []Op{{Kind: OpUpdateNode, NodeID: "a_m1",
			Patch: json.RawMessage(`{"` + k + `":{"kind":"mcp","grantId":"1"}}`)}}, mcp, counter("a_u"))
		if !errors.Is(err, ErrOpInvalid) {
			t.Errorf("patch.%s 应被拒绝，得到 %v", k, err)
		}
	}
}

func TestApplyOps_MCP与Agent互不可改(t *testing.T) {
	doc := json.RawMessage(`{"nodes":[
		{"id":"ag","type":"text","title":"a","params":{},"agent":{"sessionId":"42"}},
		{"id":"mc","type":"text","title":"m","params":{},"origin":{"kind":"mcp","grantId":"5001"}},
		{"id":"mc2","type":"text","title":"m2","params":{},"origin":{"kind":"mcp","grantId":"5002"}}]}`)
	agent := Actor{Kind: ActorAgent, UserID: 1, SessionID: 42}
	mcp := Actor{Kind: ActorMCP, UserID: 1, GrantID: 5001}
	upd := func(id string) []Op {
		return []Op{{Kind: OpUpdateNode, NodeID: id, Patch: json.RawMessage(`{"title":"x"}`)}}
	}
	edge := func(src, dst string) []Op {
		return []Op{{Kind: OpAddEdge, Edge: json.RawMessage(`{"source":"` + src + `","target":"` + dst + `"}`)}}
	}
	cases := []struct {
		name  string
		actor Actor
		ops   []Op
		want  error
	}{
		{"Agent 改自己的", agent, upd("ag"), nil},
		{"Agent 改 MCP 的", agent, upd("mc"), ErrNotOwned},
		{"MCP 改自己的", mcp, upd("mc"), nil},
		{"MCP 改 Agent 的", mcp, upd("ag"), ErrNotOwned},
		{"MCP 改别的授权的", mcp, upd("mc2"), ErrNotOwned},
		{"MCP 拿 Agent 的节点当输入", mcp, edge("ag", "mc"), nil},
		{"MCP 往 Agent 的节点上接线", mcp, edge("mc", "ag"), ErrNotOwned},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Apply(doc, c.ops, c.actor, counter("a_c"))
			if c.want == nil && err != nil {
				t.Fatalf("期望成功，得到 %v", err)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("期望 %v，得到 %v", c.want, err)
			}
		})
	}
}
