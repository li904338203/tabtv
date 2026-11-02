-- 内置内容的「内置版本」规则（tabtv TT-T-24，dev/02 §3）。
-- 起因：来源系统的 seed 对图片预设是 `on conflict do update`（每次启动冲掉用户改过的预设），
--   对技能是 `on conflict do nothing`（内置技能升级后永远下发不到已有环境）—— 两个方向各错一边。
--   桌面版每次启动都跑 seed，所以要能区分「用户改过的」与「还是内置原样的」：
--   builtin_hash = 当前行对应的内置内容哈希（用户新建的行为空）；is_user_modified = 用户通过设置页改过。
--   seed 只更新没被改过的行；改过的行由设置页提供「恢复内置版本」。
-- +goose Up
alter table image_preset add column builtin_hash varchar(64);
alter table image_preset add column is_user_modified boolean not null default false;
alter table agent_skill add column builtin_hash varchar(64);
alter table agent_skill add column is_user_modified boolean not null default false;

-- +goose Down
alter table agent_skill drop column if exists is_user_modified;
alter table agent_skill drop column if exists builtin_hash;
alter table image_preset drop column if exists is_user_modified;
alter table image_preset drop column if exists builtin_hash;
