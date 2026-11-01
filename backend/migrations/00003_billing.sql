-- 会员、订单与积分账本（数据模型 §8）
-- 起因：账本是全系统最容易出错的部分。三条硬规则落到约束上——
--   消费顺序（先扣将过期批次）、退款一次性、过期批次不复活。
-- +goose Up

create table member_level (
    id            bigint primary key,
    code          varchar(32) not null,
    name          varchar(64) not null,
    sort          integer     not null default 0,
    is_enabled    boolean     not null default true,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now()
);
create unique index uk_member_level_code on member_level (code);

create table member_plan (
    id              bigint primary key,
    level_id        bigint      not null,
    period          varchar(16) not null,
    -- 金额一律整数分。禁浮点（GO-19）。
    price_cents     bigint      not null,
    gift_credits    integer     not null default 0,
    sort            integer     not null default 0,
    is_enabled      boolean     not null default true,
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now(),
    constraint ck_plan_period check (period in ('month','quarter','year')),
    constraint ck_plan_price  check (price_cents >= 0)
);
create unique index uk_member_plan on member_plan (level_id, period);

create table credit_package (
    id          bigint primary key,
    code        varchar(32) not null,
    name        varchar(64) not null,
    credits     integer     not null,
    price_cents bigint      not null,
    sort        integer     not null default 0,
    is_enabled  boolean     not null default true,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now(),
    constraint ck_package_credits check (credits > 0),
    constraint ck_package_price   check (price_cents >= 0)
);
create unique index uk_credit_package_code on credit_package (code);

-- 会员挂 payer_account 而非 user：团队工作区的成员应继承工作区的等级权益。
create table membership (
    id               bigint primary key,
    payer_account_id bigint      not null,
    level_id         bigint      not null,
    start_at         timestamptz not null,
    expire_at        timestamptz not null,
    source_order_id  bigint,
    status           varchar(16) not null default 'active',
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    constraint ck_membership_status check (status in ('active','expired','refunded'))
);
create index idx_membership_active on membership (payer_account_id, status, expire_at desc);

-- order 是 PG 保留字，改名 purchase_order（免去每条手写 SQL 加引号）。
create table purchase_order (
    id               bigint primary key,
    payer_account_id bigint      not null,
    placed_by_user_id bigint     not null,
    type             varchar(32) not null,
    ref_id           bigint,
    amount_cents     bigint      not null,
    currency         varchar(8)  not null default 'CNY',
    pay_status       varchar(16) not null default 'pending',
    channel          varchar(32),
    channel_txn_id   varchar(128),
    paid_at          timestamptz,
    expire_at        timestamptz,
    reconcile_status varchar(16) not null default 'none',
    idempotency_key  varchar(128) not null,
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now(),
    constraint ck_order_type   check (type in ('membership','credit_package')),
    constraint ck_order_pay    check (pay_status in ('pending','paid','cancelled','refunded','expired')),
    constraint ck_order_recon  check (reconcile_status in ('none','pending','matched','diff')),
    constraint ck_order_amount check (amount_cents >= 0)
);
create unique index uk_order_idem on purchase_order (payer_account_id, idempotency_key);
-- 渠道交易号唯一：防止同一笔支付被重复履约（AC-15）。
create unique index uk_order_channel_txn on purchase_order (channel, channel_txn_id)
    where channel_txn_id is not null;
create index idx_order_account on purchase_order (payer_account_id, created_at desc);
-- 超时取消扫描。
create index idx_order_expiry on purchase_order (pay_status, expire_at) where pay_status = 'pending';

-- 支付回调流水。重复回调靠 uk_callback_event 拦截，不靠"先查订单状态再处理"。
create table payment_callback (
    id                  bigint primary key,
    channel             varchar(32)  not null,
    channel_event_id    varchar(128) not null,
    order_id            bigint,
    raw_payload         jsonb        not null,
    is_signature_valid  boolean      not null,
    processed_at        timestamptz,
    result              varchar(32),
    created_at          timestamptz  not null default now()
);
create unique index uk_callback_event on payment_callback (channel, channel_event_id);
create index idx_callback_order on payment_callback (order_id);

create table refund (
    id                 bigint primary key,
    order_id           bigint      not null,
    amount_cents       bigint      not null,
    reason             varchar(255) not null,
    operator_admin_id  bigint,
    channel_refund_id  varchar(128),
    status             varchar(16) not null default 'pending',
    created_at         timestamptz not null default now(),
    updated_at         timestamptz not null default now(),
    constraint ck_refund_status check (status in ('pending','success','failed')),
    constraint ck_refund_amount check (amount_cents > 0)
);
create unique index uk_refund_channel on refund (channel_refund_id) where channel_refund_id is not null;
create index idx_refund_order on refund (order_id);

-- 积分批次。余额 = sum(active grant.remaining)，**不设 user.balance 单字段**。
create table credit_grant (
    id                       bigint primary key,
    payer_account_id         bigint      not null,
    source                   varchar(32) not null,
    source_ref_id            varchar(128),
    amount                   integer     not null,
    remaining                integer     not null,
    expire_at                timestamptz,
    status                   varchar(16) not null default 'active',
    -- 过期批次不复活，改发补偿批次并指回原批次（GO-25）。
    compensation_of_grant_id bigint,
    created_at               timestamptz not null default now(),
    updated_at               timestamptz not null default now(),
    constraint ck_grant_source    check (source in
        ('membership','purchase','admin_adjust','register','expiry_compensation','promotion')),
    constraint ck_grant_status    check (status in ('active','exhausted','expired','revoked')),
    constraint ck_grant_amount    check (amount > 0),
    constraint ck_grant_remaining check (remaining >= 0 and remaining <= amount)
);
-- 同一来源+来源引用只能发放一次（GO-21）。
create unique index uk_grant_source on credit_grant (source, source_ref_id) where source_ref_id is not null;
-- ⚠️ 消费顺序的支撑索引：先扣将过期的，永久批次最后扣。
create index idx_grant_consume on credit_grant (payer_account_id, status, expire_at);
-- 过期扫描。
create index idx_grant_expiry on credit_grant (status, expire_at) where expire_at is not null;

create table credit_transaction (
    id                bigint primary key,
    payer_account_id  bigint      not null,
    type              varchar(16) not null,
    change            integer     not null,
    balance_after     integer     not null,
    ref_type          varchar(32) not null,
    ref_id            varchar(128) not null,
    operator_user_id  bigint,
    operator_admin_id bigint,
    reason            varchar(255),
    created_at        timestamptz not null default now(),
    constraint ck_ct_type check (type in ('grant','consume','refund','expire','adjust'))
);
-- ⚠️ 退款一次性的最终兜底（GO-21）。ref_type 与 ref_id 分两列：
-- 旧系统用 "script:{id}:storyboard" 这类拼接串，无法索引也无法按来源统计。
create unique index uk_ct_ref_type on credit_transaction (ref_type, ref_id, type);
create index idx_ct_account on credit_transaction (payer_account_id, created_at desc);

-- 逐批次消费明细。退款按原批次回补，读的就是这张表。
create table credit_transaction_detail (
    id             bigint primary key,
    transaction_id bigint  not null,
    grant_id       bigint  not null,
    amount         integer not null,
    constraint ck_ctd_amount check (amount > 0)
);
create index idx_ctd_tx on credit_transaction_detail (transaction_id);
create index idx_ctd_grant on credit_transaction_detail (grant_id);

-- +goose Down
drop table if exists credit_transaction_detail;
drop table if exists credit_transaction;
drop table if exists credit_grant;
drop table if exists refund;
drop table if exists payment_callback;
drop table if exists purchase_order;
drop table if exists membership;
drop table if exists credit_package;
drop table if exists member_plan;
drop table if exists member_level;
