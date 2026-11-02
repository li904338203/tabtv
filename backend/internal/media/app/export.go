package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 异步批量导出（NEW-15、GAP-12、GAP-18）。
//
// ⚠️ 打包在**服务端异步**做。旧系统的"全部下载"是前端逐项 `a.href` 直链：
// `<a href>` 不会带 Authorization 头，于是受保护端点一律 401 —— 那条路
// 只有把桶开成公开才走得通，而那等于所有人的产物都能被猜到地址下载。
//
// ⚠️ 导出**不扣积分**：没调用任何上游，只是把已经属于用户的字节重新装箱。

// ExportTopic 是导出任务的 outbox topic。
const ExportTopic = "export.requested"

// ExportPayload 是投递给 Worker 的消息体。
//
// ⚠️ 只带 ID，**不带清单**：清单已经在 export_task.items 里固定下来了。
// 消息里再带一份的话，两份早晚会不一致，而不一致时以哪份为准没人说得清。
type ExportPayload struct {
	ExportID string `json:"exportId"`
}

// CreateExportInput 是受理入参。
type CreateExportInput struct {
	WorkspaceID int64
	UserID      int64
	Items       []domain.ExportItem
}

// CreateExport 受理一次批量导出。
//
// ⚠️ 受理时就**逐项校验归属**：留到 Worker 再查的话，越权的那几项会
// 一路走到打包环节才被发现，而那时任务已经"进行中"了 —— 用户看到的是
// 一个转了半天然后失败的任务，而不是一次明确的拒绝。
func (s *Service) CreateExport(ctx context.Context, in CreateExportInput) (repo.ExportTask, error) {
	if err := domain.ValidateExportItems(in.Items); err != nil {
		return repo.ExportTask{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	var total int64
	for _, it := range in.Items {
		m, err := s.repo.GetScoped(ctx, in.WorkspaceID, it.MediaID)
		if err != nil {
			return repo.ExportTask{}, httpx.Errf(errcode.NotFound,
				fmt.Sprintf("清单里有取不到的文件（%d）", it.MediaID))
		}
		total += m.SizeBytes
	}
	if total > domain.MaxExportBytes {
		// ⚠️ 按**源文件累计**判，不等压缩完再看：视频几乎压不动，
		// 等打完再说超限，磁盘和带宽已经花掉了。
		return repo.ExportTask{}, httpx.Errf(errcode.PayloadTooLarge,
			fmt.Sprintf("所选文件合计 %.1f GB，超过单次导出上限 %.0f GB",
				float64(total)/(1<<30), float64(domain.MaxExportBytes)/(1<<30)))
	}

	items, err := json.Marshal(in.Items)
	if err != nil {
		return repo.ExportTask{}, httpx.Wrap(errcode.Internal, err)
	}
	task := repo.ExportTask{
		ID: s.nextID(), WorkspaceID: in.WorkspaceID, UserID: in.UserID,
		Kind: "zip", Status: domain.ExportQueued, Items: items,
		ItemCount: len(in.Items),
		ExpiresAt: time.Now().Add(s.exportTTL()),
	}
	payload, _ := json.Marshal(ExportPayload{ExportID: fmt.Sprint(task.ID)})
	if err := s.repo.InsertExport(ctx, task, s.nextID(), ExportTopic, payload); err != nil {
		return repo.ExportTask{}, httpx.Wrap(errcode.Internal, err)
	}
	return task, nil
}

func (s *Service) exportTTL() time.Duration {
	if s.exportRetention > 0 {
		return s.exportRetention
	}
	return domain.DefaultExportTTL
}

// SetExportRetention 覆盖压缩包保留期（默认 24 小时，**设计建议值**）。
func (s *Service) SetExportRetention(d time.Duration) { s.exportRetention = d }

func (s *Service) GetExport(ctx context.Context, workspaceID, id int64) (repo.ExportTask, error) {
	t, err := s.repo.GetExport(ctx, workspaceID, id)
	if err != nil {
		return repo.ExportTask{}, httpx.Err(errcode.NotFound)
	}
	return t, nil
}

func (s *Service) ListExports(ctx context.Context, workspaceID int64, limit int,
	cursorID int64) ([]repo.ExportTask, bool, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.repo.ListExports(ctx, workspaceID, limit+1, cursorID)
	if err != nil {
		return nil, false, httpx.Wrap(errcode.Internal, err)
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// ---------------------------------------------------------------- Worker 侧

// ExportOutcome 告诉消费者怎么 ack。
type ExportOutcome int

const (
	// ExportDone 打包完成。
	ExportDone ExportOutcome = iota
	// ExportDuplicate 已被别人领走或已是终态 —— at-least-once 的正常结局。
	ExportDuplicate
	// ExportRetry 暂时性失败，放回队列。
	ExportRetry
)

// RunExport 执行一次打包。
//
// ⚠️ **写临时文件不写内存**：一次导出上限 2GB，按内存缓冲并发两路就 OOM。
// zip.Writer 也不能直接写 HTTP 响应 —— 那样一旦中途某项取不回来，
// 用户手上会留下一个已经下了一半、解不开的文件，而 HTTP 状态码是 200。
func (s *Service) RunExport(ctx context.Context, exportID int64, log *slog.Logger) (
	ExportOutcome, error) {
	task, err := s.repo.ClaimExport(ctx, exportID)
	if errors.Is(err, repo.ErrExportBusy) || errors.Is(err, repo.ErrNotFound) {
		return ExportDuplicate, nil
	}
	if err != nil {
		return ExportRetry, err
	}

	mediaID, size, err := s.buildArchive(ctx, task)
	if err != nil {
		// 失败要落**错误原因**：只标 failed 的话用户只看到"导出失败"，
		// 再点一次还是失败，而他无从知道是哪一项取不回来。
		code, msg := errcode.Internal.Name(), err.Error()
		var be *httpx.BizError
		if errors.As(err, &be) {
			code, msg = be.Code.Name(), be.Message
		}
		if ferr := s.repo.FailExport(ctx, task.ID, code, msg); ferr != nil {
			log.Error("标记导出失败时出错", "exportId", task.ID, "error", ferr)
		}
		log.Warn("导出失败", "exportId", task.ID, "error", err)
		// ⚠️ 已经判失败的任务**不再重投**：重投只会重复失败，
		// 而库里那行已经带着原因，运维按状态就能捞到（与生成任务同一条规则）。
		return ExportDone, nil
	}

	if err := s.repo.FinishExport(ctx, task.ID, mediaID, size); err != nil {
		// 包已经传上去了但状态没写成 —— 下一轮重投会重新打一个包，
		// 上一个成了无引用的孤儿，由媒体回收收掉。
		return ExportRetry, err
	}
	log.Info("导出完成", "exportId", task.ID, "items", task.ItemCount, "bytes", size)
	return ExportDone, nil
}

func (s *Service) buildArchive(ctx context.Context, task repo.ExportTask) (int64, int64, error) {
	var items []domain.ExportItem
	if err := json.Unmarshal(task.Items, &items); err != nil {
		return 0, 0, httpx.Errf(errcode.Internal, "导出清单已损坏")
	}

	// 先把每一项的 MIME 查出来，文件名才有扩展名。
	mimes := make(map[int64]string, len(items))
	for _, it := range items {
		m, err := s.repo.GetScoped(ctx, task.WorkspaceID, it.MediaID)
		if err != nil {
			// ⚠️ 受理之后、打包之前被删掉是完全可能的。明确失败并说是哪一项，
			// 而不是跳过 —— 静默跳过的话用户拿到一个少了几张图的包，
			// 而任务显示"成功"。
			return 0, 0, httpx.Errf(errcode.NotFound,
				fmt.Sprintf("文件 %d 已不存在，导出中止", it.MediaID))
		}
		mimes[it.MediaID] = m.ContentType
	}
	entries := domain.PlanEntries(items, func(id int64) string { return mimes[id] })

	tmp, err := os.CreateTemp("", "seeaitv-export-*.zip")
	if err != nil {
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}
	defer func() {
		_ = tmp.Close()
		// ⚠️ 临时文件**一定要删**（NEW-15 点名了"临时文件清理"）：
		// 每次导出留一个几百 MB 的文件，几天就把 Worker 的磁盘写满，
		// 而症状是"所有任务都失败了"，看着完全不像磁盘问题。
		_ = os.Remove(tmp.Name())
	}()

	zw := zip.NewWriter(tmp)
	for _, e := range entries {
		body, _, err := s.Open(ctx, e.MediaID)
		if err != nil {
			_ = zw.Close()
			return 0, 0, httpx.Errf(errcode.Internal,
				fmt.Sprintf("读取 %s 失败，导出中止", e.Path))
		}
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name: e.Path,
			// Deflate 对已经压过的 JPEG/MP4 几乎没有收益，却要花满 CPU。
			// 但混合内容（PNG、文本）仍然压得动，所以统一用 Deflate ——
			// 逐项判类型的复杂度换不回这点带宽。
			Method:   zip.Deflate,
			Modified: time.Now(),
		})
		if err != nil {
			_ = body.Close()
			_ = zw.Close()
			return 0, 0, httpx.Wrap(errcode.Internal, err)
		}
		_, copyErr := io.Copy(w, body)
		_ = body.Close()
		if copyErr != nil {
			_ = zw.Close()
			return 0, 0, httpx.Wrap(errcode.Internal, copyErr)
		}
	}
	if err := zw.Close(); err != nil {
		// ⚠️ zip 的中央目录在**关闭时**才写进去。不检查这个错误的话，
		// 会传上去一个字节数看着正常、却解不开的文件。
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}

	stat, err := tmp.Stat()
	if err != nil {
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}

	id := s.nextID()
	key := objectKey("export_archive", id, "application/zip")
	if err := s.store.PutFile(ctx, key, tmp, stat.Size(), "application/zip"); err != nil {
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}
	if err := s.repo.Insert(ctx, repo.MediaObject{
		ID: id, WorkspaceID: task.WorkspaceID, Bucket: s.store.Bucket(), ObjectKey: key,
		ContentType: "application/zip", SizeBytes: stat.Size(),
		Purpose: "export_archive", CreatedByUserID: task.UserID,
	}); err != nil {
		return 0, 0, httpx.Wrap(errcode.Internal, err)
	}
	return id, stat.Size(), nil
}

// ---------------------------------------------------------------- 过期清理

// ExportCleaner 删除已过期的压缩包。
//
// ⚠️ 删的是**包**不是任务行：任务行留着，用户点开历史看到的是"已过期"
// 而不是"这次导出从没发生过"。而对外状态是**算出来的**
// （domain.EffectiveExportStatus），所以这个作业停了也不会有人下到 404 ——
// 它只影响存储成本，不影响正确性。
type ExportCleaner struct {
	svc      *Service
	log      *slog.Logger
	batch    int
	interval time.Duration
}

func NewExportCleaner(svc *Service, log *slog.Logger) *ExportCleaner {
	return &ExportCleaner{svc: svc, log: log, batch: 100, interval: time.Hour}
}

func (c *ExportCleaner) SetInterval(d time.Duration) { c.interval = d }

func (c *ExportCleaner) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	c.Once(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Once(ctx)
		}
	}
}

// Once 清一轮，返回删掉的包数。
func (c *ExportCleaner) Once(ctx context.Context) int {
	list, err := c.svc.repo.FindExpiredExports(ctx, time.Now(), c.batch)
	if err != nil {
		c.log.Error("过期导出扫描失败", "error", err)
		return 0
	}
	n := 0
	for _, e := range list {
		// 顺序与媒体回收一致：**先删对象，再删库行**。
		if err := c.svc.store.Delete(ctx, e.ObjectKey); err != nil {
			c.log.Warn("删除过期压缩包失败，下轮重试", "exportId", e.TaskID, "error", err)
			continue
		}
		if err := c.svc.repo.ClearExportMedia(ctx, e.TaskID); err != nil {
			c.log.Error("清空导出引用失败", "exportId", e.TaskID, "error", err)
			continue
		}
		if err := c.svc.repo.DeleteRow(ctx, e.MediaID); err != nil {
			c.log.Error("删除压缩包记录失败", "mediaId", e.MediaID, "error", err)
			continue
		}
		n++
	}
	if n > 0 {
		c.log.Info("过期导出清理完成", "deleted", n)
	}
	return n
}
