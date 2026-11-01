package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/li904338203/tabtv/backend/internal/content/repo"
	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// ---------------------------------------------------------------- 我的资产（迁移 00074）
//
// 资产 = 保存下来的一项生成产物（文本 / 图片 / 视频 / 音频）。画布右键菜单「保存到我的资产」存进来，画布左侧「资产」页签与
// 创作资产页（/works）只展示保存过的；可以分文件夹（单层）、打标签、按标签筛选；评级挂在生成结果上，整个工作区共用。

// PresetLibraryTags 预置标签（写在代码里，每个工作区都有）。「新建标签」建出来的存 library_tag。
var PresetLibraryTags = []string{"角色", "场景", "物品", "风格", "音效"}

const (
	libraryTitleMax  = 120
	libraryFolderMax = 60
	libraryTagMax    = 20
	libraryTagsPer   = 10
	libraryBatchMax  = 200
)

var libraryKinds = map[string]bool{"text": true, "image": true, "video": true, "audio": true}

func libraryErr(err error) error {
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return httpx.Err(errcode.NotFound)
	case errors.Is(err, repo.ErrConflict):
		return httpx.Errf(errcode.DuplicateName, "已经有同名的文件夹了")
	case errors.Is(err, repo.ErrNotSavable):
		return httpx.Errf(errcode.InvalidArgument, "这项生成结果不能保存：不是本工作区的、还没生成成功，或者没有这一项产物")
	default:
		return httpx.Wrap(errcode.Internal, err)
	}
}

// cleanName 去掉首尾空白，按字符（不是字节）限长；空 / 超长都报错并说清上限。
func cleanName(label, s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", httpx.Errf(errcode.InvalidArgument, label+"不能为空")
	}
	if utf8.RuneCountInString(s) > max {
		return "", httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("%s最多 %d 个字", label, max))
	}
	return s, nil
}

// LibraryTags 预置标签 + 本工作区新建的（去重、保持先后）。
func (s *Service) LibraryTags(ctx context.Context, workspaceID int64) ([]string, []string, error) {
	custom, err := s.repo.ListLibraryTags(ctx, workspaceID)
	if err != nil {
		return nil, nil, libraryErr(err)
	}
	preset := map[string]bool{}
	for _, t := range PresetLibraryTags {
		preset[t] = true
	}
	out := []string{}
	for _, t := range custom {
		if !preset[t] {
			out = append(out, t)
		}
	}
	return PresetLibraryTags, out, nil
}

// normalizeTags 清洗一组标签；不认识的（不是预置、也还没建过）顺手建成自定义标签 —— 在资产上直接打一个新标签是常见操作，
// 逼用户先去筛选框里「新建标签」再回来选，多一步而没有任何好处。
func (s *Service) normalizeTags(ctx context.Context, workspaceID int64, tags []string) ([]string, error) {
	if tags == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		name, err := cleanName("标签", t, libraryTagMax)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) > libraryTagsPer {
		return nil, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("一项资产最多 %d 个标签", libraryTagsPer))
	}
	preset, custom, err := s.LibraryTags(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, t := range append(append([]string{}, preset...), custom...) {
		known[t] = true
	}
	for _, t := range out {
		if !known[t] {
			if err := s.repo.CreateLibraryTag(ctx, workspaceID, t); err != nil {
				return nil, libraryErr(err)
			}
		}
	}
	return out, nil
}

// CreateLibraryTag 「新建标签」。和预置重名时什么都不做（它本来就在）。
func (s *Service) CreateLibraryTag(ctx context.Context, workspaceID int64, name string) (string, error) {
	n, err := cleanName("标签", name, libraryTagMax)
	if err != nil {
		return "", err
	}
	for _, t := range PresetLibraryTags {
		if t == n {
			return n, nil
		}
	}
	if err := s.repo.CreateLibraryTag(ctx, workspaceID, n); err != nil {
		return "", libraryErr(err)
	}
	return n, nil
}

// DeleteLibraryTag 删自定义标签（预置的删不掉）。
func (s *Service) DeleteLibraryTag(ctx context.Context, workspaceID int64, name string) error {
	for _, t := range PresetLibraryTags {
		if t == name {
			return httpx.Errf(errcode.InvalidArgument, "预置标签不能删除")
		}
	}
	if err := s.repo.DeleteLibraryTag(ctx, workspaceID, name); err != nil {
		return libraryErr(err)
	}
	return nil
}

// LibraryQuery 一次资产列表查询（来自查询参数）。
type LibraryQuery struct {
	FolderID     *int64
	Tags         []string
	Query        string
	Kind         string
	MinRating    int
	GenerationID int64
	Limit        int
	CursorID     int64
}

type LibraryPage struct {
	Items   []repo.LibraryItem
	HasMore bool
	Total   int
}

func (s *Service) LibraryItems(ctx context.Context, workspaceID int64, q LibraryQuery) (LibraryPage, error) {
	if q.Kind != "" && !libraryKinds[q.Kind] {
		return LibraryPage{}, httpx.Errf(errcode.InvalidArgument, "资产类型非法（只支持 text / image / video / audio）")
	}
	if q.MinRating < 0 || q.MinRating > 5 {
		return LibraryPage{}, httpx.Errf(errcode.InvalidArgument, "评级筛选只能是 0~5")
	}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	f := repo.LibraryFilter{
		WorkspaceID: workspaceID, FolderID: q.FolderID, Tags: q.Tags, Query: q.Query, Kind: q.Kind,
		MinRating: q.MinRating, GenerationID: q.GenerationID, Limit: q.Limit + 1, CursorID: q.CursorID,
	}
	rows, err := s.repo.ListLibraryItems(ctx, f)
	if err != nil {
		return LibraryPage{}, libraryErr(err)
	}
	page := LibraryPage{Items: rows}
	if len(rows) > q.Limit {
		page.Items, page.HasMore = rows[:q.Limit], true
	}
	f.Limit, f.CursorID = 0, 0
	total, err := s.repo.CountLibraryItems(ctx, f)
	if err != nil {
		s.log.Warn("资产计数失败，已降级", "error", err)
		total = -1
	}
	page.Total = total
	return page, nil
}

func (s *Service) LibraryItem(ctx context.Context, workspaceID, id int64) (repo.LibraryItem, error) {
	it, err := s.repo.GetLibraryItem(ctx, workspaceID, id)
	if err != nil {
		return repo.LibraryItem{}, libraryErr(err)
	}
	return it, nil
}

// SaveLibraryItemRequest 「保存到我的资产」。
type SaveLibraryItemRequest struct {
	GenerationID int64
	OutputIdx    int
	Title        string
	Text         string
	FolderID     int64
	Tags         []string
}

// SaveLibraryItem 保存（幂等：同一项产物已经存着就原样返回）。返回资产与是否新建。
func (s *Service) SaveLibraryItem(ctx context.Context, workspaceID, userID int64, in SaveLibraryItemRequest) (repo.LibraryItem, bool, error) {
	if in.GenerationID <= 0 || in.OutputIdx < 0 {
		return repo.LibraryItem{}, false, httpx.Errf(errcode.InvalidArgument, "缺少要保存的生成结果")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "未命名资产"
	}
	if utf8.RuneCountInString(title) > libraryTitleMax {
		title = string([]rune(title)[:libraryTitleMax])
	}
	tags, err := s.normalizeTags(ctx, workspaceID, in.Tags)
	if err != nil {
		return repo.LibraryItem{}, false, err
	}
	id, created, err := s.repo.SaveLibraryItem(ctx, repo.SaveLibraryItemInput{
		WorkspaceID: workspaceID, UserID: userID, GenerationID: in.GenerationID, OutputIdx: in.OutputIdx,
		Title: title, Text: in.Text, FolderID: in.FolderID, Tags: tags,
	})
	if err != nil {
		return repo.LibraryItem{}, false, libraryErr(err)
	}
	it, err := s.LibraryItem(ctx, workspaceID, id)
	return it, created, err
}

// UpdateLibraryItemRequest 改名 / 挪文件夹 / 改标签；为 nil 的不改。FolderID 指向 0 = 挪到顶层。
type UpdateLibraryItemRequest struct {
	Title    *string
	FolderID *int64
	Tags     *[]string
}

func (s *Service) UpdateLibraryItem(ctx context.Context, workspaceID, id int64, in UpdateLibraryItemRequest) (repo.LibraryItem, error) {
	upd := repo.UpdateLibraryItemInput{FolderID: in.FolderID}
	if in.Title != nil {
		t, err := cleanName("名称", *in.Title, libraryTitleMax)
		if err != nil {
			return repo.LibraryItem{}, err
		}
		upd.Title = &t
	}
	if in.Tags != nil {
		tags, err := s.normalizeTags(ctx, workspaceID, *in.Tags)
		if err != nil {
			return repo.LibraryItem{}, err
		}
		if tags == nil {
			tags = []string{}
		}
		upd.Tags, upd.SetTags = tags, true
	}
	if err := s.repo.UpdateLibraryItem(ctx, workspaceID, id, upd); err != nil {
		return repo.LibraryItem{}, libraryErr(err)
	}
	return s.LibraryItem(ctx, workspaceID, id)
}

// BatchLibraryItems 「管理」里的批量操作：move（挪到文件夹，0 = 顶层）/ remove（移出资产）。返回实际处理的条数。
func (s *Service) BatchLibraryItems(ctx context.Context, workspaceID int64, action string, ids []int64, folderID int64) (int, error) {
	if len(ids) == 0 {
		return 0, httpx.Errf(errcode.InvalidArgument, "没有选中任何资产")
	}
	if len(ids) > libraryBatchMax {
		return 0, httpx.Errf(errcode.InvalidArgument, fmt.Sprintf("一次最多处理 %d 项", libraryBatchMax))
	}
	var n int
	var err error
	switch action {
	case "move":
		n, err = s.repo.MoveLibraryItems(ctx, workspaceID, ids, folderID)
	case "remove":
		n, err = s.repo.RemoveLibraryItems(ctx, workspaceID, ids)
	default:
		return 0, httpx.Errf(errcode.InvalidArgument, "批量操作只支持 move / remove")
	}
	if err != nil {
		return 0, libraryErr(err)
	}
	return n, nil
}

func (s *Service) LibraryFolders(ctx context.Context, workspaceID int64) ([]repo.LibraryFolder, error) {
	f, err := s.repo.ListLibraryFolders(ctx, workspaceID)
	if err != nil {
		return nil, libraryErr(err)
	}
	return f, nil
}

func (s *Service) CreateLibraryFolder(ctx context.Context, workspaceID, userID int64, name string) (repo.LibraryFolder, error) {
	n, err := cleanName("文件夹名称", name, libraryFolderMax)
	if err != nil {
		return repo.LibraryFolder{}, err
	}
	f, err := s.repo.CreateLibraryFolder(ctx, workspaceID, userID, n)
	if err != nil {
		return repo.LibraryFolder{}, libraryErr(err)
	}
	return f, nil
}

func (s *Service) RenameLibraryFolder(ctx context.Context, workspaceID, id int64, name string) error {
	n, err := cleanName("文件夹名称", name, libraryFolderMax)
	if err != nil {
		return err
	}
	if err := s.repo.RenameLibraryFolder(ctx, workspaceID, id, n); err != nil {
		return libraryErr(err)
	}
	return nil
}

func (s *Service) DeleteLibraryFolder(ctx context.Context, workspaceID, id int64) error {
	if err := s.repo.DeleteLibraryFolder(ctx, workspaceID, id); err != nil {
		return libraryErr(err)
	}
	return nil
}

// RateGeneration 给生成结果打分（1~5，0 = 清除），整个工作区共用。
func (s *Service) RateGeneration(ctx context.Context, workspaceID, generationID, userID int64, rating int) error {
	if rating < 0 || rating > 5 {
		return httpx.Errf(errcode.InvalidArgument, "评级只能是 0~5（0 = 清除）")
	}
	if generationID <= 0 {
		return httpx.Errf(errcode.InvalidArgument, "缺少生成结果")
	}
	if err := s.repo.SetGenerationRating(ctx, workspaceID, generationID, userID, rating); err != nil {
		if errors.Is(err, repo.ErrNotSavable) {
			return httpx.Errf(errcode.InvalidArgument, "只能给本工作区里已经生成成功的结果打分")
		}
		return libraryErr(err)
	}
	return nil
}

// GenerationMarks 右键菜单要的：每次生成的评分与已保存的产物。最多 100 个。
func (s *Service) GenerationMarks(ctx context.Context, workspaceID int64, ids []int64) (map[int64]repo.GenerationMarks, error) {
	if len(ids) == 0 {
		return map[int64]repo.GenerationMarks{}, nil
	}
	if len(ids) > 100 {
		return nil, httpx.Errf(errcode.InvalidArgument, "一次最多查 100 个生成结果")
	}
	m, err := s.repo.GenerationMarks(ctx, workspaceID, ids)
	if err != nil {
		return nil, libraryErr(err)
	}
	return m, nil
}
