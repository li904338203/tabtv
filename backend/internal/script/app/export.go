package app

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/li904338203/tabtv/backend/internal/script/domain"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// 脚本 JSON 导出（AV-19）。
//
// ⚠️ 导出的形状是**对用户的承诺**，不是内部结构的投影：文件会被下载、
// 被改、被别人拿去用。直接把仓储结构体序列化出去的话，哪天加一个
// 内部列（比如指纹、乐观锁版本）就会漏进用户的文件里，
// 而删一个列会让他早先导出的文件对不上。所以这里**显式列字段**。

// ExportSchemaVersion 是导出格式的版本。
//
// ⚠️ 与脚本的乐观锁 `version` 是两件事（GO-36 同款区分）：
// 这个描述的是"文件长什么样"，那个描述的是"数据改到第几次了"。
// 混用的结果是导入方拿乐观锁版本去判断格式兼容性。
const ExportSchemaVersion = 1

// ScriptExport 是一份可下载的完整脚本。
type ScriptExport struct {
	SchemaVersion int             `json:"schemaVersion"`
	ExportedAt    string          `json:"exportedAt"`
	Script        ExportScript    `json:"script"`
	Shots         []ExportShot    `json:"shots"`
	Assets        []ExportAsset   `json:"assets"`
	Groups        []ExportGroup   `json:"groups"`
	Warnings      []string        `json:"warnings,omitempty"`
	AssetProfiles json.RawMessage `json:"assetProfiles,omitempty"`
}

type ExportScript struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	NodeID      string `json:"canvasNodeId"`
	SourceType  string `json:"sourceType"`
	CurrentStep int    `json:"currentStep"`
	RawInput    string `json:"rawInput"`
	GlobalStyle string `json:"globalStyle"`
}

type ExportShot struct {
	ID          string          `json:"id"`
	ShotNo      int             `json:"shotNo"`
	DurationSec int             `json:"durationSec"`
	Description string          `json:"description"`
	ShotSize    string          `json:"shotSize,omitempty"`
	Lighting    string          `json:"lighting,omitempty"`
	CameraMove  string          `json:"cameraMove,omitempty"`
	Dialogue    string          `json:"dialogue,omitempty"`
	SoundEffect string          `json:"soundEffect,omitempty"`
	StartState  string          `json:"startState,omitempty"`
	EndState    string          `json:"endState,omitempty"`
	Entities    json.RawMessage `json:"entities,omitempty"`
	// ImagePrompt / VideoPrompt 带**有效状态**，不是库里存的那一列。
	ImagePrompt ExportPrompt `json:"imagePrompt"`
	VideoPrompt ExportPrompt `json:"videoPrompt"`
	// AssetRefs 按引用顺序 —— 顺序即参考图的锚定顺序，丢了就没法复现。
	AssetRefs []string `json:"assetRefs,omitempty"`
}

type ExportPrompt struct {
	Text   string `json:"text"`
	Status string `json:"status"`
	Source string `json:"source,omitempty"`
}

type ExportAsset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Source      string `json:"source,omitempty"`
	// MediaID 只给 ID，**不给 URL**：签名地址几小时就失效，
	// 写进导出文件等于给用户一堆第二天就打不开的链接。
	MediaID string `json:"mediaId,omitempty"`
}

type ExportGroup struct {
	ID        string            `json:"id"`
	NodeID    string            `json:"canvasNodeId"`
	GroupType string            `json:"groupType"`
	Name      string            `json:"name,omitempty"`
	Status    string            `json:"status"`
	Items     []ExportGroupItem `json:"items"`
}

type ExportGroupItem struct {
	ItemNo       int             `json:"itemNo"`
	ShotID       string          `json:"shotId,omitempty"`
	AssetID      string          `json:"assetId,omitempty"`
	GenerationID string          `json:"generationId,omitempty"`
	Status       string          `json:"status"`
	CreditsCost  int             `json:"creditsCost"`
	Params       json.RawMessage `json:"params,omitempty"`
	Inputs       json.RawMessage `json:"inputs,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
}

// Export 导出一个脚本的全量内容（AV-19）。
//
// ⚠️ 导出的是**当前状态的快照**，包含未完成与失败的部分。
// 只导成功的那些看起来更"干净"，但用户下载脚本正是为了留档和排查 ——
// 把失败项藏起来，他就无从知道哪一镜没出来、为什么。
func (s *Service) Export(ctx context.Context, workspaceID, scriptID int64, now string) (
	ScriptExport, error) {
	sc, err := s.repo.Get(ctx, workspaceID, scriptID)
	if err != nil {
		return ScriptExport{}, translate(err)
	}
	shots, err := s.repo.ListShots(ctx, scriptID)
	if err != nil {
		return ScriptExport{}, httpx.Wrap(errcode.Internal, err)
	}
	refs, err := s.repo.ListShotRefs(ctx, scriptID)
	if err != nil {
		return ScriptExport{}, httpx.Wrap(errcode.Internal, err)
	}
	assets, err := s.repo.ListAssetsByScript(ctx, scriptID)
	if err != nil {
		return ScriptExport{}, httpx.Wrap(errcode.Internal, err)
	}
	// 提示词状态要用**算出来的**那份：库里可能还停在 ready，
	// 而上游早就改过了。导出一份标着 ready 的过期词比不导更糟。
	prompts, err := s.Prompts(ctx, workspaceID, scriptID)
	if err != nil {
		return ScriptExport{}, err
	}
	byShot := map[int64]map[domain.PromptKind]PromptView{}
	for _, p := range prompts {
		if byShot[p.ShotID] == nil {
			byShot[p.ShotID] = map[domain.PromptKind]PromptView{}
		}
		byShot[p.ShotID][p.Kind] = p
	}

	out := ScriptExport{
		SchemaVersion: ExportSchemaVersion, ExportedAt: now,
		Script: ExportScript{
			ID: itoa(sc.ID), ProjectID: itoa(sc.ProjectID), NodeID: sc.CanvasNodeID,
			SourceType: sc.SourceType, CurrentStep: sc.CurrentStep,
			RawInput: sc.RawInput, GlobalStyle: sc.GlobalStyle,
		},
		AssetProfiles: sc.AssetProfiles,
		Shots:         make([]ExportShot, 0, len(shots)),
		Assets:        make([]ExportAsset, 0, len(assets)),
		Groups:        []ExportGroup{},
	}

	var stale, missing int
	for _, sh := range shots {
		e := ExportShot{
			ID: itoa(sh.ID), ShotNo: sh.ShotNo, DurationSec: sh.DurationSec,
			Description: sh.Description, ShotSize: sh.ShotSize, Lighting: sh.Lighting,
			CameraMove: sh.CameraMove, Dialogue: sh.Dialogue,
			SoundEffect: sh.SoundEffect, StartState: sh.StartState,
			EndState: sh.EndState, Entities: sh.Entities,
			ImagePrompt: exportPrompt(byShot[sh.ID][domain.PromptImage]),
			VideoPrompt: exportPrompt(byShot[sh.ID][domain.PromptVideo]),
		}
		for _, ref := range refs[sh.ID] {
			e.AssetRefs = append(e.AssetRefs, itoa(ref.AssetID))
		}
		for _, p := range []ExportPrompt{e.ImagePrompt, e.VideoPrompt} {
			switch domain.PromptStatus(p.Status) {
			case domain.PromptStale:
				stale++
			case domain.PromptPending, domain.PromptFailed:
				missing++
			}
		}
		out.Shots = append(out.Shots, e)
	}

	for _, a := range assets {
		e := ExportAsset{
			ID: itoa(a.ID), Name: a.Name, Type: a.Type, Description: a.Description,
			Status: a.Status, Source: a.Source,
		}
		if a.MediaObjectID != 0 {
			e.MediaID = itoa(a.MediaObjectID)
		}
		out.Assets = append(out.Assets, e)
	}

	groups, err := s.repo.ListGroups(ctx, workspaceID, scriptID)
	if err != nil {
		return ScriptExport{}, httpx.Wrap(errcode.Internal, err)
	}
	for _, g := range groups {
		items, err := s.repo.ListItems(ctx, g.ID)
		if err != nil {
			return ScriptExport{}, httpx.Wrap(errcode.Internal, err)
		}
		eg := ExportGroup{
			ID: itoa(g.ID), NodeID: g.CanvasNodeID, GroupType: g.GroupType,
			Name: g.Name, Status: g.Status,
			Items: make([]ExportGroupItem, 0, len(items)),
		}
		for _, it := range items {
			ei := ExportGroupItem{
				ItemNo: it.ItemNo, Status: it.Status, CreditsCost: it.CreditsCost,
				Params: it.ParamsSnapshot, Inputs: it.InputRefs,
				ErrorMessage: it.ErrorMessage,
			}
			if it.ShotID != 0 {
				ei.ShotID = itoa(it.ShotID)
			}
			if it.ScriptAssetID != 0 {
				ei.AssetID = itoa(it.ScriptAssetID)
			}
			if it.GenerationID != 0 {
				ei.GenerationID = itoa(it.GenerationID)
			}
			eg.Items = append(eg.Items, ei)
		}
		out.Groups = append(out.Groups, eg)
	}

	// ⚠️ 把"这份导出不完整"**写进文件本身**。只在界面上提示的话，
	// 文件传给别人、或者半年后自己再打开，就没人知道当时有几镜没出来。
	if stale > 0 {
		out.Warnings = append(out.Warnings,
			"有 "+strconv.Itoa(stale)+" 条提示词已过期（上游在合成之后改过），导出的是旧内容")
	}
	if missing > 0 {
		out.Warnings = append(out.Warnings,
			"有 "+strconv.Itoa(missing)+" 条提示词尚未合成或合成失败")
	}
	return out, nil
}

func exportPrompt(v PromptView) ExportPrompt {
	status := string(v.Status)
	if status == "" {
		status = string(domain.PromptPending)
	}
	return ExportPrompt{Text: v.Text, Status: status, Source: string(v.Source)}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
