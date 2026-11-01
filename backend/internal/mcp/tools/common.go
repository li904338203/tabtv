package tools

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/mcp/app"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 工具共用的小函数：主体、ID、时间、游标、文本上限。
//
// ⚠️ ID 对外一律字符串（GO-25、开发 01 §12 R20 ⑤）：DTO 里写 int64+`,string` 的话 schema 推断成
// integer，入参与出参校验两头都失败。时间一律 UTC RFC3339。

// 用户文本的上限（码点）。工具结果整体还有截断层（domain.Truncate），但它只删数组元素、
// 不切字符串 —— 一个几万字的标题或文档会让截断层无能为力，所以单个字段先在这里封顶。
const (
	maxTitleRunes   = 200
	maxParamsRunes  = 3000
	maxDocRunes     = 12000
	maxErrorRunes   = 500
	maxNodeIDs      = 50
	defaultPageSize = 20
	maxPageSize     = 50
)

// principal 取本次调用的主体。取不到是装配错误（凭据中间件没挂），不是用户输入问题。
func principal(ctx context.Context) (app.Principal, error) {
	p, ok := app.PrincipalFrom(ctx)
	if !ok || p.WorkspaceID == 0 {
		return app.Principal{}, httpx.Errf(errcode.Unauthenticated, "缺少 MCP 调用主体")
	}
	return p, nil
}

func idStr(v int64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatInt(v, 10)
}

// parseID 解析对外的字符串 ID；name 用于错误文案（说清是哪个参数）。
func parseID(name, s string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v <= 0 {
		return 0, httpx.Errf(errcode.InvalidArgument, name+" 必须是正整数 ID 的字符串形式（如 \"1234567890\"）")
	}
	return v, nil
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func tsPtr(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	s := ts(*t)
	return &s
}

func pageSize(n int) int {
	if n <= 0 {
		return defaultPageSize
	}
	if n > maxPageSize {
		return maxPageSize
	}
	return n
}

// cappedParams 把节点 params 转成可输出的值；超长时给前缀 + 截断标记（与 Agent 的 read_canvas 同一做法），
// 保证结果仍是合法 JSON 而不是半截对象。
func cappedParams(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if utf8.RuneCount(raw) > maxParamsRunes {
		r := []rune(string(raw))
		return map[string]any{"_truncated": true, "_prefix": string(r[:maxParamsRunes])}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return v
}

// 任务游标：统一状态列表按 (createdAt, id) 倒序翻页，对外编成一个不透明串。
func encodeTaskCursor(at time.Time, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(id, 10)))
}

func decodeTaskCursor(s string) (*time.Time, int64, error) {
	bad := httpx.Errf(errcode.InvalidArgument, "cursor 非法：请原样传入上一页返回的 nextCursor")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, 0, bad
	}
	at, idPart, ok := strings.Cut(string(b), "|")
	if !ok {
		return nil, 0, bad
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, 0, bad
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return nil, 0, bad
	}
	return &t, id, nil
}

// randomSuffix 是 16 字节随机串（hex）：不带幂等键的写操作用它当一次性批次。
func randomSuffix() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
