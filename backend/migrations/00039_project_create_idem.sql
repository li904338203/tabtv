-- 项目：创建幂等键（AGT-42，创造页「直接生成」，AG-02）
--
-- 起因：创造页的「直接生成」一次请求要做三件事 —— 建项目、建一个节点、受理生成。
-- 受理（generation.Accept）要调扣费与 outbox，必须在建项目的事务**之外**（Provider/MQ 不入事务），
-- 所以两者没法原子回滚：受理失败时项目保留、节点标失败（开发 01 §10）。
--
-- 生成自己有幂等键（uk_generation_idem），但**防不住项目重复**：第一次受理失败时没有 generation 行，
-- 用户点重试（或浏览器网络重试）就会再建一个项目 —— 表现为项目列表里多出一串同名的空项目，
-- 而每一个看起来都是"我刚建的"。所以项目自己要有一列幂等键，同键重复请求返回已建的那个。
--
-- 取值：只有创造页的组合请求写它；普通「新建项目」不收幂等键，留 null。
-- 部分唯一索引（`where ... is not null`）：普通建项目全是 null，不该互相撞。
-- 作用域是工作区（同一个键在两个工作区里是两回事 —— 键由浏览器生成，跨工作区撞上纯属巧合）。
-- +goose Up
alter table project add column create_idempotency_key varchar(64);
create unique index uk_project_create_idem on project (workspace_id, create_idempotency_key)
    where create_idempotency_key is not null;

-- +goose Down
drop index if exists uk_project_create_idem;
alter table project drop column if exists create_idempotency_key;
