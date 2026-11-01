-- 智能剪辑（docs/agent/smartcut SCT-39）：音频轨上的片段可以「跟随」画面轨上的一段（SC-F-56）。
-- 起因：
--   按画面生成的音效（可灵视频配音效）放进音效轨后要跟着它的那一段走 —— 片段被移动、裁切、变速时音效的成片时间
--   随之重算，落到被裁掉区间里的隐藏（不删除）。设计里只给字幕留了锚点（00067 的 timeline_subtitle.anchor），
--   片段表没有：音效落下去之后就停在发起时的位置，用户把那一段往后挪，音效还在原处响，而画面已经换了一段。
-- 形状与字幕的锚点相同：{clipId, srcStartMs, srcEndMs}（生成时那一段素材的区间；音频从 0 起与它一一对应）。
-- ⚠️ 只加一列，可为空（不跟随）。读的时候按锚点现算落点（timeline/domain.FollowClip），不另存派生值。
-- +goose Up
alter table timeline_clip add column anchor jsonb;

-- +goose Down
alter table timeline_clip drop column if exists anchor;
