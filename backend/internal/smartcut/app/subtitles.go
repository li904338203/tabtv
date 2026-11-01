package app

import (
	"context"
	"encoding/json"

	"github.com/li904338203/tabtv/backend/internal/smartcut/domain"
)

// 识别字幕的写入（docs/agent/smartcut SCT-36，SC-F-120）。
//
// 识别时选了「生成字幕」：已有识别结果的素材在受理那一刻就写（不会再有任务完成来触发它），其余的在各自的识别
// 任务完成时由回写写入。每次只写这一批素材的片段 —— 别的片段的字幕不动，几份素材先后识别完就先后写进去，
// 不必等「全部识别完」（那要记一份跨任务的状态，而少写一段比多等一份更好排查）。
// 写入走时间线的整份保存（校验、快照 subtitles、乐观锁、timeline.updated），编辑者记 system；
// 版本对不上（剪辑台里刚存过一次）时重读重放，最多 subtitleWriteAttempts 次。

// subtitleWriteAttempts 识别字幕写入的最多尝试次数（版本冲突时重读重放）。
const subtitleWriteAttempts = 3

// asrSubtitleWrite 一次识别字幕写入。
type asrSubtitleWrite struct {
	workspaceID, projectID, timelineID, userID int64
	// clipIDs 只写这几段（空 = 画面轨与配音轨的全部片段）。
	clipIDs []string
	// mediaIDs 只写用到这几份素材的片段。
	mediaIDs          []int64
	engine, paramsKey string
}

// writeASRSubtitles 把这批素材的识别结果写成字幕。没有可写的（没有结果、什么都没变）时不写。
func (s *Service) writeASRSubtitles(ctx context.Context, w asrSubtitleWrite) error {
	p := s.transcribe
	if p.Timelines == nil || p.Media == nil || len(w.mediaIDs) == 0 {
		return nil
	}
	rows, err := p.Media.ASRRows(ctx, w.workspaceID, w.mediaIDs, w.engine, w.paramsKey)
	if err != nil {
		return err
	}
	trs := map[int64]domain.Transcript{}
	lang := ""
	for _, id := range w.mediaIDs {
		row, ok := rows[id]
		if !ok || row.Status != "success" {
			continue
		}
		var tr domain.Transcript
		if json.Unmarshal(row.Result, &tr) != nil {
			continue
		}
		if tr.Language == "" {
			tr.Language = domain.ASRParamsLang(w.paramsKey)
		}
		trs[id] = tr
		if lang == "" {
			lang = domain.NormalizeLang(tr.Language)
		}
	}
	if len(trs) == 0 {
		return nil
	}
	editor := Editor{UserID: w.userID, Kind: "system"}
	for attempt := 1; ; attempt++ {
		content, err := p.Timelines.Content(ctx, w.workspaceID, w.timelineID)
		if err != nil {
			return err
		}
		subs, covered := domain.ASRSubtitles(content, w.clipIDs, trs)
		merged, changed, _ := domain.MergeASRSubtitles(content, subs, covered, lang)
		if !changed {
			return nil
		}
		_, err = p.Timelines.Save(ctx, SavePlanInput{WorkspaceID: w.workspaceID, TimelineID: w.timelineID, Editor: editor,
			Content: merged, Reason: "subtitles", SnapshotReason: "subtitles"})
		if err == nil || !isVersionConflict(err) || attempt >= subtitleWriteAttempts {
			return err
		}
	}
}
