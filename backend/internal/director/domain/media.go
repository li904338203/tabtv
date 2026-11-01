package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// 场景正文里引用媒体的角色。
const (
	MediaRolePanorama = "panorama"
	MediaRoleSplat    = "splat"
)

// MediaRef 是场景正文对一个媒体的引用。
type MediaRef struct {
	ID   int64
	Role string
}

// MediaRefs 提取场景正文引用的媒体：全景图与泼溅文件（DRD-16）。
//
// 用在两处：保存时校验这些媒体属于本工作区（否则拿别人的媒体 ID 写进自己的场景，
// 就能挡住别人删除）；以及提取成 `director_scene.media_ids` 列，给删除保护按媒体反查。
//
// ⚠️ 背景当前是纯色、但正文里仍留着全景配置时，**照样算引用**：用户切回全景时要用它，
// 不保护的话那张图可能已经被回收，切回去是一张裂图。
// 截图、预演视频不在这里：它们记在 `director_media` 表里（DRD-12）。
func MediaRefs(raw json.RawMessage) ([]MediaRef, error) {
	s, err := ParseScene(raw)
	if err != nil {
		return nil, err
	}
	t := s.Tree()
	var refs []MediaRef
	if bg, ok := t["background"].(map[string]any); ok {
		if p, ok := bg["panorama"].(map[string]any); ok {
			if ref, ok, err := mediaRefOf(p["mediaId"], MediaRolePanorama); err != nil {
				return nil, err
			} else if ok {
				refs = append(refs, ref)
			}
		}
	}
	if sp, ok := t["splat"].(map[string]any); ok {
		if ref, ok, err := mediaRefOf(sp["mediaId"], MediaRoleSplat); err != nil {
			return nil, err
		} else if ok {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func mediaRefOf(v any, role string) (MediaRef, bool, error) {
	if v == nil {
		return MediaRef{}, false, nil
	}
	s, ok := v.(string)
	if !ok {
		return MediaRef{}, false, fmt.Errorf("%s 的媒体 ID 必须是字符串", role)
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return MediaRef{}, false, fmt.Errorf("%s 的媒体 ID 不合法：%q", role, s)
	}
	return MediaRef{ID: id, Role: role}, true, nil
}

// MediaIDs 把引用转成 `media_ids` 列的值：去重、按数值升序、转成字符串
// （列是 text[]，GIN 的 @> 才能命中，同 MKD-31）。没有引用时返回空切片而不是 nil，
// 入库后是 '{}' 而不是 NULL。
func MediaIDs(refs []MediaRef) []string {
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(refs))
	for _, r := range refs {
		if r.ID <= 0 || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		ids = append(ids, r.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return out
}
