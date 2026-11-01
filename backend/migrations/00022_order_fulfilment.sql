-- 订单履约快照与会员资格的 superseded 状态（GO-04、P11-01）
--
-- 起因一：**履约要用的数据必须在下单时就定死**。原表只有金额，
-- 履约时得回头查目录算一遍"发多少分、会员到期到哪天" —— 而用户下单之后
-- 运营完全可能改价或改赠分，于是他付的是旧价、拿到的是新权益（或更少的）。
-- 这与生成任务冻结 `params_snapshot` 是同一条规则：一旦受理，
-- 后台此后的配置改动不得改变这一单的解释。
--
-- 起因二：`membership.status` 原先只有 active/expired/refunded 三个值，
-- 而**升级**会产生第四种情况 —— 旧那条既没过期也没退款，是被新的顶替了。
-- 硬塞进 expired 的话，"升级后原来那条的到期时间还在未来"会让
-- 「当前有效会员」的查询同时命中两行，取到哪一行取决于排序 —— 而排序
-- 一改（比如加个二级排序）权益就变了，且没有任何报错。
-- +goose Up

-- 履约快照：这一单付完之后到底发生什么，下单时就写死。
alter table purchase_order
    add column fulfil_credits   integer not null default 0,
    add column fulfil_level_id  bigint,
    add column fulfil_start_at  timestamptz,
    add column fulfil_expire_at timestamptz;

comment on column purchase_order.fulfil_credits is
    '履约时发放的积分。下单即冻结，不在履约时回查目录';
comment on column purchase_order.fulfil_expire_at is
    '会员订单履约后的到期时间。续费是顺延、升级是保持原到期日，两者都在下单时算好';

-- superseded：被后续的升级顶替。
--
-- ⚠️ 与 expired 必须分开：expired 是"时间到了"（由过期作业刷），
-- superseded 是"被顶替了"（履约事务里同步写）。合并的话，
-- 一条到期时间仍在未来的 expired 行会让权益查询无从判断。
alter table membership drop constraint ck_membership_status;
alter table membership add constraint ck_membership_status
    check (status in ('active','expired','refunded','superseded'));

-- +goose Down
alter table membership drop constraint ck_membership_status;
alter table membership add constraint ck_membership_status
    check (status in ('active','expired','refunded'));
alter table purchase_order
    drop column if exists fulfil_expire_at,
    drop column if exists fulfil_start_at,
    drop column if exists fulfil_level_id,
    drop column if exists fulfil_credits;
