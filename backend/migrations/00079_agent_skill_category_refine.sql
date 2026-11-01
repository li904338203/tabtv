-- 通用技能分类细分（2026-11-01 用户要求：去掉「通用」这个分类、把通用技能再细分）。
-- 起因：通用场景的 13 个技能只有「通用 / 视频创作 / 图片创作」三类，「视频创作」一类就装了 8 个，
--   技能弹层里按分类筛选几乎不起作用；「通用」也说不清它是什么。细分成 7 类：
--   画布与工作流 · 创意策划 · 视频生成 · 视频分析与修改 · 声音与配乐 · 图片生成 · 图片分析与复核。
--   分类是自由文本、弹层按 sort 的首次出现排顺序，所以「配乐与声音设计」的 sort 42 → 55，排到视频两类之后。
-- ⚠️ 技能由 seed 以 on conflict do nothing 写入，改 Go 里的 seed 不会下发到已有环境，所以走迁移。
--   每条都带「仍是旧值」的条件：运营在管理端改过的分类 / 排序不覆盖。营销场景的技能不在此列。
-- +goose Up
update agent_skill set category = '画布与工作流' where code = 'canvas_basics' and category = '通用';
update agent_skill set category = '创意策划' where code in ('story_topic', 'film_multishot') and category = '视频创作';
update agent_skill set category = '视频生成' where code in ('video_single', 'video_extend', 'storyboard_grid_video') and category = '视频创作';
update agent_skill set category = '视频分析与修改' where code in ('video_fix', 'video_breakdown') and category = '视频创作';
update agent_skill set category = '声音与配乐' where code = 'sound_design' and category = '视频创作';
update agent_skill set sort = 55 where code = 'sound_design' and sort = 42;
update agent_skill set category = '图片生成' where code in ('character_refs', 'image_craft') and category = '图片创作';
update agent_skill set category = '图片分析与复核' where code in ('image_reverse_prompt', 'image_review') and category = '图片创作';

-- +goose Down
update agent_skill set category = '通用' where code = 'canvas_basics' and category = '画布与工作流';
update agent_skill set category = '视频创作'
where code in ('story_topic', 'film_multishot', 'video_single', 'video_extend', 'storyboard_grid_video', 'video_fix', 'video_breakdown', 'sound_design')
  and category in ('创意策划', '视频生成', '视频分析与修改', '声音与配乐');
update agent_skill set sort = 42 where code = 'sound_design' and sort = 55;
update agent_skill set category = '图片创作'
where code in ('character_refs', 'image_craft', 'image_reverse_prompt', 'image_review') and category in ('图片生成', '图片分析与复核');
