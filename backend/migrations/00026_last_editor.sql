-- 画布冲突要能说出是**谁**改的（P12-02；NEW-19、AC-03、FE-03）
--
-- 起因：P04-03 的冲突详情只给 `currentVersion` 与 `currentUpdatedAt`，
-- 用户看到的是一句「画布已被其他窗口修改」—— 而「其他窗口」有两种，
-- 他要做的事完全不同：
--   · 自己的另一个标签页 → 直接「以我为准」覆盖就行；
--   · 同事 → 覆盖等于抹掉别人的工作，他该先去问一句。
-- 分不出来的话，用户只能凭猜；而猜错的那一半正是 NEW-19 要防的
-- 「无声覆盖另一成员的修改」。
--
-- ⚠️ 记的是**最后一次成功保存的人**，不是"正在编辑的人"。后者是 presence
-- （Redis 里的心跳，会过期），两者不能混：一个是已经发生的事实、
-- 另一个是此刻的状态，把 presence 落库的话，进程崩溃就会留下永远在线的幽灵。
-- +goose Up
alter table project add column last_editor_user_id bigint;

-- +goose Down
alter table project drop column last_editor_user_id;
