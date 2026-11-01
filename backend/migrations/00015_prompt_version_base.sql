-- 起因：P03-05 端到端验证时发现「第一次发布产出的是 v2，而 v1 永远回滚不到」。
--
-- prompt_template.version 的列默认值是 1，而新建草稿行的 content 是空串 ——
-- 于是每个模板都凭空多出一个「版本 1」：它没有快照（快照只在发布时写），
-- 内容是空的，管理端却会把它列进版本历史，点回滚必得 40401。
--
-- 改成 0 起算后，version 的语义变成「已发布过几次」，
-- 由此得到一条结构性不变量：**任何 >= 1 的版本号都有快照、都能回滚**。
-- 这比在回滚时特判 v1 更可靠 —— 特判只能挡住这一个入口，
-- 而版本历史、审计、对外 API 都会各自暴露那个假版本。
--
-- 同时把 status 的默认值从 published 改为 draft：
-- 一条没有人显式决定过状态的行不该直接对线上生效。

-- +goose Up
alter table prompt_template alter column version set default 0;
alter table prompt_template alter column status set default 'draft';

-- 未发布过的行（还没有任何快照）回归 0；已发布的行保持现状，
-- 它们的每个版本号都已有快照，改动号码反而会让审计对不上。
update prompt_template t
set version = 0
where not exists (select 1 from prompt_template_version v where v.template_id = t.id);

-- +goose Down
alter table prompt_template alter column version set default 1;
alter table prompt_template alter column status set default 'published';
