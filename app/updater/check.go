package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultFeed 是更新源：GitHub Releases 的「最新正式版」（草稿与预发布不在其中）。
// 私有仓库阶段匿名请求回 404，此时报 ErrUnavailable 而不是错误（TT-T-31）。
const DefaultFeed = "https://api.github.com/repos/li904338203/tabtv/releases/latest"

// ErrUnavailable 表示更新源目前查不到（仓库未公开、还没有任何发布）。
var ErrUnavailable = errors.New("无法检查更新（仓库未公开或尚无发布）")

// Asset 是 release 里的一个文件。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release 是最新发布。
type Release struct {
	Tag     string  `json:"tag_name"`
	Name    string  `json:"name"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Draft   bool    `json:"draft"`
	Pre     bool    `json:"prerelease"`
	Assets  []Asset `json:"assets"`
}

// Asset 按名字取文件。
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client 访问更新源。
type Client struct {
	Feed    string
	Current string // 当前版本（ldflags 注入）
	HTTP    *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// 只带版本号：不带任何使用数据（TT-N-02）。
	req.Header.Set("User-Agent", "tabtv/"+c.Current)
	return c.http().Do(req)
}

// Latest 取最新正式版。
func (c *Client) Latest(ctx context.Context) (Release, error) {
	feed := c.Feed
	if feed == "" {
		feed = DefaultFeed
	}
	resp, err := c.get(ctx, feed)
	if err != nil {
		return Release{}, fmt.Errorf("检查更新失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Release{}, ErrUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("检查更新失败：更新源返回 %d", resp.StatusCode)
	}
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return Release{}, fmt.Errorf("检查更新失败：%w", err)
	}
	if r.Draft || r.Pre {
		// /releases/latest 本来就不返回这两类；防御一下，别把草稿推给用户。
		return Release{}, ErrUnavailable
	}
	return r, nil
}

// Download 下载本平台更新包到 dir 并按同一 release 的 SHA256SUMS 校验，返回文件路径。
//
// ⚠️ 先写临时文件、校验通过才改名：中途断网或校验失败都不会留下一个看起来完整的安装包，
// 下次也不会误用它。失败一律删掉临时文件，当前版本照常运行。
func (c *Client) Download(ctx context.Context, r Release, assetName, dir string) (string, error) {
	asset, ok := r.Asset(assetName)
	if !ok {
		return "", fmt.Errorf("这个版本没有本平台的更新包（%s）", assetName)
	}
	sumsAsset, ok := r.Asset("SHA256SUMS")
	if !ok {
		return "", errors.New("这个版本缺少 SHA256SUMS，无法校验，已放弃更新")
	}
	sums, err := c.fetchText(ctx, sumsAsset.URL, 1<<20)
	if err != nil {
		return "", err
	}
	table, err := ParseSums(sums)
	if err != nil {
		return "", err
	}
	want, ok := table[assetName]
	if !ok {
		return "", fmt.Errorf("SHA256SUMS 里没有 %s，已放弃更新", assetName)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tabtv-update-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	ok = false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	resp, err := c.get(ctx, asset.URL)
	if err != nil {
		return "", fmt.Errorf("下载更新包失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载更新包失败：返回 %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", fmt.Errorf("下载更新包失败：%w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", errors.New("更新包校验失败（文件可能损坏或被篡改），已删除，可以稍后重试")
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	final := filepath.Join(dir, assetName)
	if err := os.Rename(tmpName, final); err != nil {
		return "", err
	}
	ok = true
	return final, nil
}

func (c *Client) fetchText(ctx context.Context, url string, limit int64) (string, error) {
	resp, err := c.get(ctx, url)
	if err != nil {
		return "", fmt.Errorf("下载校验文件失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载校验文件失败：返回 %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	return strings.TrimSpace(string(b)), err
}
