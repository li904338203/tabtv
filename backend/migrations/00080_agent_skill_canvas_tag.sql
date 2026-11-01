-- 画布技能的标签「通用」→「工作流」（2026-11-01，接 00079：用户要求去掉「通用」，技能弹层的预览里标签仍显示「画布 · 通用」）。
-- 只改仍是 seed 原值的行：运营在管理端改过的标签不覆盖。
-- +goose Up
update agent_skill set tags = '["画布", "工作流"]'::jsonb where code = 'canvas_basics' and tags = '["画布", "通用"]'::jsonb;

-- +goose Down
update agent_skill set tags = '["画布", "通用"]'::jsonb where code = 'canvas_basics' and tags = '["画布", "工作流"]'::jsonb;
