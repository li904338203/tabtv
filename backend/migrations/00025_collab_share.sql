-- 协作共享与邀请生命周期（P12-01；NEW-17/NEW-18、GAP-11、AC-02）
--
-- 起因：`resource_share` 在 00002 就随工作区一起建好了，但到 P11 结束为止
-- **没有任何代码写过它，也没有任何查询读过它** —— 这是「选了等于没选」的
-- 另一个版本（同 GAP-01 里的 presetCode）。本次把它接进真实的读取路径，
-- 顺带补三处缺口：
--   1. 撤销只留下 revoked_at，事后只知道"这条授权没了"，不知道是谁的决定；
--   2. 同一工作区对同一受邀人可以同时存在多条 pending 邀请 ——
--      运营连点三次就发出三个都能用的邀请码，撤销其中一个毫无意义；
--   3. 「我收到的邀请」要按 target_value 反查，没有索引就是全表扫。
-- +goose Up

-- 撤销者。共享是一次显式的对外授权决定，撤销同样是 —— 两头都要留痕。
alter table resource_share add column revoked_by bigint;

-- 授予方视角：这个资源共享给了谁。
create index idx_share_resource on resource_share (resource_type, resource_id)
    where revoked_at is null;
-- 工作区归档时要一次撤销它授出的全部共享。
-- ⚠️ 这条撤销不能省：受让方的读取路径按 resource_share 授权，
-- 归档只改 workspace.status 的话，被归档工作区的资源仍然共享得好好的。
create index idx_share_workspace on resource_share (workspace_id)
    where revoked_at is null;

-- 同一工作区对同一受邀人只能有一条待处理邀请。
-- ⚠️ 唯一索引只是兜底：应用层在建新邀请前会先把同目标的 pending 显式撤销，
-- 并在响应里告诉调用方「上一条已失效」。少了这一步，改角色重发邀请会直接
-- 撞索引报 409「名称已存在」—— 一句和当前操作完全对不上号的错误。
create unique index uk_invite_pending on workspace_invitation
    (workspace_id, target_kind, target_value) where status = 'pending';

-- 「我收到的邀请」：按 target 反查。
create index idx_invite_target on workspace_invitation (target_kind, target_value)
    where status = 'pending';

-- 跨工作区导入资产时复制媒体。
-- ⚠️ 复制的是**字节**不是引用：共用同一个 object_key 的话，源工作区清理素材
-- 会让受让方那边的图凭空裂掉，而两边都不会报错 —— 与 00019 里
-- 「派生块是独立的字节，存在不依赖源图」是同一条规则。
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in ('grid_tile', 'share_import'));

-- +goose Down
alter table media_object drop constraint ck_media_derive_kind;
alter table media_object add constraint ck_media_derive_kind
    check (derive_kind is null or derive_kind in ('grid_tile'));
drop index if exists idx_invite_target;
drop index if exists uk_invite_pending;
drop index if exists idx_share_workspace;
drop index if exists idx_share_resource;
alter table resource_share drop column revoked_by;
