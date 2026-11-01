package protocol

import (
	"bytes"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path"
	"sort"
	"strings"
)

// FilePart 是 multipart 请求里的一个文件位。
//
// ⚠️ 协议层**只声明要哪张图**，不负责取字节：取字节要访问对象存储或出网下载，
// 那是 I/O。放进协议层的话，每个协议都得能起数据库和网络才测得动，
// 而 fixture 测试的全部价值就是不需要它们。
type FilePart struct {
	// Field 是表单字段名（OpenAI 编辑接口是 image / mask）。
	Field    string
	Filename string
	// Ref 是 Job.Images 里的原始引用（data: 或 http 地址），由调用方取成字节。
	Ref string
}

// MaterializedFile 是已经取到字节的文件位。
type MaterializedFile struct {
	Field       string
	Filename    string
	ContentType string
	Data        []byte
}

// EncodeMultipart 拼 multipart/form-data 请求体，返回体与对应的 Content-Type。
//
// ⚠️ Content-Type 必须用返回的这个：里面带着随机 boundary，
// 自己写死一个 `multipart/form-data` 上游会解不出任何字段，
// 而报错通常只是一句笼统的「缺少参数」，指不到真正的原因。
//
// ⚠️ 字段按名字排序：multipart 本身不要求顺序，但不定序的话
// 请求日志与 fixture 测试都不可复现 —— map 的遍历顺序每次都不同。
//
// ⚠️ 文件位**不能用 `CreateFormFile`**：它把每个文件位都写成 `application/octet-stream`。
// 单张图编辑时上游还认；多张参考图时中转把它们转成 `data:` 地址、要求图片类型，
// 回 400「unsupported MIME type 'application/octet-stream'」—— 分镜图组一整批因此全部失败（CD-82）。
func EncodeMultipart(fields map[string]string, files []MaterializedFile) (
	body []byte, contentType string, err error) {
	return EncodeMultipartWith(fields, nil, files)
}

// EncodeMultipartWith 同 EncodeMultipart，另带同名要出现多次的字段（按名字排序，同名的值保持给定顺序）。
func EncodeMultipartWith(fields map[string]string, repeated map[string][]string, files []MaterializedFile) (
	body []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if err := w.WriteField(k, fields[k]); err != nil {
			return nil, "", err
		}
	}
	multi := make([]string, 0, len(repeated))
	for k := range repeated {
		multi = append(multi, k)
	}
	sort.Strings(multi)
	for _, k := range multi {
		for _, v := range repeated[k] {
			if err := w.WriteField(k, v); err != nil {
				return nil, "", err
			}
		}
	}

	for _, f := range files {
		ct := fileContentType(f)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
			quoteEscaper.Replace(f.Field), quoteEscaper.Replace(fileNameFor(f.Filename, ct))))
		h.Set("Content-Type", ct)
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// quoteEscaper 与标准库 multipart 对文件名的转义一致。
var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

// fileContentType 定文件位的 Content-Type：**字节嗅出是图片就以字节为准**，否则用声明的类型。
//
// ⚠️ 声明的类型靠不住：对象存储下载回来的常常就是 octet-stream，data: 地址里写的也可能不对；
// 而字节里的文件头不会撒谎。嗅不出图片（测试里的假数据、音频）时才用声明的，再不行用嗅到的。
func fileContentType(f MaterializedFile) string {
	sniffed := http.DetectContentType(f.Data)
	if strings.HasPrefix(sniffed, "image/") {
		return sniffed
	}
	if declared := strings.TrimSpace(f.ContentType); declared != "" && !genericBinary(declared) {
		return declared
	}
	return sniffed
}

func genericBinary(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return true
	}
	return mt == "application/octet-stream" || mt == "binary/octet-stream"
}

var imageExtensions = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
}

// fileNameFor 让文件名的扩展名跟着真实类型走：有的中转按扩展名判类型，
// 一张 JPEG 叫 `reference-1.png` 的话同样会被当成格式不符。
func fileNameFor(name, contentType string) string {
	mt, _, _ := mime.ParseMediaType(contentType)
	ext, ok := imageExtensions[mt]
	if !ok {
		return name
	}
	current := strings.ToLower(path.Ext(name))
	if current == ext || (ext == ".jpg" && current == ".jpeg") {
		return name
	}
	return strings.TrimSuffix(name, path.Ext(name)) + ext
}
