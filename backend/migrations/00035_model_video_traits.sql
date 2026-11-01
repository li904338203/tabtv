-- 模型目录：视频模型的三项能力 —— 单次最长时长 / 是否支持多镜头 / 产出是否自带音轨（AGT-05，AG-P6）
--
-- 起因：画布 Agent 要把一个成片需求分成「单段直出」还是「多段拼接」（AGT-50），
-- 分界线是「这个模型一次最多能出几秒」。没有这一列的话只能在代码里写死 15 秒 ——
-- 上游放开一档（10 → 15 → 20 秒）就得改代码发版，而不同模型的上限本来就各不相同，
-- 写死的那个数对一半模型是错的，表现是「本可一段出完的片子被拆成三段、多花三次钱」。
-- 多镜头与自带音轨同理：规划时要知道一段里能不能切镜、要不要另外叠配乐。
--
-- ⚠️ 这三项只是**规划阶段的提示**，不是事实：
--   · 渲染补静音仍以 ffprobe 探到的真实音轨为准（CLAUDE.md §5.10）—— 模型标「自带音轨」
--     而某次产物恰好没有音轨时，按标记跳过补静音会让后面每段音画错位，而任务"成功"；
--   · 报价、受理、能力矩阵、model_param_support 都不读这三列。
--
-- 取值：max_duration_sec 为 null = 未配置（分流方按保守策略处理，不猜一个数）；
-- 两个布尔默认 false。三项只对 domain='video' 有意义，非视频模型由领域校验拒绝设置。
-- +goose Up
alter table model add column max_duration_sec integer;
alter table model add column is_multi_shot boolean not null default false;
alter table model add column has_native_audio boolean not null default false;
alter table model add constraint ck_model_max_duration
    check (max_duration_sec is null or max_duration_sec > 0);

-- +goose Down
alter table model drop constraint if exists ck_model_max_duration;
alter table model drop column if exists has_native_audio;
alter table model drop column if exists is_multi_shot;
alter table model drop column if exists max_duration_sec;
