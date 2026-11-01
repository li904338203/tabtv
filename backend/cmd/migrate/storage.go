package main

// 对象存储运维（2026-11-02，接入火山 TOS）：
//
//	storage-check  对当前配置的存储做一轮读写探测（上传 → HEAD → 用预签名地址裸取 → 删除）
//	storage-copy   把另一个桶（SEEAITV_COPY_SRC_*）里的对象按原键拷到当前配置的存储（带公共前缀）
//
// 凭据一律来自环境变量，不打印。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
)

// storageCheck 探测当前配置的对象存储能不能用 —— 不只是「桶在不在」：
// 预签名地址要能被**不带任何额外头**的客户端取回（上游取参考图就是这样取的）。
func storageCheck(ctx context.Context, cfg config.Storage) error {
	if cfg.Type != "s3" {
		return fmt.Errorf("当前 SEEAITV_STORAGE_TYPE=%q，storage-check 只检查 s3", cfg.Type)
	}
	c, err := objstore.New(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("\n对象存储探测：endpoint=%s bucket=%s pathStyle=%v prefix=%q\n\n", cfg.Endpoint, cfg.Bucket, cfg.PathStyle, cfg.Prefix)
	step := func(name string, err error) error {
		if err != nil {
			fmt.Printf("FAIL  %s\n      %v\n", name, err)
			return fmt.Errorf("%s 失败", name)
		}
		fmt.Printf("  ok  %s\n", name)
		return nil
	}
	if err := step("HeadBucket", c.Check(ctx)); err != nil {
		return err
	}
	key := fmt.Sprintf("zz-probe/%d.txt", time.Now().UnixNano())
	body := []byte("seeaitv storage probe " + time.Now().UTC().Format(time.RFC3339))
	if err := step("上传 PutObject", c.PutFile(ctx, key, bytes.NewReader(body), int64(len(body)), "text/plain")); err != nil {
		return err
	}
	defer func() { _ = c.Delete(context.Background(), key) }()
	ok, err := c.Exists(ctx, key)
	if err == nil && !ok {
		err = errors.New("上传后 HEAD 说不存在")
	}
	if err := step("HeadObject", err); err != nil {
		return err
	}
	u, err := c.PresignGet(ctx, key)
	if err := step("签发预签名地址", err); err != nil {
		return err
	}
	err = func() error {
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		got, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(got[:min(len(got), 300)])))
		}
		if !bytes.Equal(got, body) {
			return errors.New("取回的内容与上传的不一致")
		}
		return nil
	}()
	if err := step("用预签名地址裸取（模拟上游取参考图）", err); err != nil {
		return err
	}
	keys, err := c.ListOlderThan(ctx, "zz-probe/", time.Now().Add(time.Minute), 5)
	if err == nil && len(keys) == 0 {
		err = errors.New("按前缀列不出刚上传的对象")
	}
	if err := step("按前缀列举 ListObjectsV2", err); err != nil {
		return err
	}
	if err := step("删除 DeleteObject", c.Delete(ctx, key)); err != nil {
		return err
	}
	fmt.Println("\n全部通过：这套配置可以用于 API 与 Worker。")
	return nil
}

// copySource 读 SEEAITV_COPY_SRC_*：要搬走的旧存储（本机 / 测试服的 MinIO）。
func copySource() (config.Storage, error) {
	get := func(k, def string) string {
		if v := os.Getenv("SEEAITV_COPY_SRC_" + k); v != "" {
			return v
		}
		return def
	}
	pathStyle, _ := strconv.ParseBool(get("PATH_STYLE", "true"))
	src := config.Storage{Type: "s3", Endpoint: get("ENDPOINT", ""), Region: get("REGION", "us-east-1"),
		Bucket: get("BUCKET", ""), AccessKey: get("ACCESS_KEY", ""), SecretKey: get("SECRET_KEY", ""),
		PathStyle: pathStyle, Prefix: get("PREFIX", ""), SignTTL: time.Hour}
	if src.Endpoint == "" || src.Bucket == "" {
		return src, errors.New("缺少 SEEAITV_COPY_SRC_ENDPOINT / SEEAITV_COPY_SRC_BUCKET（要搬走的旧存储）")
	}
	return src, nil
}

// skipCopy 临时对象不搬：截取片段（scratch/）与公网中转（relay/）各有自己的过期清理，搬过去只是占地方。
func skipCopy(key string) bool {
	return strings.HasPrefix(key, "scratch/") || strings.HasPrefix(key, "relay/") || strings.HasPrefix(key, "zz-probe/")
}

// storageCopy 把旧存储里的对象按原键拷到当前配置的存储。
//
// - 幂等：目标里已有同名且大小一致的跳过，中断了直接重跑；
// - 大文件走临时文件，不进内存（成片几百 MB，几路并发就能把内存吃满）；
// - SEEAITV_COPY_DRY_RUN=true 只统计不写。
func storageCopy(ctx context.Context, dstCfg config.Storage) error {
	srcCfg, err := copySource()
	if err != nil {
		return err
	}
	if dstCfg.Type != "s3" {
		return fmt.Errorf("目标 SEEAITV_STORAGE_TYPE=%q，只能拷到 s3", dstCfg.Type)
	}
	sp, _ := objstore.NormalizePrefix(srcCfg.Prefix)
	dp, _ := objstore.NormalizePrefix(dstCfg.Prefix)
	if strings.TrimRight(srcCfg.Endpoint, "/") == strings.TrimRight(dstCfg.Endpoint, "/") && srcCfg.Bucket == dstCfg.Bucket && sp == dp {
		return errors.New("源与目标是同一个桶、同一个前缀：当前存储还没切到新地方（先在 .env 里配好 SEEAITV_STORAGE_*）")
	}
	src, err := objstore.New(ctx, srcCfg)
	if err != nil {
		return fmt.Errorf("源存储：%w", err)
	}
	dst, err := objstore.New(ctx, dstCfg)
	if err != nil {
		return fmt.Errorf("目标存储：%w", err)
	}
	if err := src.Check(ctx); err != nil {
		return fmt.Errorf("源存储：%w", err)
	}
	if err := dst.Check(ctx); err != nil {
		return fmt.Errorf("目标存储：%w", err)
	}
	dry := os.Getenv("SEEAITV_COPY_DRY_RUN") == "true"
	workers := 4
	if n, err := strconv.Atoi(os.Getenv("SEEAITV_COPY_WORKERS")); err == nil && n > 0 && n <= 16 {
		workers = n
	}
	fmt.Printf("\n拷贝：%s/%s（前缀 %q）→ %s/%s（前缀 %q）%s\n\n", srcCfg.Endpoint, srcCfg.Bucket, srcCfg.Prefix,
		dstCfg.Endpoint, dstCfg.Bucket, dstCfg.Prefix, map[bool]string{true: "【只统计，不写】", false: ""}[dry])

	type item struct {
		key  string
		size int64
	}
	jobs := make(chan item)
	var copied, skipped, failed, bytesCopied atomic.Int64
	var mu sync.Mutex
	var failures []string
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if err := copyOne(ctx, src, dst, it.key, it.size, dry); err != nil {
					failed.Add(1)
					mu.Lock()
					failures = append(failures, it.key+"："+err.Error())
					mu.Unlock()
					continue
				}
				copied.Add(1)
				bytesCopied.Add(it.size)
			}
		}()
	}

	var listed, ignored int64
	pager := s3.NewListObjectsV2Paginator(src.S3(), &s3.ListObjectsV2Input{Bucket: aws.String(src.Bucket())})
	var listErr error
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			listErr = err
			break
		}
		for _, o := range page.Contents {
			if o.Key == nil {
				continue
			}
			// 源也可能带公共前缀：列出来的键去掉它，拷到目标时由目标客户端加上自己的前缀。
			key := *o.Key
			if p, _ := objstore.NormalizePrefix(srcCfg.Prefix); p != "" {
				key = strings.TrimPrefix(key, p)
			}
			listed++
			if skipCopy(key) {
				ignored++
				continue
			}
			size := aws.ToInt64(o.Size)
			if same, err := dst.SameSize(ctx, key, size); err == nil && same {
				skipped.Add(1)
				continue
			}
			jobs <- item{key: key, size: size}
		}
		if listed%500 == 0 {
			fmt.Printf("  已列出 %d · 已拷 %d · 已有跳过 %d · 失败 %d\n", listed, copied.Load(), skipped.Load(), failed.Load())
		}
	}
	close(jobs)
	wg.Wait()
	fmt.Printf("\n列出 %d · 临时对象不搬 %d · 拷贝 %d（%.1f MB）· 目标已有跳过 %d · 失败 %d\n",
		listed, ignored, copied.Load(), float64(bytesCopied.Load())/1e6, skipped.Load(), failed.Load())
	for i, f := range failures {
		if i == 20 {
			fmt.Printf("  …… 另有 %d 条\n", len(failures)-20)
			break
		}
		fmt.Println("  失败：" + f)
	}
	if listErr != nil {
		return fmt.Errorf("列举源桶中途失败（已拷的不受影响，重跑会接着拷）：%w", listErr)
	}
	if failed.Load() > 0 {
		return fmt.Errorf("%d 个对象没拷成功，重跑即可（已拷的会跳过）", failed.Load())
	}
	return nil
}

func copyOne(ctx context.Context, src, dst *objstore.Client, key string, size int64, dry bool) error {
	if dry {
		return nil
	}
	out, err := src.S3().GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(src.Bucket()), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("读源对象：%w", err)
	}
	defer out.Body.Close()
	f, err := os.CreateTemp("", "seeaitv-copy-*")
	if err != nil {
		return err
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	n, err := io.Copy(f, out.Body)
	if err != nil {
		return fmt.Errorf("下载：%w", err)
	}
	if size > 0 && n != size {
		return fmt.Errorf("下载到 %d 字节，源对象是 %d 字节", n, size)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ct := aws.ToString(out.ContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	if err := dst.PutFile(ctx, key, f, n, ct); err != nil {
		return fmt.Errorf("上传：%w", err)
	}
	return nil
}
