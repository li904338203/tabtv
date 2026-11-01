-- 支付渠道配置与回调追踪（GO-06、P11-02）
--
-- 起因一：渠道凭据必须像 Provider 的 API key 一样**加密存库**，
-- 而不是塞进环境变量。环境变量改一次要重启，而"换一张证书"是运营动作；
-- 更要紧的是：多渠道、多商户号时环境变量根本表达不了。
--
-- 起因二：**回调是外网可达的公开端点**，所以每一条都要落库留痕 ——
-- 验签失败的也要留（那正是排查攻击与配置错误的唯一线索）。
-- `payment_callback` 早就在 schema 里，这次补上"处理到哪一步"的字段。
--
-- 起因三：`purchase_order` 需要记**查单**的结果。"用户说付了但回调没来"
-- 是支付里最常见的工单，没有查单入口就只能让用户等。
-- +goose Up

create table payment_channel (
    id                  bigint      primary key,
    -- 渠道码：wechat / alipay / dev。与 purchase_order.channel 同一套取值。
    code                varchar(32) not null,
    name                varchar(64) not null,
    -- 商户号 / 应用 ID。这两个**不是密钥**，可以明文（对账时要能看见）。
    merchant_id         varchar(128) not null default '',
    app_id              varchar(128) not null default '',
    -- ⚠️ 下面三项都是**密钥**：加密存储，永不出现在配置响应与请求日志里。
    --   private_key_cipher —— 商户私钥（我们用它签名）
    --   platform_key_cipher —— 渠道公钥/平台证书（我们用它验签）
    --   api_v3_key_cipher  —— 微信 APIv3 对称密钥（解回调用）
    private_key_cipher  text,
    platform_key_cipher text,
    api_v3_key_cipher   text,
    key_version         integer     not null default 1,
    -- 回调地址与下单地址。放库里而不是写死：沙箱与生产是两个域名，
    -- 写死的话切环境要发版。
    base_url            varchar(512) not null default '',
    notify_url          varchar(512) not null default '',
    -- ⚠️ `is_sandbox` 要显式记：沙箱与生产的凭据长得一模一样，
    -- 配错了的唯一表现是"付款成功但钱没到账"（或反过来，真的扣了用户的钱）。
    -- 这一位让管理端能把两者明确区分开，也让审计说得清。
    is_sandbox          boolean     not null default true,
    status              varchar(16) not null default 'disabled',
    created_at          timestamptz not null default now(),
    updated_at          timestamptz not null default now(),
    constraint ck_pay_channel_status check (status in ('enabled','disabled'))
);
create unique index uk_payment_channel_code on payment_channel (code);

-- 回调处理状态。
--
-- ⚠️ 原表只有 `result varchar(32)`，说不清"到哪一步失败的"。
-- 支付排障要回答的恰恰是这个：验签就没过（配置/攻击）、
-- 还是验签过了但订单找不到（下错单/串环境）、
-- 还是订单找到了但金额不对（被改过）、还是履约本身炸了（我们的 bug）。
alter table payment_callback
    add column stage        varchar(32) not null default 'received',
    add column channel_code varchar(32) not null default '',
    add column out_trade_no varchar(128),
    add column amount_cents bigint,
    add column error_message varchar(500);

alter table payment_callback
    add constraint ck_callback_stage check (stage in
        ('received','signature_failed','order_not_found','amount_mismatch',
         'not_succeeded','duplicated','fulfilled','fulfil_failed'));

create index idx_callback_stage on payment_callback (stage, created_at desc);

-- 主动查单的结果。
--
-- ⚠️ 「用户说付了但回调没来」是支付里最常见的工单。没有查单入口的话
-- 只能让用户等下一次重试，而渠道的重试是有次数上限的 ——
-- 上限用完之后那笔钱就只能人工对账。
alter table purchase_order
    add column last_synced_at timestamptz,
    add column sync_note      varchar(255);

-- +goose Down
alter table purchase_order
    drop column if exists sync_note,
    drop column if exists last_synced_at;
drop index if exists idx_callback_stage;
alter table payment_callback drop constraint if exists ck_callback_stage;
alter table payment_callback
    drop column if exists error_message,
    drop column if exists amount_cents,
    drop column if exists out_trade_no,
    drop column if exists channel_code,
    drop column if exists stage;
drop table if exists payment_channel;
