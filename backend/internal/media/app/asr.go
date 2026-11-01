package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/li904338203/tabtv/backend/internal/media/domain"
	"github.com/li904338203/tabtv/backend/internal/media/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 识别结果的分析行（docs/agent/smartcut SCT-33）：智能剪辑经这几个方法排、挂、回写识别；规则见 media/repo/asr.go。

// SpeechAudio 取一批素材的识别用音轨状态。
func (s *Service) SpeechAudio(ctx context.Context, workspaceID int64, mediaIDs []int64) (map[int64]repo.SpeechAudioState, error) {
	return s.repo.SpeechAudio(ctx, workspaceID, mediaIDs, domain.AnalysisEngine(domain.AnalysisSpeechAudio))
}

// ASRRows 取一批素材在某组参数下非失败的识别行。
func (s *Service) ASRRows(ctx context.Context, workspaceID int64, mediaIDs []int64, engine, paramsKey string) (
	map[int64]repo.Analysis, error) {
	return s.repo.ASRRows(ctx, workspaceID, mediaIDs, engine, paramsKey)
}

// InsertASR 排一行识别；同键已有非失败的行时返回那一行与 false。素材不在本工作区时 404。
func (s *Service) InsertASR(ctx context.Context, workspaceID, mediaID int64, engine, paramsKey string,
	pending json.RawMessage) (repo.Analysis, bool, error) {
	a, inserted, err := s.repo.InsertASR(ctx, s.nextID(), workspaceID, mediaID, engine, paramsKey, pending)
	if errors.Is(err, repo.ErrNotFound) {
		return repo.Analysis{}, false, httpx.Errf(errcode.NotFound, "素材不存在或已删除")
	}
	return a, inserted, err
}

func (s *Service) AttachASR(ctx context.Context, id, generationID int64) (bool, error) {
	return s.repo.AttachASR(ctx, id, generationID)
}

func (s *Service) FailASR(ctx context.Context, id int64, msg string) (bool, error) {
	return s.repo.FailASR(ctx, id, msg)
}

func (s *Service) SucceedASR(ctx context.Context, id int64, result json.RawMessage) (bool, error) {
	return s.repo.SucceedASR(ctx, id, result)
}

func (s *Service) FailedASRCount(ctx context.Context, mediaID int64, engine, paramsKey string) (int, error) {
	return s.repo.FailedASRCount(ctx, mediaID, engine, paramsKey)
}

// ListPendingASR 等着受理的识别（连同识别用音轨的状态）。
func (s *Service) ListPendingASR(ctx context.Context, limit int) ([]repo.PendingASR, error) {
	return s.repo.ListPendingASR(ctx, domain.AnalysisEngine(domain.AnalysisSpeechAudio), limit)
}

func (s *Service) ClaimPendingASR(ctx context.Context, id int64, lease time.Duration) (bool, error) {
	return s.repo.ClaimPendingASR(ctx, id, lease)
}

// FinishASR 识别任务到终态时回写；见 repo.FinishASR。
func (s *Service) FinishASR(ctx context.Context, generationID, workspaceID, mediaID int64, engine, paramsKey,
	status string, result json.RawMessage, msg string) (bool, error) {
	return s.repo.FinishASR(ctx, s.nextID(), generationID, workspaceID, mediaID, engine, paramsKey, status, result, msg)
}

// ASRSuccessRows 一批素材全部成功的识别行（最新的在前）。
func (s *Service) ASRSuccessRows(ctx context.Context, workspaceID int64, mediaIDs []int64) ([]repo.Analysis, error) {
	return s.repo.ASRSuccessRows(ctx, workspaceID, mediaIDs)
}
