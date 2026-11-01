-- 订单退款、权益回收与对账（GO-04、GAP-20、P11-03）
--
-- 起因一：`refund` 表早就在 schema 里，但**没有任何字段记"回收了什么"**。
-- 退款是三个环节（调支付方、回收权益、订单状态），少了中间那段的话，
-- 事后无从回答「这一单的积分到底收回来没有」——而那正是对账时最要紧的问题。
--
-- 起因二：⚠️ **升级/续费会把上一条会员标成 `superseded`**。退掉这一单时，
-- 如果不把那条恢复回来，用户退的是升级差价、结果连**原来的基础会员也没了**。
-- 所以要记住"这次退款顶替回了哪一条"，否则恢复动作不可重放也不可审计。
--
-- 起因三：对账要能落结论。`purchase_order.reconcile_status` 早就有，
-- 但没有"差异是什么"的字段 —— 只标一个 `diff` 等于只说"有问题"，
-- 而对账的全部价值在于**可行动**。
-- +goose Up

alter table refund
    -- 这次退款回收了什么。⚠️ 三项都要记：事后要能回答
    -- 「钱退了没有」「积分收回来没有」「会员回收了没有」——
    -- 三个问题的处置完全不同，合成一个"已退款"就分不出来了。
    add column payer_account_id bigint,
    add column revoked_credits  integer     not null default 0,
    add column revoked_grant_id bigint,
    add column revoked_membership_id bigint,
    -- ⚠️ 被顶替的那条会员：退款时要把它恢复成 active。
    -- 不记的话恢复动作只能靠"找最近一条 superseded"去猜，
    -- 而那在连续两次升级之后会猜错。
    add column restored_membership_id bigint,
    add column requested_by_user_id bigint,
    -- 渠道侧的退款结果原文（脱敏后），排障用。
    add column channel_result varchar(500);

-- ⚠️ 一张订单**只能有一笔成功的退款**。靠部分唯一索引兜底，
-- 不靠"先查再插"—— 并发的两次申请各自查一遍都会说"还没退过"。
create unique index uk_refund_order_active on refund (order_id)
    where status in ('pending', 'success');

create index idx_refund_payer on refund (payer_account_id, created_at desc);

-- 对账差异的具体内容。
--
-- ⚠️ 只标一个 `diff` 等于只说"有问题"：对账的全部价值在于**可行动**，
-- 而可行动的前提是说清"本地 2900 分、渠道 100 分"这类具体差额。
alter table purchase_order
    add column reconcile_detail varchar(500),
    add column reconciled_at    timestamptz;

-- 积分批次被哪次退款收回。
--
-- ⚠️ `credit_grant.status` 里早就有 `revoked` 这个值，但没有"被谁收回"的线索。
-- 用户问「我的 1000 积分怎么没了」时，没有这一列就只能翻流水猜。
alter table credit_grant
    add column revoked_by_refund_id bigint;

-- +goose Down
alter table credit_grant drop column if exists revoked_by_refund_id;
alter table purchase_order
    drop column if exists reconciled_at,
    drop column if exists reconcile_detail;
drop index if exists idx_refund_payer;
drop index if exists uk_refund_order_active;
alter table refund
    drop column if exists channel_result,
    drop column if exists requested_by_user_id,
    drop column if exists restored_membership_id,
    drop column if exists revoked_membership_id,
    drop column if exists revoked_grant_id,
    drop column if exists revoked_credits,
    drop column if exists payer_account_id;
