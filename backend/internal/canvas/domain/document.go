package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------- 项目文档（AGT-09）

// MaxDocBytes 单份文档正文上限（字节）。
//
// ⚠️ 这是**设计建议值**，不是已批准的容量参数（同 PARAM-05 一类）：
// 256 KiB 足够放一份几百镜的分镜表；再大就该拆成多份，而不是让一次保存
// 把一整本书塞进一行 text、每次打开节点都整份取回来。
const MaxDocBytes = 256 << 10

// MaxDocPathLen 与 `project_document.path varchar(200)` 一致（按字符算）。
const MaxDocPathLen = 200

// NodeTypeDocument 是文档节点的存储类型（`canvas_data.nodes[].type`）。
const NodeTypeDocument = "document"

var (
	ErrDocPathInvalid = errors.New("文档路径不合法")
	ErrDocTooLarge    = errors.New("文档正文过大")
)

// 固定路径：Agent 约定的几份文档。其余一律放在 /notes/ 下。
var fixedDocPaths = map[string]bool{
	"/plan.md":    true,
	"/shots.md":   true,
	"/.memory.md": true,
}

// ValidateDocPath 校验文档路径并返回规范化后的值。
//
// 白名单：`/plan.md`、`/shots.md`、`/.memory.md`、`/notes/<名字>.md`（只一层）。
//
// ⚠️ 为什么是白名单而不是「任意以 .md 结尾的路径」：路径是 Agent 与人共用的命名空间，
// Agent 按固定路径读写规划与记忆。放开任意路径的话，`/plan.md` 与 `/Plan.md`、
// `//plan.md`、`/a/../plan.md` 会是几份不同的文档 —— Agent 读到的不是用户改过的那份，
// 而两边都"没错"。`..` 与空段在这里直接拒绝，而不是规范化后放行：
// 规范化放行等于承认它们是合法写法，下一处拼路径的代码就会照着写。
func ValidateDocPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("%w：不能为空", ErrDocPathInvalid)
	}
	if n := utf8.RuneCountInString(p); n > MaxDocPathLen {
		return "", fmt.Errorf("%w：%d 字，上限 %d 字", ErrDocPathInvalid, n, MaxDocPathLen)
	}
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%w：不是合法的 UTF-8", ErrDocPathInvalid)
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%w：必须以 / 开头", ErrDocPathInvalid)
	}
	segs := strings.Split(p[1:], "/")
	for _, s := range segs {
		switch {
		case s == "":
			return "", fmt.Errorf("%w：含空段", ErrDocPathInvalid)
		case s == "." || s == "..":
			return "", fmt.Errorf("%w：不允许 . 或 ..", ErrDocPathInvalid)
		}
		for _, r := range s {
			// 控制字符与反斜杠：前者进日志/界面会出怪字，后者在部分工具里被当成分隔符。
			if unicode.IsControl(r) || r == '\\' {
				return "", fmt.Errorf("%w：含非法字符", ErrDocPathInvalid)
			}
		}
	}
	if fixedDocPaths[p] {
		return p, nil
	}
	if len(segs) == 2 && segs[0] == "notes" && strings.HasSuffix(segs[1], ".md") &&
		strings.TrimSuffix(segs[1], ".md") != "" {
		return p, nil
	}
	return "", fmt.Errorf("%w：只允许 /plan.md、/shots.md、/.memory.md 或 /notes/<名字>.md", ErrDocPathInvalid)
}

// ValidateDocContent 校验正文体积（按字节，与存储一致）。
func ValidateDocContent(content string) error {
	if n := len(content); n > MaxDocBytes {
		return fmt.Errorf("%w：%d 字节，上限 %d 字节", ErrDocTooLarge, n, MaxDocBytes)
	}
	if !utf8.ValidString(content) {
		return errors.New("文档正文不是合法的 UTF-8")
	}
	return nil
}

// RebindDocIDs 把副本画布里文档节点的 `params.docId` 改写成副本自己的文档 ID。
//
// ⚠️ 只改 `type=="document"` 的节点：别的节点 params 里恰好有个 docId 键
// （将来某个节点类型复用了这个名字）不该被顺手改掉。
// ⚠️ 不在映射里的 docId **原样保留**（源项目里指向已删文档的节点）：
// 文档查询按 project_id 过滤，它在副本里只会显示"文档不存在"，不会读到源项目的内容。
// 走通用 map 树，未知字段原样保留（GO-35，同 CopyCanvasData）。
//
// 返回改写了几个节点。
func RebindDocIDs(raw json.RawMessage, ids map[int64]int64) (json.RawMessage, int, error) {
	if len(raw) == 0 || len(ids) == 0 {
		return raw, 0, nil
	}
	var tree map[string]any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, 0, fmt.Errorf("画布数据不是合法 JSON 对象: %w", err)
	}
	nodes, _ := tree["nodes"].([]any)
	n := 0
	for _, item := range nodes {
		node, ok := item.(map[string]any)
		if !ok || node["type"] != NodeTypeDocument {
			continue
		}
		params, ok := node["params"].(map[string]any)
		if !ok {
			continue
		}
		old, ok := docIDOf(params["docId"])
		if !ok {
			continue
		}
		if nw, ok := ids[old]; ok {
			// 对外 ID 一律 string（防 JS 精度丢失）。
			params["docId"] = strconv.FormatInt(nw, 10)
			n++
		}
	}
	if n == 0 {
		return raw, 0, nil
	}
	out, err := json.Marshal(tree)
	if err != nil {
		return nil, 0, err
	}
	return out, n, nil
}

// docIDOf 兼容 string 与 number 两种写法（前端存的是 string）。
func docIDOf(v any) (int64, bool) {
	switch x := v.(type) {
	case string:
		id, err := strconv.ParseInt(x, 10, 64)
		return id, err == nil && id > 0
	case float64:
		// float64 在 2^53 以上会丢精度，雪花号恰好在那个范围 —— 这种写法本来就是坏的，
		// 只为不崩；转不回来的就当不在映射里。
		return int64(x), x > 0
	}
	return 0, false
}
