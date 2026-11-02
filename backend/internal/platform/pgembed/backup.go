package pgembed

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Keep 是保留的备份份数（TT-F-41）。
const Keep = 3

// BackupCluster 停库、把集群目录整体复制到 backups/pg-<时间>-v<库版本>，然后重新起库。
//
// 没有 pg_dump（随包二进制里没有），所以做文件级备份：停库状态下的目录复制就是一份一致的快照，
// 恢复时把它放回 pg/ 即可。返回新的 Server（端口可能变化）与备份目录。
func BackupCluster(ctx context.Context, s *Server, backups string, schemaVersion int64) (*Server, string, error) {
	o := s.Options()
	if err := s.Stop(ctx); err != nil {
		return s, "", fmt.Errorf("备份前停库失败：%w", err)
	}
	name := fmt.Sprintf("pg-%s-v%d", time.Now().UTC().Format("20060102T150405Z"), schemaVersion)
	dest := filepath.Join(backups, name)
	copyErr := copyDir(o.Data, dest+".part")
	if copyErr == nil {
		copyErr = os.Rename(dest+".part", dest)
	}
	// 无论备份成功与否都把库起回来：失败时由调用方决定不迁移，但应用至少要能报错。
	ns, startErr := Start(ctx, o)
	if copyErr != nil {
		_ = os.RemoveAll(dest + ".part")
		if startErr != nil {
			return nil, "", fmt.Errorf("备份失败：%v；重新启动数据库也失败：%w", copyErr, startErr)
		}
		return ns, "", fmt.Errorf("备份失败：%w", copyErr)
	}
	if startErr != nil {
		return nil, dest, startErr
	}
	if err := Prune(backups, Keep); err != nil {
		return ns, dest, fmt.Errorf("备份已完成，但清理旧备份失败：%w", err)
	}
	return ns, dest, nil
}

// Prune 只保留最新的 keep 份 pg-* 备份（名字里的时间可直接按字典序比较）。
func Prune(backups string, keep int) error {
	entries, err := os.ReadDir(backups)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "pg-") && !strings.HasSuffix(e.Name(), ".part") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		if err := os.RemoveAll(filepath.Join(backups, names[0])); err != nil {
			return err
		}
		names = names[1:]
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if !info.Mode().IsRegular() {
			return nil // 停库后集群里不应有套接字等特殊文件；有也不需要
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
