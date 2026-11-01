package domain

import (
	"fmt"
	"strings"
	"time"
)

// 批量导出（NEW-15、GAP-12、GAP-18）。
//
// ⚠️ 打包**在服务端异步做**，不在浏览器里拼。旧系统的"全部下载"是前端
// 逐项 `a.href` 直链（GAP-18）：既绕过了鉴权头（受保护端点一律 401，
// 于是只能把桶开成公开），又在几十项时同时开几十条下载把浏览器打满。
// 任务化之后，进度、失败原因、过期清理才有地方表达。
//
// ⚠️ 导出**不收积分**：它没有调用任何上游，只是把已经属于用户的字节
// 重新装一次箱。收费没有依据 —— 与宫格切分是同一条理由（见 grid.go）。

// MaxExportItems 单次导出的项数上限。
//
// ⚠️ 上限存在的理由不是防滥用，是**让失败有边界**：一个包里装了两千项，
// 中间第 1999 项取不回来时，前面那一小时的下载与打包全都白做。
const MaxExportItems = 200

// MaxExportBytes 单个压缩包的总字节上限。
//
// ⚠️ 按**源文件累计**判，不是等压缩完再看：视频几乎压不动，
// 等打完再说超限，磁盘和带宽已经花掉了。
const MaxExportBytes int64 = 2 << 30 // 2 GiB

// DefaultExportTTL 压缩包的保留期。
//
// **设计建议值**（PARAM 未定稿），不是已批准的政策。取一天的依据：
// 导出是"点了就下"的动作，过夜之后用户多半已经拿到文件；
// 而包是原始素材的完整副本，留着就是双份存储成本。
const DefaultExportTTL = 24 * time.Hour

// ExportItem 是清单里的一项（受理时固定的快照）。
type ExportItem struct {
	// MediaID 要装进包的媒体对象。
	MediaID int64 `json:"mediaId,string"`
	// Name 建议文件名（不含扩展名），来自产物标题/资产名/镜号。
	Name string `json:"name,omitempty"`
}

// ZipEntry 是压缩包里的一条，文件名已去重且安全。
type ZipEntry struct {
	MediaID int64
	// Path 是包内路径。**一定是相对路径、不含 `..`、不含分隔符**。
	Path string
}

// PlanEntries 给每一项定好包内文件名。
//
// ⚠️ 必须去重：两个镜头都叫"第 1 镜"的话，同名条目在多数解压工具里
// 后一个覆盖前一个 —— 用户导出 9 张，解出来 7 张，而下载和解压都"成功"了。
//
// ⚠️ 必须清洗：名字来自用户可控的文本（资产名、节点标题）。带 `..` 或
// 绝对路径的条目名是经典的 zip slip —— 一部分解压工具会照着写到目标目录之外。
// 我们自己生成路径，所以在生成的这一步一次性挡掉，比指望每个解压端都安全靠谱。
func PlanEntries(items []ExportItem, mimeOf func(mediaID int64) string) []ZipEntry {
	used := map[string]bool{}
	out := make([]ZipEntry, 0, len(items))
	for i, it := range items {
		base := sanitizeName(it.Name)
		if base == "" {
			// 没名字就用序号。⚠️ 用序号不用 mediaID：ID 是 19 位雪花，
			// 一个文件夹里全是 1972... 开头的文件，用户没法分辨顺序。
			base = fmt.Sprintf("%03d", i+1)
		}
		ext := ".bin"
		if mimeOf != nil {
			if e := ExtFor(mimeOf(it.MediaID)); e != "" {
				ext = e
			}
		}
		name := base + ext
		for n := 2; used[strings.ToLower(name)]; n++ {
			// "封面.png" → "封面 (2).png"，与桌面系统的习惯一致。
			name = fmt.Sprintf("%s (%d)%s", base, n, ext)
		}
		// ⚠️ 大小写不敏感地去重：macOS 与 Windows 的默认文件系统都不区分
		// 大小写，`A.png` 与 `a.png` 在那里仍然是同一个文件。
		used[strings.ToLower(name)] = true
		out = append(out, ZipEntry{MediaID: it.MediaID, Path: name})
	}
	return out
}

// maxNameRunes 单个文件名的长度上限（按字符不按字节）。
//
// 多数文件系统按**字节**限 255，而一个中文字符是 3 字节 ——
// 按 80 个字符截断后最坏 240 字节，留出扩展名和去重后缀的余量。
const maxNameRunes = 80

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			// 控制字符：解压工具的表现各不相同，一律丢掉。
		case strings.ContainsRune(`/\:*?"<>|`, r):
			// 路径分隔符与 Windows 保留字符。`/` 尤其重要：
			// 留着它就等于让用户决定包内的目录结构。
			b.WriteRune('_')
		case r == '.' && b.Len() == 0:
			// 以点开头在类 Unix 下是隐藏文件，解出来用户找不到。
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	// ⚠️ 分隔符换成 `_` 之后，`../../etc` 会剩下 `_.._etc` —— 没有分隔符的
	// `..` 已经不构成穿越，但仍然要抹掉：一部分解压工具会先把 `_` 之类的
	// 字符做一轮自己的规范化，规范化之后那两个点可能又变回一个路径段。
	// 一次性消掉比逐个去猜解压端的行为可靠。
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", "_")
	}
	rs := []rune(out)
	if len(rs) > maxNameRunes {
		out = strings.TrimRight(string(rs[:maxNameRunes]), " .")
	}
	return out
}

// ExtFor 按 MIME 给扩展名。没有扩展名的文件在 Windows 上双击打不开。
func ExtFor(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "audio/mpeg":
		return ".mp3"
	case "audio/wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "text/plain":
		return ".txt"
	case "application/zip":
		return ".zip"
	}
	return ""
}

// 导出任务状态。queued/running/success/failed 与生成任务同一套词，
// 没有独立的 cancelled —— 打包最多几分钟，取消的价值不抵一套状态的复杂度。
const (
	ExportQueued  = "queued"
	ExportRunning = "running"
	ExportSuccess = "success"
	ExportFailed  = "failed"
	// ExportExpired 只出现在**读取结果**里，库里没有这个值。
	ExportExpired = "expired"
)

// EffectiveExportStatus 算出对外可见的状态。
//
// ⚠️ 过期是**算出来的**，不是清理作业标出来的：靠作业标记的话，
// 作业停了一天，界面上就有一整天的下载按钮点下去 404 ——
// 而用户看到的是"导出成功"。与提示词 ready/stale 是同一条规则：
// 正确性不依赖某个后台任务准时跑过。
func EffectiveExportStatus(status string, expiresAt, now time.Time) string {
	if status == ExportSuccess && !expiresAt.IsZero() && now.After(expiresAt) {
		return ExportExpired
	}
	return status
}

// ValidateExportItems 受理前的清单校验。
func ValidateExportItems(items []ExportItem) error {
	if len(items) == 0 {
		return fmt.Errorf("导出清单为空")
	}
	if len(items) > MaxExportItems {
		return fmt.Errorf("单次最多导出 %d 项，当前 %d 项", MaxExportItems, len(items))
	}
	seen := map[int64]bool{}
	for _, it := range items {
		if it.MediaID <= 0 {
			return fmt.Errorf("清单里有非法的媒体 ID")
		}
		// ⚠️ 同一项重复出现不报错而是**去重由调用方做**？不 —— 在这里拒绝。
		// 悄悄去重的话用户选了 10 项、包里只有 8 个文件，他会以为丢了东西。
		if seen[it.MediaID] {
			return fmt.Errorf("清单里有重复项")
		}
		seen[it.MediaID] = true
	}
	return nil
}
