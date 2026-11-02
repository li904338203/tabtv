// Package projectpkg 把单个项目打成 zip、以及从 zip 导入成新项目（TT-T-33，dev/02 §6）。
//
// 包内容：
//
//	manifest.json          格式、schemaVersion、导出时间、各表行数、warnings
//	project.json           project 行（不含 canvas_data）+ canvasData 原文
//	data/<表名>.json        项目从属表的整行（脚本、镜头、资产、组、时间线、导演台、生成记录与产物、上传参考、项目文档）
//	media.json             media_object 行
//	media/<旧媒体ID><扩展名> 媒体字节
//
// ⚠️ 导出**按整行**而不是按领域挑字段：画布、脚本、时间线、导演台各自的表还会长出新列，
//
//	挑字段的写法每加一列就要记得改这里，漏了不报错、导入后那一列悄悄变成默认值。
//	整行导出 + 导入时只插「本机表上存在的列」，两边版本差一点也能导。
//
// ⚠️ 导入一律重新发号：旧 ID 换新 ID 是**对整棵 JSON 树按值替换**（数字与字符串都看，
//
//	`/api/...` 地址里的路径段也看）。雪花号全局唯一，按值匹配不会误伤；
//	逐字段列举的话，画布 JSON 里新冒出来的引用字段（节点参数、组子项、导演台）一个漏改就是裂图。
//	未知字段原样保留（GO-35）。
//
// ⚠️ 包里**绝不**出现厂商配置与 Key（TT-F-10）：不导出 provider_config / model 表，
//
//	生成记录里的上游原文（provider_raw_result / error_detail / provider_task_id）清空；
//	模型与操作只按 code 记，导入时换成本机的 ID。
package projectpkg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// Format 与 SchemaVersion 是包格式（与 project.version 乐观锁无关，导入后 version 从 1 起）。
const (
	Format        = "tabtv.project"
	SchemaVersion = 1
)

// 解包上限：防恶意包把磁盘写满。单个文件 8GiB（几分钟的 4K 成片也够），全部 32GiB，条目 20 万。
const (
	MaxEntryBytes = int64(8) << 30
	MaxTotalBytes = int64(32) << 30
	MaxEntries    = 200_000
)

// Manifest 是 manifest.json。
type Manifest struct {
	Format        string         `json:"format"`
	SchemaVersion int            `json:"schemaVersion"`
	AppVersion    string         `json:"appVersion"`
	ExportedAt    time.Time      `json:"exportedAt"`
	ProjectName   string         `json:"projectName"`
	Tables        map[string]int `json:"tables"`
	Media         int            `json:"media"`
	// Catalog 把生成记录里的 operation_id / model_id 记成 code：两台机器上的 ID 不一样。
	Catalog  CatalogCodes `json:"catalog"`
	Warnings []string     `json:"warnings,omitempty"`
}

// CatalogCodes：旧 ID（字符串）→ code。
type CatalogCodes struct {
	Operations map[string]string `json:"operations"`
	Models     map[string]string `json:"models"`
}

// CheckManifest 校验包格式。认不出的大版本明确拒绝：硬导的话新版本写进来的字段被当成未知字段带进库，
// 而它们的语义这一版根本不知道。
func CheckManifest(m Manifest) error {
	if m.Format != Format {
		return errors.New("这不是 tabtv 的项目包（manifest.format 不对）")
	}
	if m.SchemaVersion < 1 {
		return errors.New("项目包缺少格式版本（manifest.schemaVersion）")
	}
	if m.SchemaVersion > SchemaVersion {
		return fmt.Errorf("这个项目包来自更新的 tabtv（包格式版本 %d，本机只认到 %d），请先升级应用", m.SchemaVersion, SchemaVersion)
	}
	return nil
}

var drive = regexp.MustCompile(`^[A-Za-z]:`)

// CheckEntryName 校验 zip 条目名（防 zip slip）。只认本格式会出现的几个位置。
func CheckEntryName(name string) error {
	switch {
	case name == "":
		return errors.New("项目包里有空名字的条目")
	case strings.Contains(name, `\`):
		return fmt.Errorf("项目包条目路径不合法：%q（含反斜杠）", name)
	case strings.HasPrefix(name, "/") || drive.MatchString(name):
		return fmt.Errorf("项目包条目路径不合法：%q（绝对路径）", name)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("项目包条目路径不合法：%q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("项目包条目路径不合法：%q（含 ..）", name)
		}
	}
	if path.Clean(name) != name {
		return fmt.Errorf("项目包条目路径不合法：%q", name)
	}
	switch {
	case name == "manifest.json", name == "project.json", name == "media.json":
		return nil
	case strings.HasPrefix(name, "data/") && strings.Count(name, "/") == 1 && strings.HasSuffix(name, ".json"):
		return nil
	case strings.HasPrefix(name, "media/") && strings.Count(name, "/") == 1:
		return nil
	}
	return fmt.Errorf("项目包里有不认识的条目：%q", name)
}

// MediaEntry 是媒体文件在包里的名字：media/<旧ID><扩展名>。
func MediaEntry(oldID, ext string) string { return "media/" + oldID + ext }

// ---------------------------------------------------------------- JSON 树

// Decode 把 JSON 解成通用树，数字保留为 json.Number（雪花号超过 float64 的精度）。
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// idRun 匹配字符串里的雪花号（15~20 位数字）。
var idRun = regexp.MustCompile(`\d{15,20}`)

// CollectIDs 收集树里所有可能是 ID 的值：数字、整串是数字的字符串、字符串里的长数字串。
// 收集是「宁多勿少」的：用它去查 media_object，查不到的自然丢掉。
func CollectIDs(v any, into map[string]struct{}) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if idRun.MatchString(k) {
				into[k] = struct{}{}
			}
			CollectIDs(x, into)
		}
	case []any:
		for _, x := range t {
			CollectIDs(x, into)
		}
	case json.Number:
		into[t.String()] = struct{}{}
	case string:
		for _, m := range idRun.FindAllString(t, -1) {
			into[m] = struct{}{}
		}
	}
}

// Remap 把树里的旧 ID 换成新 ID，返回新树。规则：
//   - 数字与整串等于旧 ID 的字符串：整体替换（类型不变）；
//   - 以 `/api/` 开头的地址：路径与查询里等于旧 ID 的长数字串逐个替换；
//   - 对象的键等于旧 ID 的也替换（有的前端结构以 ID 为键）。
//
// 其余一切（包括没在映射里的 ID）原样保留。
func Remap(v any, ids map[string]string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if n, ok := ids[k]; ok {
				k = n
			}
			out[k] = Remap(x, ids)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Remap(x, ids)
		}
		return out
	case json.Number:
		if n, ok := ids[t.String()]; ok {
			return json.Number(n)
		}
		return t
	case string:
		if n, ok := ids[t]; ok {
			return n
		}
		if strings.HasPrefix(t, "/api/") {
			return idRun.ReplaceAllStringFunc(t, func(s string) string {
				if n, ok := ids[s]; ok {
					return n
				}
				return s
			})
		}
		return t
	}
	return v
}

// RemapJSON 是 Remap 的字节版本。
func RemapJSON(raw []byte, ids map[string]string) ([]byte, error) {
	v, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Remap(v, ids))
}

// ImportName 是导入后的项目名：与已有项目重名时追加「（导入）」，仍重名再追加序号。
func ImportName(name string, taken func(string) bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "导入的项目"
	}
	if !taken(name) {
		return name
	}
	base := name + "（导入）"
	if !taken(base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s（导入 %d）", name, i)
		if !taken(cand) {
			return cand
		}
	}
	return base
}

// FileName 是导出包的文件名：<项目名>.tabtv.zip（项目名里的路径字符换掉）。
func FileName(project string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", "*", "_", "?", "_", `"`, "_", "<", "_", ">", "_", "|", "_")
	name := strings.TrimSpace(r.Replace(project))
	if name == "" {
		name = "project"
	}
	if rs := []rune(name); len(rs) > 80 {
		name = string(rs[:80])
	}
	return name + ".tabtv.zip"
}
