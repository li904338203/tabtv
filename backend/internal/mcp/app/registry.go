// Package app 是 MCP 域的编排层：工具注册表、调用主体与本次请求的可用范围。
//
// ⚠️ 本包**不认识 go-sdk**：SDK 适配在 transport。注册表是运行时与工具快照
// （api/mcp/tools.json）的**同一个来源**（MT-8、MCP-N-10）——快照另写一份的话，
// 「文档里有、线上没有」只会在用户调用时才暴露。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	collab "github.com/li904338203/tabtv/backend/internal/collaboration/domain"
	"github.com/li904338203/tabtv/backend/internal/mcp/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// MethodParam 是多 method 工具的方法参数名（MCP-F-21：同类操作合并为一个工具，用 method 区分）。
const MethodParam = "method"

// Method 是工具的一个操作。**动作 / 是否花钱 / 是否写挂在 method 上**：
// `projects.list` 只读而 `projects.create` 写，挂在工具上的话只能二选一，必然有一边标错。
type Method struct {
	// Name 为空表示工具没有 method 参数（只允许单 method 的工具这样写）。
	Name string
	// Action 是所需的工作区动作，决定所需 scope（domain.ScopeFor）。必填。
	Action collab.Action
	// Paid 会花积分（走确认服务，MCPT-10）。只能配 generate / export 动作。
	Paid bool
	// Write 会改数据（建项目、写画布……）。不能配 view 动作。
	Write bool
	// Destructive 会删除或覆盖用户已有的东西（注解 destructiveHint）。目前没有工具需要它。
	Destructive bool
	// OpenWorld 不花钱、但会调用外部模型上游（注解 openWorldHint）：3D 导演台的 AI 搭景（DRT-29）。
	// Paid 本身就意味着调用上游，不必再标。
	OpenWorld bool
}

// ReadOnly 是否只读：不写、不花钱、只要 view（规则在 domain.MethodSpec.ReadOnly，只有一份）。
func (m Method) ReadOnly() bool { return m.Spec("").ReadOnly() }

// Spec 是可见性判定（domain.Visible）需要的 method 事实。
func (m Method) Spec(toolset string) domain.MethodSpec {
	return domain.MethodSpec{Toolset: toolset, Action: m.Action, Paid: m.Paid, Write: m.Write}
}

// Call 是一次已校验过入参的调用。Input 是指向工具入参类型的指针。
type Call struct {
	Method string
	Input  any
}

// Output 是工具的结果：Data 序列化为 structuredContent（并在文本里给完整 JSON），
// Summary 是给人看的一句话摘要（MCP-I-03）。
type Output struct {
	Data    any
	Summary string
	// Images 以 image content 内联返回（只给小图：media.get 的 320px 缩略图，MCP-F-54）。
	// ⚠️ 视频 / 音频**不得**内联（base64 后几十 MB 直接撑爆客户端上下文），给签名直链。
	Images []Image
	// Links 以 resource_link 内容返回（MCPT-16，MCP-F-36）：指向本平台资源（画布大纲 / 文档 / 媒体）的 URI，
	// 客户端可再用 resources/read 读取。不进 structuredContent 与 schema。
	Links []ResourceLink
	// Elicit 非空表示本次不给结果，而是请用户在 elicitation 表单里确认（MCPT-10）。
	// transport 把它翻成 InputRequests + RequestState，此时 Data / Summary / Images 一律不发
	// （协议规定 input_required 的结果不能带内容）。
	Elicit *ElicitRequest
}

// Image 是一张内联图片。
type Image struct {
	MIMEType string
	Data     []byte
}

// Handler 执行一次调用。返回的 error 若是 *httpx.BizError，业务码与文案会原样交给客户端（MCP-I-06）。
type Handler func(ctx context.Context, call Call) (Output, error)

// Annotations 是由 method 集合**推导**出的工具注解（MCP-I-04），不允许手写 ——
// 手写的话加了一个写 method 而忘了改注解，客户端会把一个会改数据的工具当成只读的自动放行。
type Annotations struct {
	ReadOnlyHint    bool  `json:"readOnlyHint"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  bool  `json:"idempotentHint"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// Tool 是注册表里的一个工具。In / Out 是入参与出参类型的零值（必须是结构体），
// schema 由它们推导；推荐用 Define 构造，handler 拿到的就是强类型入参。
type Tool struct {
	Name        string
	Toolset     string
	Description string
	Methods     []Method
	In, Out     any
	Handler     Handler

	inType       reflect.Type
	inputSchema  *jsonschema.Schema
	outputSchema *jsonschema.Schema
	inResolved   *jsonschema.Resolved
	outResolved  *jsonschema.Resolved
	annotations  Annotations
}

// Define 用强类型 handler 构造工具：In / Out 由类型参数给出，handler 返回出参与一句摘要。
func Define[In, Out any](t Tool, h func(ctx context.Context, method string, in In) (Out, string, error)) Tool {
	var zin In
	var zout Out
	t.In, t.Out = zin, zout
	t.Handler = func(ctx context.Context, c Call) (Output, error) {
		in, _ := c.Input.(*In)
		if in == nil {
			in = new(In)
		}
		out, summary, err := h(ctx, c.Method, *in)
		if err != nil {
			return Output{}, err
		}
		o := Output{Data: out, Summary: summary}
		if ic, ok := any(out).(ImageCarrier); ok {
			o.Images = ic.InlineImages()
		}
		if lc, ok := any(out).(LinkCarrier); ok {
			o.Links = lc.ResourceLinks()
		}
		if ec, ok := any(out).(ElicitCarrier); ok {
			o.Elicit = ec.ElicitRequest()
		}
		return o, nil
	}
	return t
}

// ResourceLink 是一条指向本平台资源的链接（seeaitv://…）。
type ResourceLink struct {
	URI      string
	Name     string
	Title    string
	MIMEType string
}

// LinkCarrier 由需要附带 resource_link 的出参类型实现（链接字段标 `json:"-"`）。
type LinkCarrier interface {
	ResourceLinks() []ResourceLink
}

// ImageCarrier 由需要内联图片的出参类型实现（图片字段标 `json:"-"`，不进 structuredContent 与 schema）。
type ImageCarrier interface {
	InlineImages() []Image
}

// InputSchema 返回入参 schema（多 method 工具已注入 method 枚举）。
func (t *Tool) InputSchema() *jsonschema.Schema { return t.inputSchema }

// OutputSchema 返回出参 schema（已加入截断的两个可选字段）。
func (t *Tool) OutputSchema() *jsonschema.Schema { return t.outputSchema }

// Annotations 返回推导出的注解。
func (t *Tool) Annotations() Annotations { return t.annotations }

// HasMethodParam 是否需要 method 参数。
func (t *Tool) HasMethodParam() bool { return len(t.Methods) > 1 || t.Methods[0].Name != "" }

// Method 按名字取 method；无 method 参数的工具传空串。
func (t *Tool) Method(name string) (Method, bool) {
	for _, m := range t.Methods {
		if m.Name == name {
			return m, true
		}
	}
	return Method{}, false
}

// HasReadOnlyMethod 是否至少有一个只读 method（只读端点据此决定是否列出）。
func (t *Tool) HasReadOnlyMethod() bool {
	return slices.ContainsFunc(t.Methods, Method.ReadOnly)
}

// PeekMethod 在完整校验之前取出 arguments 里点名的 method（无 method 参数的工具返回空串）。
// 只给闸门用：被拒的调用不该先因为参数不合法拿到一句「arguments 不符合 inputSchema」——
// 那会让模型去修参数，而参数改成什么样这个调用都不会被放行。
func (t *Tool) PeekMethod(args json.RawMessage) string {
	if !t.HasMethodParam() {
		return ""
	}
	var obj struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(args, &obj)
	return obj.Method
}

// Decode 按 inputSchema 校验入参并解码成 Call。失败返回参数错误（40001），文案说清哪里不对。
func (t *Tool) Decode(args json.RawMessage) (Call, error) {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	var generic any
	if err := json.Unmarshal(args, &generic); err != nil {
		return Call{}, httpx.Errf(errcode.InvalidArgument, "arguments 不是合法 JSON："+err.Error())
	}
	if err := t.inResolved.Validate(generic); err != nil {
		return Call{}, httpx.Errf(errcode.InvalidArgument, "arguments 不符合 inputSchema："+err.Error())
	}
	method := ""
	if t.HasMethodParam() {
		obj, _ := generic.(map[string]any)
		method, _ = obj[MethodParam].(string)
	}
	in := reflect.New(t.inType)
	if err := json.Unmarshal(args, in.Interface()); err != nil {
		return Call{}, httpx.Errf(errcode.InvalidArgument, "arguments 解码失败："+err.Error())
	}
	return Call{Method: method, Input: in.Interface()}, nil
}

// ValidateOutput 按 outputSchema 校验（截断之后的）结果。
// 失败说明工具的 DTO 与声明对不上 —— 是代码缺陷，不是用户输入问题。
func (t *Tool) ValidateOutput(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	return t.outResolved.Validate(v)
}

// Registry 是工具注册表。注册期出错一律 panic —— 那是代码写错了，进程起来之前就该暴露。
type Registry struct {
	tools map[string]*Tool
	// resources / prompts（MCPT-16，见 extras.go）。
	resources []ResourceTemplate
	prompts   PromptSource
}

// NewRegistry 建空注册表。业务工具的装配在 internal/mcp/tools.NewRegistry。
func NewRegistry() *Registry { return &Registry{tools: map[string]*Tool{}} }

var toolNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Register 校验并登记工具。
func (r *Registry) Register(t Tool) {
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("mcp 注册工具 %q：%s", t.Name, fmt.Sprintf(format, args...)))
	}
	if !toolNameRe.MatchString(t.Name) {
		fail("名字必须是小写字母开头的 snake_case（≤64）")
	}
	if _, dup := r.tools[t.Name]; dup {
		fail("重名")
	}
	if !domain.IsToolset(t.Toolset) {
		fail("未知工具集 %q", t.Toolset)
	}
	if t.Handler == nil {
		fail("缺 handler")
	}
	if strings.TrimSpace(t.Description) == "" {
		fail("缺描述（模型靠它决定何时调用）")
	}
	if len(t.Methods) == 0 {
		fail("至少要有一个 method")
	}
	seen := map[string]bool{}
	for _, m := range t.Methods {
		if m.Action == "" {
			fail("method %q 必须声明动作", m.Name)
		}
		if _, ok := domain.ScopeFor(m.Action); !ok {
			fail("method %q 的动作 %q 没有对应 scope（MCP 只开放 view/edit/generate/export）", m.Name, m.Action)
		}
		if m.Write && m.Action == collab.ActionView {
			fail("method %q 会写数据却只要 view", m.Name)
		}
		if m.Paid && m.Action != collab.ActionGenerate && m.Action != collab.ActionExport {
			fail("method %q 花钱却不要 generate / export", m.Name)
		}
		if len(t.Methods) > 1 && m.Name == "" {
			fail("多 method 工具的每个 method 都要有名字")
		}
		if m.Name != "" && !toolNameRe.MatchString(m.Name) {
			fail("method 名 %q 必须是 snake_case", m.Name)
		}
		if seen[m.Name] {
			fail("method %q 重名", m.Name)
		}
		seen[m.Name] = true
	}

	inType := structType(t.In)
	if inType == nil {
		fail("In 必须是结构体")
	}
	if structType(t.Out) == nil {
		fail("Out 必须是结构体（MCP-I-03：每个工具都要有 outputSchema）")
	}
	in, err := jsonschema.ForType(inType, nil)
	if err != nil {
		fail("推导 inputSchema：%v", err)
	}
	if t.HasMethodParam() {
		injectMethod(in, t.Methods)
	}
	out, err := jsonschema.ForType(structType(t.Out), nil)
	if err != nil {
		fail("推导 outputSchema：%v", err)
	}
	if err := injectTruncation(out); err != nil {
		fail("%v", err)
	}
	inRes, err := in.Resolve(nil)
	if err != nil {
		fail("inputSchema 无法解析：%v", err)
	}
	outRes, err := out.Resolve(nil)
	if err != nil {
		fail("outputSchema 无法解析：%v", err)
	}
	t.inType, t.inputSchema, t.outputSchema = inType, in, out
	t.inResolved, t.outResolved = inRes, outRes
	t.annotations = deriveAnnotations(t.Methods)
	r.tools[t.Name] = &t
}

// List 按名字排序返回全部工具（快照与 tools/list 的顺序都由它决定）。
func (r *Registry) List() []*Tool {
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup 按名字取工具。
func (r *Registry) Lookup(name string) (*Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func structType(v any) reflect.Type {
	if v == nil {
		return nil
	}
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}

func injectMethod(s *jsonschema.Schema, methods []Method) {
	if s.Properties == nil {
		s.Properties = map[string]*jsonschema.Schema{}
	}
	s.Properties[MethodParam] = methodProp(methods)
	s.Required = append([]string{MethodParam}, slices.DeleteFunc(slices.Clone(s.Required),
		func(n string) bool { return n == MethodParam })...)
	s.PropertyOrder = append([]string{MethodParam}, slices.DeleteFunc(slices.Clone(s.PropertyOrder),
		func(n string) bool { return n == MethodParam })...)
}

// methodProp 是 method 参数的 schema：枚举 + 一句说明，顺序同注册顺序。
func methodProp(methods []Method) *jsonschema.Schema {
	names := make([]any, 0, len(methods))
	labels := make([]string, 0, len(methods))
	for _, m := range methods {
		names = append(names, m.Name)
		labels = append(labels, m.Name)
	}
	return &jsonschema.Schema{Type: "string", Enum: names, Description: "操作：" + strings.Join(labels, " / ")}
}

// InputSchemaFor 返回只含 ms 这几个 method 的入参 schema（tools/list 按本次请求收窄 method 枚举用）。
// ms 覆盖全部 method 或工具没有 method 参数时返回原 schema。
//
// ⚠️ 返回的是**副本**：注册表里的 schema 被 SDK 与全部请求共用，原地改枚举的话一个只读端点的请求
// 会把所有人的工具定义一起改掉。只复制被改的那两层（顶层与 Properties），其余属性共享、不得修改。
func (t *Tool) InputSchemaFor(ms []Method) *jsonschema.Schema {
	if !t.HasMethodParam() || len(ms) == len(t.Methods) {
		return t.inputSchema
	}
	cp := *t.inputSchema
	cp.Properties = make(map[string]*jsonschema.Schema, len(t.inputSchema.Properties))
	for k, v := range t.inputSchema.Properties {
		cp.Properties[k] = v
	}
	cp.Properties[MethodParam] = methodProp(ms)
	return &cp
}

// AnnotationsFor 按一组 method 推导注解（规则同注册时的 deriveAnnotations）。
// 只读端点把混合工具收窄成只剩读 method 时，注解也要跟着变成只读 —— 否则客户端看到的是
// 「这个工具会写数据」，而它在这个端点上其实什么都写不了。注解只是提示，闸门仍在 Check。
func AnnotationsFor(ms []Method) Annotations { return deriveAnnotations(ms) }

// injectTruncation 把截断字段加进出参 schema（可选属性）。出参类型自己占用了这两个名字时报错。
func injectTruncation(s *jsonschema.Schema) error {
	if s.Type != "object" {
		return fmt.Errorf("outputSchema 必须是 object")
	}
	if s.Properties == nil {
		s.Properties = map[string]*jsonschema.Schema{}
	}
	for _, f := range []string{domain.FieldTruncated, domain.FieldTruncationHint} {
		if _, taken := s.Properties[f]; taken {
			return fmt.Errorf("出参字段 %q 与截断标记冲突", f)
		}
	}
	s.Properties[domain.FieldTruncated] = &jsonschema.Schema{Type: "boolean",
		Description: "结果过长已被截断（只在截断时出现）"}
	s.Properties[domain.FieldTruncationHint] = &jsonschema.Schema{Type: "string",
		Description: "被截断的数组与精确读取方式（只在截断时出现）"}
	return nil
}

// deriveAnnotations 由 method 集合推导注解（MCP-I-04）：
//   - 全部只读 → readOnlyHint=true（openWorld=false：只读本平台的数据）；
//   - 否则 destructiveHint 取「任一 method 会删改已有内容」；
//   - 含花钱 → openWorldHint=true（会调用外部模型上游）、idempotentHint=true
//     （花钱调用一律带幂等键，同一确认重试不会再扣一次）；
//   - 不花钱但调用外部模型（Method.OpenWorld，如 AI 搭景）→ openWorldHint=true；
//   - 不花钱的写 → idempotentHint=false（建项目、写画布重复调用会多出一份）。
func deriveAnnotations(ms []Method) Annotations {
	allRead, anyPaid, anyDestructive, allWritesPaid, anyOpenWorld := true, false, false, true, false
	for _, m := range ms {
		if !m.ReadOnly() {
			allRead = false
			if !m.Paid {
				allWritesPaid = false
			}
		}
		anyPaid = anyPaid || m.Paid
		anyDestructive = anyDestructive || m.Destructive
		anyOpenWorld = anyOpenWorld || m.OpenWorld
	}
	if allRead {
		return Annotations{ReadOnlyHint: true, OpenWorldHint: ptr(anyOpenWorld)}
	}
	return Annotations{
		DestructiveHint: ptr(anyDestructive),
		IdempotentHint:  anyPaid && allWritesPaid,
		OpenWorldHint:   ptr(anyPaid || anyOpenWorld),
	}
}

func ptr[T any](v T) *T { return &v }

func scopeOf(m Method) (string, bool) { return domain.ScopeFor(m.Action) }
