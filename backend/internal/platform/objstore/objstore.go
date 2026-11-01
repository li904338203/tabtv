// Package objstore 提供 S3 兼容对象存储客户端（ADR-0001 TECH-07）。
//
// 一套代码覆盖 MinIO / 阿里云 OSS / 腾讯云 COS / R2 / S3，靠自定义 endpoint。
//
// ⚠️ local 存储仅限单机开发：它无法预签名，参考图只能内联 base64
// （体积暴涨且部分上游直接拒收），且 API 与 Worker 分机部署时互相读不到盘。
// production 下 config.Validate 会直接拒绝 local。
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/li904338203/tabtv/backend/internal/platform/config"
)

type Client struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
	signTTL time.Duration
	// prefix 全部对象键的公共前缀（见 config.Storage.Prefix）；由 prefixMiddleware 统一加上。
	prefix string
}

// Open 建客户端并探测存储可达（启动时用：存储不可用就该起不来）。
func Open(ctx context.Context, cfg config.Storage) (*Client, error) {
	c, err := New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Check(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// New 只建客户端、**不探测**（签名是纯本地计算；集成测试据此在没有对象存储时验签名地址）。
// 服务启动一律用 Open。
func New(ctx context.Context, cfg config.Storage) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("SEEAITV_STORAGE_BUCKET 未配置")
	}
	loadOpts := []func(*awscfg.LoadOptions) error{awscfg.WithRegion(cfg.Region)}
	if cfg.AccessKey != "" {
		loadOpts = append(loadOpts, awscfg.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}
	base, err := awscfg.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("装载 AWS 配置失败: %w", err)
	}

	prefix, err := NormalizePrefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	api := s3.NewFromConfig(base, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			// MinIO 等自建服务只认 path-style；火山 TOS 的 S3 端点只认 virtual-hosted（SEEAITV_STORAGE_PATH_STYLE=false）。
			o.UsePathStyle = cfg.PathStyle
		}
		// 校验和只在接口必需时算（2026-11-02 接入 TOS）：SDK v1.73 起默认给上传加 CRC32 尾随校验、给下载加
		// 一个参与签名的 x-amz-checksum-mode 头，兼容 S3 的服务常因此报签名错误或 400 —— 而且错在预签名
		// 地址上时，表现是「上游取参考图 403」，看着像上游的问题。MinIO 与 AWS 在这个取值下照常工作。
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		if prefix != "" {
			o.APIOptions = append(o.APIOptions, prefixMiddleware(prefix))
		}
	})

	c := &Client{
		s3:      api,
		presign: s3.NewPresignClient(api),
		bucket:  cfg.Bucket,
		signTTL: cfg.SignTTL,
		prefix:  prefix,
	}
	return c, nil
}

// PresignGet 签发下载地址。
// ⚠️ TTL 不要调短（默认 6h）：地址在提交任务时签发，
// 上游可能排队几分钟才来取，失败重试更晚。
func (c *Client) PresignGet(ctx context.Context, key string) (string, error) {
	return c.PresignGetTTL(ctx, key, c.signTTL)
}

// PresignGetTTL 按指定有效期签发下载地址（MCPT-04：对外 MCP 的 media.get 只给 1 小时，MCP-PARAM-06）。
//
// ⚠️ 与 PresignGet 分开而不是改它的默认值：那条路给上游取参考图，地址要撑过排队与重试；
// 这条路把地址交给外部客户端，有效期越短泄漏面越小。ttl<=0 时回落到配置的 SignTTL。
func (c *Client) PresignGetTTL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = c.signTTL
	}
	req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("签发下载地址失败: %w", err)
	}
	return req.URL, nil
}

// PresignGetDownload 签发一个「下载为附件」的地址（智能剪辑成片下载，SCK-10）：浏览器直接存成文件，
// 不在新标签页里播放；文件名按 RFC 5987 带 UTF-8（中文名不乱码）。ttl<=0 时回落到配置的 SignTTL。
func (c *Client) PresignGetDownload(ctx context.Context, key string, ttl time.Duration, fileName string) (string, error) {
	if ttl <= 0 {
		ttl = c.signTTL
	}
	in := &s3.GetObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)}
	if fileName != "" {
		in.ResponseContentDisposition = aws.String(ContentDisposition(fileName))
	}
	req, err := c.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("签发下载地址失败: %w", err)
	}
	return req.URL, nil
}

// ContentDisposition 附件下载头：`attachment; filename="<ASCII 兜底>"; filename*=UTF-8”<百分号编码>`。
//
// ⚠️ 两个都给：只给 filename* 的话个别老客户端不认，只给 filename 的话中文名会乱码或被截。
func ContentDisposition(fileName string) string {
	ascii := make([]rune, 0, len(fileName))
	for _, r := range fileName {
		switch {
		case r < 0x20 || r > 0x7e || r == '"' || r == '\\':
			ascii = append(ascii, '_')
		default:
			ascii = append(ascii, r)
		}
	}
	return `attachment; filename="` + string(ascii) + `"; filename*=UTF-8''` + url.PathEscape(fileName)
}

func (c *Client) Bucket() string { return c.bucket }

// SignTTL 是配置的默认签名有效期（SEEAITV_STORAGE_SIGN_TTL）。
func (c *Client) SignTTL() time.Duration { return c.signTTL }

func (c *Client) S3() *s3.Client { return c.s3 }

func (c *Client) Name() string { return "objstore" }

// Check 探活。用 HeadBucket 而不是 ListBuckets：
// 生产凭据通常只对单个桶有权限，ListBuckets 会因权限不足而误报故障。
func (c *Client) Check(ctx context.Context) error {
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(c.bucket)})
	if err != nil {
		return fmt.Errorf("对象存储探活失败（桶 %s）: %w", c.bucket, err)
	}
	return nil
}

// Exists 判断对象在不在（HeadObject）。不存在返回 false 而不是错误；权限、网络这类错误原样返回 ——
// 把它们也当成「不存在」的话，调用方会去重传一份，真正的问题反而被盖住了。
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err == nil {
		return true, nil
	}
	var nf *types.NotFound
	var re *awshttp.ResponseError
	if errors.As(err, &nf) || (errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound) {
		return false, nil
	}
	return false, err
}

// SameSize 对象在且大小等于 size（拷贝工具判断「已经拷过」用）。不在返回 false、nil。
func (c *Client) SameSize(ctx context.Context, key string, size int64) (bool, error) {
	out, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	if err != nil {
		var nf *types.NotFound
		var re *awshttp.ResponseError
		if errors.As(err, &nf) || (errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound) {
			return false, nil
		}
		return false, err
	}
	return aws.ToInt64(out.ContentLength) == size, nil
}

// PutFile 上传一个可 Seek 的 body（一般是临时文件）：签名要先读一遍 body 再回到开头，所以大文件不能进内存。
func (c *Client) PutFile(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket), Key: aws.String(key), Body: body,
		ContentType: aws.String(contentType), ContentLength: aws.Int64(size),
	})
	return err
}

// Delete 删一个对象。不存在也算成功（S3 的 DeleteObject 本来就是幂等的）。
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(c.bucket), Key: aws.String(key)})
	return err
}

// ListOlderThan 列出某个前缀下最后修改时间早于 before 的对象键，最多 limit 个（清理临时对象用）。
//
// ⚠️ 只按前缀扫、只给清理作业用：桶里是全部用户的素材，开放任意前缀等于给了一个列全桶的口子。
func (c *Client) ListOlderThan(ctx context.Context, prefix string, before time.Time, limit int) ([]string, error) {
	if prefix == "" {
		return nil, errors.New("清理只能按前缀扫，不能扫整个桶")
	}
	var keys []string
	var token *string
	for len(keys) < limit {
		out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(c.bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return keys, err
		}
		for _, o := range out.Contents {
			if o.Key != nil && o.LastModified != nil && o.LastModified.Before(before) {
				// 列出来的键带着公共前缀：去掉再交出去，调用方拿它去 Delete 时中间件会再加上。
				keys = append(keys, strings.TrimPrefix(*o.Key, c.prefix))
				if len(keys) >= limit {
					break
				}
			}
		}
		if out.IsTruncated == nil || !*out.IsTruncated || out.NextContinuationToken == nil {
			break
		}
		token = out.NextContinuationToken
	}
	return keys, nil
}
