-- 起因：P05-03 关闭时挂账的「供应商级并发」，在 P06-03 把真实 Provider 接进执行器后补齐。
--
-- 并发上限此前只有**付费主体级**（同一个账户同时跑几个任务）。它护的是用户之间的公平，
-- 护不住上游：一个工作区把十个任务全压到同一家 Provider 上，就会撞上对方的账号级 QPS/并发限制，
-- 换回来一批 429。那批任务按「可重试」放回队列，下一轮又一起冲上去 —— 退避只在单个任务上生效，
-- 一整批同时退避等于同时重试，问题原样复现。
--
-- 因此并发要在**供应商这一维**也能收：`max_concurrent` 为 0 表示不限（默认，行为与改前一致），
-- 大于 0 时 Worker 领取任务前先数一遍该 Provider 下 running 的任务数。
--
-- ⚠️ 达到上限**不是失败**，任务留在 queued 等下一轮（与付费主体级同一套语义）——
-- 判失败的话，用户批量提交十个会有七个直接失败。

-- +goose Up
alter table provider_config
    add column max_concurrent integer not null default 0;

alter table provider_config
    add constraint ck_provider_max_concurrent check (max_concurrent >= 0);

comment on column provider_config.max_concurrent is
    '该 Provider 允许同时运行的任务数，0 表示不限；达到上限的任务留在 queued 等待，不判失败';

-- 领取任务时要按 provider 数 running：generation 只有 model_id，
-- 因此这条索引落在 model 上，让 `join model on model.id = generation.model_id` 之后
-- 按 provider_id 聚合不必全表扫。
create index if not exists idx_model_provider_status on model (provider_id, status);

-- +goose Down
drop index if exists idx_model_provider_status;
alter table provider_config drop constraint if exists ck_provider_max_concurrent;
alter table provider_config drop column if exists max_concurrent;
