-- 死信落库（P13-03；GAP-07 的另一半）
--
-- 起因：死信消息此前**只存在于 RabbitMQ 的 `.dead` 队列里**，没有任何人
-- 看得到它 —— 而 `AckDead` 的两种情形（消息解不开、提交结果不确定）恰恰
-- 都要求人去对账。队列里躺着的消息对运维等于不存在：管理端没有入口，
-- 要看只能开 broker 的管理界面，而那上面 peek 一条就会把它从队列里取走。
--
-- 所以由 Worker 把死信**排空进这张表**，运维在表上操作（重投 / 丢弃）。
-- 这样做还有两个附带好处：重投走的是正常的 publish 路径（不需要 API 进程
-- 连 AMQP），处置结果有状态与留痕（队列里没有"已处理"这个概念）。
-- +goose Up

create table dead_letter (
    id             bigint       primary key,
    -- 哪条主队列产生的（generation.jobs / render.jobs / export.jobs）。
    queue          varchar(64)  not null,
    -- 原始 routing key。重投时要**原样用它**，不能按 queue 猜一个 ——
    -- 猜错的话消息会被投到另一条队列上，而那边的消费者解不开它、
    -- 于是又进一次死信，看起来像"重投没生效"。
    routing_key    varchar(128) not null default '',
    -- ⚠️ 用 text 不用 jsonb：死信**按定义就可能是解不开的字节**
    -- （`AckDead` 的第一种情形就是"消息解不开"）。用 jsonb 的话 PG 会拒收，
    -- 于是最需要人看的那一条恰恰是唯一存不进来的 —— 而落库失败只会在
    -- Worker 日志里留一行，消息本身从此彻底消失。
    -- 写入前一律过 transport.SafeText（PG 的 text 列拒收 0x00）。
    payload        text         not null,
    -- 下面两列只在**解得开**时填；解不开时为空是正常的，不是数据缺陷。
    aggregate_type varchar(32),
    aggregate_id   bigint,
    reason         varchar(255) not null default '',
    status         varchar(16)  not null default 'pending',
    replay_count   integer      not null default 0,
    handled_by_admin_id bigint,
    handled_at     timestamptz,
    note           varchar(255),
    created_at     timestamptz  not null default now(),
    updated_at     timestamptz  not null default now(),
    constraint ck_dead_letter_status check (status in ('pending','replayed','discarded'))
);

-- ⚠️ 幂等：死信投递同样是 at-least-once，排空器可能看到同一条两次。
-- 去重键是 (queue, payload 的 md5)，且**只在 pending 时唯一** ——
-- 一条被重投之后再次失败是**一次新的故障**，必须能再记一条；
-- 无条件唯一的话第二次死信会被静默吞掉，而那正是"重投没解决问题"的证据。
create unique index uk_dead_letter_open on dead_letter (queue, md5(payload))
    where status = 'pending';
-- 运维台默认只看待处理的。
create index idx_dead_letter_status on dead_letter (status, created_at desc);
-- 从任务反查："这个任务有没有卡在死信里"。
create index idx_dead_letter_agg on dead_letter (aggregate_type, aggregate_id)
    where aggregate_id is not null;

-- +goose Down
drop table if exists dead_letter;
