package objstore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本地目录后端（TT-T-08，dev/01 TTD-10）：桌面应用的默认存储，对象就是数据目录 media/ 下的文件。
//
// 与 S3 后端是同一个 Client 类型、同一组方法：媒体域、Provider 适配、导出、清理作业一律不用知道
// 底下是哪一种。两种后端的差异只有一处 —— 本地没有可以交给别人的地址：PresignGet* 返回
// `local://<key>`（不是 http/https）。各调用方本来就把「签不出公网地址」当成要回落的情况：
// 公开取图改为由接口直接出字节，发给上游的参考媒体改为内联（或走可选的公网中转存储）。

// LocalScheme 是本地后端签出的伪地址前缀。
const LocalScheme = "local://"

// LocalBucket 是本地后端的「桶名」（库里 media_object.bucket 记这个）。
const LocalBucket = "local"

type localFS struct{ root string }

// NewLocal 建一个本地目录后端。root 不存在时创建。
func NewLocal(root string, signTTL time.Duration) (*Client, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("创建本地存储目录失败: %w", err)
	}
	if signTTL <= 0 {
		signTTL = 6 * time.Hour
	}
	return &Client{local: &localFS{root: abs}, bucket: LocalBucket, signTTL: signTTL}, nil
}

// IsLocal 报告是否为本地后端。
func (c *Client) IsLocal() bool { return c.local != nil }

// path 把对象键映射到磁盘路径。键来自服务端（objectKey），但仍挡掉 `..` 与绝对路径：
// 一个键逃出存储目录就能读写任意文件。
func (l *localFS) path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimLeft(key, "/")))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("对象键非法: %q", key)
	}
	return filepath.Join(l.root, clean), nil
}

// put 先写临时文件再原子改名：写到一半崩溃不会留下一个看似完整的半截对象。
func (l *localFS) put(key string, body io.Reader) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (l *localFS) open(key string) (io.ReadCloser, int64, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func (l *localFS) stat(key string) (int64, bool, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, false, err
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return st.Size(), true, nil
}

func (l *localFS) delete(key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (l *localFS) listOlderThan(prefix string, before time.Time, limit int) ([]string, error) {
	dir, err := l.path(prefix)
	if err != nil {
		return nil, err
	}
	// 前缀可能是「目录/」也可能是「目录/文件名前缀」：从它所在的目录开始走，按键前缀过滤。
	start := dir
	if !strings.HasSuffix(prefix, "/") {
		start = filepath.Dir(dir)
	}
	var keys []string
	err = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".put-") {
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(before) {
			keys = append(keys, key)
			if len(keys) >= limit {
				return fs.SkipAll
			}
		}
		return nil
	})
	return keys, err
}
