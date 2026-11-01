package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/script/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 镜头表编辑（AV-07）：增删、复制、上下移动、排序。

// SaveShotInput 是一次镜头保存。
type SaveShotInput struct {
	WorkspaceID     int64
	ScriptID        int64
	ShotID          int64
	ExpectedVersion int
	DurationSec     *int
	Description     *string
	ShotSize        *string
	Lighting        *string
	Dialogue        *string
	SoundEffect     *string
	CameraMove      *string
	StartState      *string
	EndState        *string
	Entities        json.RawMessage
}

// SaveShot 保存一个镜头。
func (s *Service) SaveShot(ctx context.Context, in SaveShotInput) (repo.Shot, error) {
	if _, err := s.repo.Get(ctx, in.WorkspaceID, in.ScriptID); err != nil {
		return repo.Shot{}, translate(err)
	}
	if in.DurationSec != nil {
		if err := domain.ValidateShot(domain.Shot{DurationSec: *in.DurationSec}); err != nil {
			return repo.Shot{}, httpx.Errf(errcode.InvalidArgument, err.Error())
		}
	}
	if _, err := s.repo.SaveShot(ctx, repo.SaveShotInput{
		ScriptID: in.ScriptID, ShotID: in.ShotID, ExpectedVersion: in.ExpectedVersion,
		DurationSec: in.DurationSec, Description: in.Description, ShotSize: in.ShotSize,
		Lighting: in.Lighting, Dialogue: in.Dialogue, SoundEffect: in.SoundEffect,
		CameraMove: in.CameraMove, StartState: in.StartState, EndState: in.EndState,
		Entities: in.Entities,
	}); err != nil {
		return repo.Shot{}, translateShot(err)
	}
	// ⚠️ 改了镜头内容，它的提示词立刻变灰（AV-16）。
	// 这是**即时反馈**，不是 stale 的正确性来源 —— 正确性靠行上的指纹
	// （读的时候比一比），漏掉任何一个调用点都不会有编译错误。
	if err := s.repo.MarkPromptsStale(ctx, in.ShotID); err != nil {
		s.warn(ctx, "标记提示词过期失败", "shotId", in.ShotID, "error", err)
	}
	shot, err := s.repo.GetShot(ctx, in.ScriptID, in.ShotID)
	if err != nil {
		return repo.Shot{}, translateShot(err)
	}
	// ⚠️ 组里那些**还没跑**的子项也要一起标过期（FLOW-05）：它们带的是
	// 建组时那份快照，照旧跑出来的画面是用户已经改掉的版本。
	// 这同样只是即时反馈 —— 真正的保护是提交前比对 shotVersion。
	if err := s.repo.MarkItemsStale(ctx, in.ShotID, shot.Version); err != nil {
		s.warn(ctx, "标记组子项过期失败", "shotId", in.ShotID, "error", err)
	}
	return shot, nil
}

// AddShotInput 新增一个镜头。AfterShotID 为 0 表示插到最前面。
type AddShotInput struct {
	WorkspaceID int64
	ScriptID    int64
	AfterShotID int64
	DurationSec int
	Description string
}

// AddShot 插入一个空镜头。
func (s *Service) AddShot(ctx context.Context, in AddShotInput) (Detail, error) {
	sc, err := s.repo.Get(ctx, in.WorkspaceID, in.ScriptID)
	if err != nil {
		return Detail{}, translate(err)
	}
	shots, err := s.loadShots(ctx, sc.ID)
	if err != nil {
		return Detail{}, err
	}
	index := -1
	if in.AfterShotID != 0 {
		if index = domain.IndexOfShot(shots, in.AfterShotID); index < 0 {
			return Detail{}, httpx.Errf(errcode.NotFound, "插入位置的镜头不存在")
		}
	}
	duration := in.DurationSec
	if duration == 0 {
		duration = 5
	}
	next, err := domain.InsertAfter(shots, index, domain.Shot{
		DurationSec: duration, Description: in.Description,
	})
	if err != nil {
		return Detail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return s.persistOrder(ctx, sc, shots, next)
}

// DuplicateShot 复制一个镜头。
func (s *Service) DuplicateShot(ctx context.Context, workspaceID, scriptID, shotID int64) (
	Detail, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return Detail{}, translate(err)
	}
	shots, err := s.loadShots(ctx, sc.ID)
	if err != nil {
		return Detail{}, err
	}
	index := domain.IndexOfShot(shots, shotID)
	if index < 0 {
		return Detail{}, httpx.Err(errcode.NotFound)
	}
	next, err := domain.Duplicate(shots, index)
	if err != nil {
		return Detail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return s.persistOrder(ctx, sc, shots, next)
}

// MoveShot 上移/下移一个镜头。
func (s *Service) MoveShot(ctx context.Context, workspaceID, scriptID, shotID int64,
	dir string) (Detail, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return Detail{}, translate(err)
	}
	shots, err := s.loadShots(ctx, sc.ID)
	if err != nil {
		return Detail{}, err
	}
	index := domain.IndexOfShot(shots, shotID)
	if index < 0 {
		return Detail{}, httpx.Err(errcode.NotFound)
	}
	next, err := domain.Reorder(shots, index, domain.Move(dir))
	if err != nil {
		return Detail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return s.persistOrder(ctx, sc, shots, next)
}

// DeleteShot 删除一个镜头并重排。
func (s *Service) DeleteShot(ctx context.Context, workspaceID, scriptID, shotID int64) (
	Detail, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return Detail{}, translate(err)
	}
	if err := s.repo.DeleteShot(ctx, scriptID, shotID); err != nil {
		return Detail{}, translateShot(err)
	}
	shots, err := s.loadShots(ctx, sc.ID)
	if err != nil {
		return Detail{}, err
	}
	// 删完直接按现有顺序重排镜号：镜号必须连续。
	ids := make([]int64, 0, len(shots))
	for _, s := range shots {
		ids = append(ids, s.ID)
	}
	if len(ids) > 0 {
		if err := s.repo.ApplyOrder(ctx, sc.ID, ids); err != nil {
			return Detail{}, httpx.Wrap(errcode.Internal, err)
		}
	}
	return s.detail(ctx, sc)
}

// ReorderShots 按给定的镜头 ID 顺序重排。
func (s *Service) ReorderShots(ctx context.Context, workspaceID, scriptID int64,
	orderedIDs []int64) (Detail, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return Detail{}, translate(err)
	}
	if err := s.repo.ApplyOrder(ctx, sc.ID, orderedIDs); err != nil {
		// ⚠️ 少传 ID 是**调用方的错**，不是内部错误：仓储会检查有没有
		// 镜头被漏在负数镜号上，漏了就整体回滚。
		return Detail{}, httpx.Errf(errcode.InvalidArgument, err.Error())
	}
	return s.detail(ctx, sc)
}

func (s *Service) loadShots(ctx context.Context, scriptID int64) ([]domain.Shot, error) {
	rows, err := s.repo.ListShots(ctx, scriptID)
	if err != nil {
		return nil, httpx.Wrap(errcode.Internal, err)
	}
	out := make([]domain.Shot, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.Shot{
			ID: r.ID, ShotNo: r.ShotNo, DurationSec: r.DurationSec,
			Description: r.Description, Version: r.Version,
		})
	}
	return out, nil
}

// persistOrder 把领域层算好的新顺序落库。
//
// ⚠️ 新增的镜头（ID 为 0）要先插入拿到 ID，再统一重排 ——
// 反过来的话新镜头没有 ID，重排时无从点名。
func (s *Service) persistOrder(ctx context.Context, sc repo.Script,
	before, after []domain.Shot) (Detail, error) {
	ids := make([]int64, 0, len(after))
	inserts := make([]repo.Shot, 0, 1)
	for i := range after {
		if after[i].ID == 0 {
			after[i].ID = s.nextID()
			inserts = append(inserts, repo.Shot{
				ID: after[i].ID, ScriptID: sc.ID, DurationSec: after[i].DurationSec,
				Description: after[i].Description,
			})
		}
		ids = append(ids, after[i].ID)
	}
	// ⚠️ 插入与重排**必须在一个事务里**：分开的话，重排失败时插入已经提交，
	// 那一行会永远留在临时镜号上 —— 它在列表里排到最前面，
	// 而且下一次插入会撞上同一个占位号，这个脚本从此再也插不进镜头。
	if err := s.repo.InsertAndReorder(ctx, sc.ID, inserts, ids); err != nil {
		return Detail{}, httpx.Wrap(errcode.Internal, err)
	}
	return s.detail(ctx, sc)
}

func translateShot(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, repo.ErrVersionConflict):
		return httpx.Errf(errcode.VersionConflict, "镜头已被他人修改，请刷新后重试")
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Errf(errcode.NotFound, "镜头不存在")
	}
	return httpx.Wrap(errcode.Internal, err)
}
