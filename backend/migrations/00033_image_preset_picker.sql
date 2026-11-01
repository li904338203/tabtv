-- 图片预设：恢复老系统定稿的「玩法」目录，工具另归一组（P15-02）
--
-- 起因：用户要求图片节点的「图片预设」弹窗与上方工具条的「九宫格」下拉照老系统来。
-- 老系统（已评审定稿）是 15 个玩法，分四组：分镜叙事（调度故事板、故事板、
-- 25 宫格连贯分镜、剧情推演四宫格、画面推演 3 秒后 / 5 秒前）· 质感调节 ·
-- 空间与机位（720 全景、多机位九宫格）· 设定图（五种）。新系统 seed 里只有 P09 做
-- 「图片加工工具」时另设计的 9 个（涂抹重绘、抠图、高清放大……），**整个分镜叙事组
-- 都没有，九宫格/四宫格/25 宫格一个都没有**。
--
-- 两类东西在老 UI 里的位置不同：
--   · 玩法（picker）—— 出现在「图片预设」弹窗与「九宫格」下拉里；
--   · 工具（tool）  —— 对应工具条上的按钮（人像质感、打光、高清、抠图、涂抹重绘……），
--     其中涂抹重绘要蒙版编辑器，放进预设弹窗里点一下是用不对的。
-- 所以工具单独归 `tool` 组：仍然启用、`/image-tools` 照常能查到（P09 的验收与 E2E
-- 不受影响，代号一个不改），只是不进选择器。
--
-- 另加两列，都是老系统有、新表没有的：
--   · description —— 选中预设后提示词框的占位文案（「描述一个 10-15 秒内能演完的小剧情…」），
--     没有它用户不知道这个玩法要他写什么；
--   · is_new      —— 选择器里的 NEW 角标。
-- +goose Up
alter table image_preset add column description varchar(300);
alter table image_preset add column is_new boolean not null default false;

alter table image_preset drop constraint ck_preset_group;
alter table image_preset add constraint ck_preset_group
    check (preset_group in ('sheet', 'tuning', 'storyboard', 'space', 'tool'));

-- +goose Down
-- ⚠️ 回退前工具组的行要先挪回原组，否则加回旧约束会失败。
update image_preset set preset_group = case
    when code in ('character_sheet', 'prop_sheet') then 'sheet'
    when code in ('panorama', 'multi_angle') then 'space'
    else 'tuning' end
where preset_group = 'tool';
alter table image_preset drop constraint ck_preset_group;
alter table image_preset add constraint ck_preset_group
    check (preset_group in ('sheet', 'tuning', 'storyboard', 'space'));
alter table image_preset drop column if exists is_new;
alter table image_preset drop column if exists description;
