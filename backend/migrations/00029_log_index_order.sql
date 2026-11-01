-- 请求日志检索的索引与查询排序对齐（P14-02 压测发现）
--
-- 起因：P14-02 的压测在 4.2 万行 `api_request_log` 上跑「只看失败」检索，
-- p95 打到 **1.78 秒**。`explain analyze` 定位出来的原因不是数据量本身，
-- 而是**索引与 ORDER BY 对不上**：
--
--   Index Scan Backward using api_request_log_pkey
--     Filter: (http_status >= 400)
--     Rows Removed by Filter: 33610
--     Buffers: shared hit=25542
--
-- 查询按 `id desc` 排序（游标分页要求排序键唯一，CD-64），而 P13-04 建的
-- 部分索引 `idx_api_log_failed` 建在 `created_at` 上 —— 于是它**排序用不上**，
-- PG 只能倒扫主键、逐行过滤，扫掉 33610 行才凑够 30 条。
--
-- ⚠️ 这个代价**随表增长线性上升**，而这两张表是**唯二会无限增长的**：
-- 今天 4 万行是 1.78s，一个月后就是分钟级 —— 而它在低数据量下完全看不出来
-- （空表上任何索引都够快）。**这正是压测该发现的那一类问题**。
--
-- ⚠️ 修法是让索引的排序键与查询的排序键一致，而**不是**改查询去迁就索引：
-- 按 `created_at` 翻页会在同毫秒写入的几条上漏或重（CD-64 记过一次），
-- 那是用正确性换性能。
-- +goose Up

-- 失败筛选 + id 倒序：两者一起才用得上。
create index idx_api_log_failed_id on api_request_log (id desc) where http_status >= 400;
-- 全量列表也按 id 倒序翻页，同理。
create index idx_api_log_id on api_request_log (id desc);

-- Provider 日志是同一条链路上的同一个问题：`ListLogs` 也按 `id desc` 排序，
-- 而 `idx_provider_log_failed` 建在 `(provider_code, created_at)` 上。
-- ⚠️ 它现在只有 160 行，压测里看不出来 —— 但它与 API 日志同样无限增长，
-- 等到看得出来的时候已经是线上问题了。
create index idx_provider_log_failed_id on provider_request_log (id desc) where error_type <> 'none';
create index idx_provider_log_id on provider_request_log (id desc);

-- ⚠️ `idx_api_log_time` / `idx_api_log_failed` **保留**：清理作业按
-- `created_at` 删（`CleanupAPILogsBefore`），那条路仍然需要时间索引。
-- 删掉的话清理会退化成全表扫，而清理是每小时跑的。

-- +goose Down
drop index if exists idx_provider_log_id;
drop index if exists idx_provider_log_failed_id;
drop index if exists idx_api_log_id;
drop index if exists idx_api_log_failed_id;
