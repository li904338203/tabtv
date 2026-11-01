-- 会话：分清「被续期轮换掉的」与「退出登录的」，并记下轮换成了谁（会话修复）
--
-- 起因：refresh 每续一次就作废旧的（轮换）。但「服务端已经轮换、响应没回到浏览器」时 ——
-- 续期途中刷新或关闭页面、移动网络断开、两个标签页同时续 —— 浏览器手里只剩那张已作废的旧 refresh，
-- 下一次续期必然 40103，用户被踢回登录页，而服务端与前端各自都"没错"。修完续期之后实测踩到：
-- 换一个页面的瞬间上一页正在续期，响应被导航中止，新页面拿着旧 refresh 一续就被踢。
--
-- 修法：被**轮换**掉的 refresh 在很短的宽限期内还能接上这条链当前那一张（不新开会话），
-- 为此要能区分两种撤销 —— 退出、封禁、改密撤销的一秒宽限都不能给：
--   · revoked_reason 增加 `rotated`（此前轮换借用的是 `logout`，与真退出分不开）；
--   · replaced_by：轮换成了哪一张（后继的 jti）。退出登录也沿它找到当前那一张去撤销 ——
--     手里的 access 可能属于一张早已被轮换掉的 refresh，只撤销它自己等于什么都没做。
-- +goose Up
alter table auth_session add column replaced_by varchar(64);

alter table auth_session drop constraint ck_auth_session_reason;
alter table auth_session add constraint ck_auth_session_reason check (revoked_reason is null or revoked_reason in
    ('logout','password_changed','banned','admin_revoked','expired','rotated'));

-- +goose Down
-- ⚠️ 加回旧约束之前先把 rotated 挪回 logout（回退前的代码就是这么记的），否则约束加不上。
update auth_session set revoked_reason = 'logout' where revoked_reason = 'rotated';
alter table auth_session drop constraint ck_auth_session_reason;
alter table auth_session add constraint ck_auth_session_reason check (revoked_reason is null or revoked_reason in
    ('logout','password_changed','banned','admin_revoked','expired'));
alter table auth_session drop column if exists replaced_by;
