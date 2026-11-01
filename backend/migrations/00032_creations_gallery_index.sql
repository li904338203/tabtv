-- 「我的作品」（成功产出画廊）的索引（P15-02）
--
-- 起因：移植过来的「我的作品」页一直是空的。老系统的「作品」= 用户**全部成功的生成**
-- （按图片/视频/音频筛，带项目名与模型），而新系统的 `/works` 是 P13-02 的
-- 「**提交去审核**的作品」—— 两个「作品」不是一回事，用户一件都没提交过，
-- 于是这一页永远空着。新增 `GET /creations`（content 域）承接老页面的语义。
--
-- 这条查询是「某工作区里成功的生成，按 id 倒序翻页」：
--   where workspace_id = ? and status = 'success' [and result_type = ?] and id < ?
--   order by id desc limit N
-- 已有的 `idx_generation_ws (workspace_id, status, created_at desc)` 排序键是 created_at，
-- **按 id 翻页用不上它的顺序** —— PG 只能把这个工作区的全部成功生成取出来再排
-- （CD-67 压测抓到过同一类：索引与 ORDER BY 错配，低数据量下看不出，代价随表增长）。
-- 部分索引只收成功的那些，正好是画廊要的。
--
-- ⚠️ 用 id 而不是 (created_at, id) 两段游标：雪花 ID 在受理时生成、与 created_at 同序，
--    而单段游标不必把时间序列化进 URL —— 按秒截断的时间戳做游标，
--    会在「同一秒里写入好几条」的分页边界上漏行。
-- +goose Up
create index idx_generation_ws_success_id
    on generation (workspace_id, id desc)
    where status = 'success';

-- +goose Down
drop index if exists idx_generation_ws_success_id;
