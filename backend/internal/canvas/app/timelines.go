package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// 项目复制与清理时带上智能剪辑的时间线（docs/agent/smartcut SCT-07，SC-F-170 / SC-F-171、SCB-11）。
//
// 时间线属于时间线域：画布域不碰它的表，只在**自己的事务里**调用这两个端口（由时间线仓储实现，wiring 接上）。
// 与建副本 / 删项目同一个事务是硬要求：分开做的话中间失败会留下「副本节点指向源项目的时间线」，
// 或者「项目删了、时间线还在」—— 后者正是 SCB-11：被清理项目的时间线片段永远挡住素材删除。

// ProjectTimelines 是时间线域在项目复制 / 清理时的两个动作。
type ProjectTimelines interface {
	// CopyForProjectInTx 在调用方的事务里把源项目绑定节点的时间线复制到新项目，返回「源 → 副本」。
	CopyForProjectInTx(ctx context.Context, tx pgx.Tx, workspaceID, srcProjectID, dstProjectID int64) (map[int64]int64, error)
	// PurgeProjectsInTx 在调用方的事务里删掉这些项目的全部时间线及其从属行。
	PurgeProjectsInTx(ctx context.Context, tx pgx.Tx, projectIDs []int64) error
}

// TimelineSweeper 清理孤儿时间线（节点被删、或所属项目已不存在，SC-F-06），返回删了几条。
type TimelineSweeper interface {
	SweepOrphans(ctx context.Context, before time.Time, limit int) (int, error)
}

// SetProjectTimelines 接上时间线域。为 nil 时复制项目不复制时间线、清理项目不删时间线（旧行为）。
func (s *Service) SetProjectTimelines(t ProjectTimelines) { s.timelines = t }

// purgeHook 给仓储的清理事务用的回调；没接时间线域时为 nil。
func (s *Service) purgeHook() func(context.Context, pgx.Tx, []int64) error {
	if s.timelines == nil {
		return nil
	}
	return s.timelines.PurgeProjectsInTx
}
