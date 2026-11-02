package updater

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
)

// ParseSums 解析 `sha256sum` 输出格式的 SHA256SUMS：`<64 位十六进制>  <文件名>`（二进制模式文件名前带 `*`）。
// 格式不对的行直接报错 —— 校验文件本身坏了时宁可不更新，也不能「挑能读的那几行」。
func ParseSums(content string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("SHA256SUMS 第 %d 行格式不对", n)
		}
		sum, name := strings.ToLower(f[0]), strings.TrimPrefix(f[1], "*")
		if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 {
			return nil, fmt.Errorf("SHA256SUMS 第 %d 行的校验值不是 SHA-256", n)
		}
		out[name] = sum
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("SHA256SUMS 是空的")
	}
	return out, nil
}

// AssetName 是本平台更新包的文件名（命名约定见 dev/04 R-7，发布流水线 TT-T-29 必须照此命名）。
// macOS 返回空串：未签名应用不能自己替换自己（TT-F-40），只提示去下载页。
func AssetName(version, goos, goarch string) string {
	v := strings.TrimPrefix(version, "v")
	switch goos {
	case "windows":
		return fmt.Sprintf("tabtv-%s-windows-%s-setup.exe", v, goarch)
	case "linux":
		return fmt.Sprintf("tabtv-%s-linux-%s.AppImage", v, goarch)
	}
	return ""
}

// CurrentAssetName 是 AssetName 的本机版本。
func CurrentAssetName(version string) string { return AssetName(version, runtime.GOOS, runtime.GOARCH) }
