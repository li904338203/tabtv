// Package app 提供媒体上传与受保护读取。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

type Service struct {
	repo   *repo.Repo
	store  *objstore.Client
	nextID func() int64
	// exportRetention 压缩包保留期，0 表示用默认值。
	exportRetention time.Duration
	// checkers 活动引用的来源（P14-03）。见 references.go 里为什么是计算式而不是登记式。
	checkers []ReferenceChecker
	log      *slog.Logger
	// analysisWaker 排进新的分析后叫醒扫描器（SCT-16）；nil 时只靠兜底扫描。
	analysisWaker AnalysisWaker
	// links 公开直连地址的复用缓存（见 public_link.go）。
	links linkCache
}

func New(r *repo.Repo, store *objstore.Client, nextID func() int64) *Service {
	return &Service{repo: r, store: store, nextID: nextID}
}

type UploadInput struct {
	WorkspaceID int64
	UserID      int64
	Purpose     string
	// DeclaredMIME 来自 multipart 的 Content-Type，**仅作交叉校验**，不作依据。
	DeclaredMIME string
	FileName     string
	Size         int64
	Body         io.Reader
	// ExpectKind 限定允许的媒体大类（封面只收图、试听只收音频）。
	//
	// ⚠️ KindSplat（3D 导演台的泼溅文件）只能经这里显式要求：通用识别不认泼溅（见 domain.KindSplat）。
	ExpectKind domain.Kind
	// MaxBytes 大于 0 时把这一类媒体的默认上限**收紧**到它（导演台预演 50 MiB），不能放宽。
	MaxBytes int64
	// Derive 非空表示这份字节是从另一份媒体派生来的（宫格切块）。
	Derive *DeriveInfo
}

// DeriveInfo 是派生溯源（NEW-14）。
//
// ⚠️ 只是**溯源线索**，不是所有权关系：源图删掉之后派生件依然有效
// （NEW-16 要求"源节点删除不破坏仍有效引用"）。
type DeriveInfo struct {
	SourceMediaID int64
	Kind          string
	// Key 区分同一种派生下的变体（宫格的 "3x3"）。
	Key   string
	Index int
	Meta  []byte
}

type Uploaded struct {
	ID        int64
	ObjectKey string
	MIME      string
	Kind      domain.Kind
	SizeBytes int64
	Checksum  string
	// Width / Height 图片尺寸；0 表示未知（非图片或格式不认识）。
	Width  int
	Height int
	// DurationMs 音视频时长；0 表示未知（非音视频或 ffprobe 认不出）。
	DurationMs int
}

// Upload 校验并存储一个媒体文件。
//
// ⚠️ **走临时文件而不是内存缓冲**（ADR TECH-07）：
// 产出里有几百 MB 的视频，按内存缓冲并发几路就把进程撑爆。
func (s *Service) Upload(ctx context.Context, in UploadInput) (Uploaded, error) {
	if s.store == nil {
		return Uploaded{}, httpx.Errf(errcode.Internal, "对象存储未配置")
	}

	tmp, err := os.CreateTemp("", "seeaitv-upload-*")
	if err != nil {
		return Uploaded{}, httpx.Wrap(errcode.Internal, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	// 先读前缀用于识别类型，再把已读部分一并写回临时文件。
	head := make([]byte, domain.SniffLen)
	n, err := io.ReadFull(in.Body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Uploaded{}, httpx.Wrap(errcode.Internal, err)
	}
	head = head[:n]

	var detected domain.Detected
	var verifyErr error
	if in.ExpectKind == domain.KindSplat {
		detected, verifyErr = domain.VerifySplatDeclared(head, in.DeclaredMIME)
	} else {
		detected, verifyErr = domain.VerifyDeclared(head, in.DeclaredMIME)
	}
	if errors.Is(verifyErr, domain.ErrUnknownType) {
		if in.ExpectKind == domain.KindSplat {
			return Uploaded{}, httpx.Errf(errcode.UnsupportedMediaType, "此处只接受 3D 高斯泼溅文件（.ply / .spz）")
		}
		return Uploaded{}, httpx.Errf(errcode.UnsupportedMediaType, "无法识别的文件类型")
	}
	if verifyErr != nil {
		// 声明与实际不符：这是伪装上传的典型特征，明确拒绝。
		return Uploaded{}, httpx.Errf(errcode.FileContentMismatch, verifyErr.Error())
	}
	if in.ExpectKind != "" && detected.Kind != in.ExpectKind {
		return Uploaded{}, httpx.Errf(errcode.UnsupportedMediaType,
			fmt.Sprintf("此处只接受%s文件，实际为 %s", kindLabel(in.ExpectKind), detected.MIME))
	}

	limits := domain.DefaultLimits(detected.Kind).Tighten(in.MaxBytes)
	if err := limits.Check(detected, in.Size); err != nil {
		return Uploaded{}, httpx.Errf(errcode.PayloadTooLarge, err.Error())
	}

	// 流式写入临时文件并同时算 checksum。
	// MaxBytes+1 作为硬闸：声明的 Size 可能撒谎。
	hasher := sha256.New()
	if _, err := tmp.Write(head); err != nil {
		return Uploaded{}, httpx.Wrap(errcode.Internal, err)
	}
	hasher.Write(head)

	limited := io.LimitReader(in.Body, limits.MaxBytes+1-int64(n))
	written, err := io.Copy(io.MultiWriter(tmp, hasher), limited)
	if err != nil {
		return Uploaded{}, httpx.Wrap(errcode.Internal, err)
	}
	total := int64(n) + written
	if total > limits.MaxBytes {
		return Uploaded{}, httpx.Errf(errcode.PayloadTooLarge,
			fmt.Sprintf("文件超过上限 %.1f MB", float64(limits.MaxBytes)/(1<<20)))
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return Uploaded{}, httpx.Wrap(errcode.Internal, err)
	}
	// 泼溅的第二段识别（DRD-28）：16 字节只能初判，以 ply 开头的文本、任意 gzip 都过得了第一段。
	// 放在存对象之前：认不出来的文件不该进对象存储。
	if detected.Kind == domain.KindSplat {
		if err := domain.VerifySplat(tmp.Name(), detected.MIME); err != nil {
			if errors.Is(err, domain.ErrNotSplat) {
				return Uploaded{}, httpx.Errf(errcode.UnsupportedMediaType, err.Error())
			}
			return Uploaded{}, httpx.Wrap(errcode.Internal, err)
		}
	}

	// ⚠️ 尺寸在**这里**探测：此刻文件就在本地临时文件里，读头部几乎不花钱。
	// 等到用的时候再去对象存储拉一次、解一次，是一次网络往返加一次解码 ——
	// 而蒙版校验恰恰发生在受理链路上，那条路上多一次往返就是用户多等一秒。
	//
	// 探测失败不影响上传：记不下尺寸的后果只是蒙版校验放行（见 CheckMaskSize）。
	width, height, _ := domain.ImageSize(tmp, detected.MIME)
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return Uploaded{}, httpx.Wrap(errcode.Internal, err)
	}
	// ⚠️ 时长同理在**这里**探测（NEW-07 的前置）：文件就在本地临时文件里，
	// ffprobe 读一下头几乎不花钱。没有时长就没法校验"裁切区间不能超出素材"，
	// 而超出的后果是渲染出一段黑屏、任务却是"成功"的。
	//
	// 探测失败不影响上传：ffprobe 可能没装、可能不认识这个封装 ——
	// 那个文件浏览器能播、上游也能用，只是我们记不下时长。
	durationMs, _ := domain.ProbeDuration(ctx, tmp.Name(), detected.MIME)

	id := s.nextID()
	checksum := hex.EncodeToString(hasher.Sum(nil))
	key := objectKey(in.Purpose, id, detected.MIME)

	if _, err := s.store.S3().PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.store.Bucket()),
		Key:           aws.String(key),
		Body:          tmp,
		ContentType:   aws.String(detected.MIME),
		ContentLength: aws.Int64(total),
	}); err != nil {
		return Uploaded{}, httpx.Wrap(errcode.Internal, fmt.Errorf("上传对象存储失败: %w", err))
	}

	// ⚠️ 先存对象再写库：反过来的话，写库成功而上传失败会留下
	// 指向不存在对象的记录，前端拿到 URL 却 404。
	row := repo.MediaObject{
		ID: id, WorkspaceID: in.WorkspaceID, Bucket: s.store.Bucket(), ObjectKey: key,
		ContentType: detected.MIME, SizeBytes: total, Checksum: checksum,
		Width: width, Height: height, DurationMs: durationMs,
		Purpose: in.Purpose, CreatedByUserID: in.UserID,
	}
	if in.Derive != nil {
		row.SourceMediaID = in.Derive.SourceMediaID
		row.DeriveKind = in.Derive.Kind
		row.DeriveKey = in.Derive.Key
		row.DeriveIndex = in.Derive.Index
		row.DeriveMeta = in.Derive.Meta
	}
	if err := s.repo.Insert(ctx, row); err != nil {
		return Uploaded{}, httpx.Wrap(errcode.Internal, err)
	}

	return Uploaded{ID: id, ObjectKey: key, MIME: detected.MIME,
		Kind: detected.Kind, SizeBytes: total, Checksum: checksum,
		Width: width, Height: height, DurationMs: durationMs}, nil
}

// Open 打开一个媒体对象供下载。调用方负责归属校验。
func (s *Service) Open(ctx context.Context, mediaID int64) (io.ReadCloser, repo.MediaObject, error) {
	m, err := s.repo.Get(ctx, mediaID)
	if err != nil {
		return nil, repo.MediaObject{}, httpx.Err(errcode.NotFound)
	}
	out, err := s.store.S3().GetObject(ctx, &s3.GetObjectInput{
		// 桶用当前配置的，不用行里记的：那一列是写入时的历史记录，换存储（2026-11-02 迁到 TOS）后仍是旧桶名，照它取旧文件一律 404。
		Bucket: aws.String(s.store.Bucket()), Key: aws.String(m.ObjectKey),
	})
	if err != nil {
		return nil, repo.MediaObject{}, httpx.Wrap(errcode.Internal, err)
	}
	return out.Body, m, nil
}

func (s *Service) Get(ctx context.Context, mediaID int64) (repo.MediaObject, error) {
	m, err := s.repo.Get(ctx, mediaID)
	if err != nil {
		return repo.MediaObject{}, httpx.Err(errcode.NotFound)
	}
	return m, nil
}

// objectKey 按用途与日期分目录，便于按前缀做生命周期规则与排查。
func objectKey(purpose string, id int64, mime string) string {
	return path.Join(purpose, time.Now().UTC().Format("2006/01/02"),
		fmt.Sprintf("%d%s", id, extFor(mime)))
}

func extFor(mime string) string {
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
	case domain.MIMEPly:
		return ".ply"
	case domain.MIMESpz:
		return ".spz"
	}
	return ".bin"
}

func kindLabel(k domain.Kind) string {
	switch k {
	case domain.KindImage:
		return "图片"
	case domain.KindAudio:
		return "音频"
	case domain.KindVideo:
		return "视频"
	case domain.KindSplat:
		return "3D 高斯泼溅"
	}
	return "媒体"
}
