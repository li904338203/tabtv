<script setup lang="ts">
/**
 * 「营销 Agent」页骨架（MKT-T-10，营销 dev/03 §1、§2；MKT-F-01~03）。
 *
 * 布局：① 标题区 → ② 营销输入框 → ③ 快捷技能芯片 → ④ 模板墙。
 * 本卡只负责布局与 `/marketing/config` 的加载；②③④ 的本体由后续卡填进下面的挂载位：
 * - ② 输入框（含商品 / 角色 / 源视频槽位 MKT-T-12、选项弹层 MKT-T-13）—— MKT-T-14；
 * - ③ 快捷技能芯片 —— MKT-T-14；④ 模板墙 —— MKT-T-15。
 * 发送全部委托给输入框组件（同 `create.vue` 的分工），页面不碰发送流程。
 *
 * ⚠️ **不挂 `auth` 中间件**：模板墙公开，未登录也能浏览（MKT-F-03）；需要登录的动作在点击时才跳登录。
 * ⚠️ 顶栏没有路由 → 标题映射（B-6），标题由页面自己 `useHead`。
 * ⚠️ 三种状态分开说：总开关关（404）→「暂未开放」；Agent 关（`available:false`，B-5）→ 照常浏览、
 *    只说清发不出去；加载失败 → 只在本页报错并给重试（侧栏入口照常在）。
 *
 * 快捷技能芯片（MKT-T-14，MKT-F-20~22）：营销场景里勾了快捷入口的技能按 sort 取前 6 个（与创造页同一个
 * `quickSkillEntries`）+「全部技能 →」；点 = 选中技能（插胶囊 + 联动），**不发送**。技能目录要登录（B-5），
 * 未登录不显示芯片；取不到目录也只是没有芯片。
 */
import type { components } from "@tabtv/api-client";
import { ArrowRight, LoaderCircle, Megaphone, RotateCw, Zap } from "@lucide/vue";
import AgentComposer from "~/components/agent/composer/AgentComposer.vue";
import { skillCoverUrl } from "~/utils/marketingOptions";
import { quickSkillEntries } from "~/utils/skillEntries";

type AgentSkill = components["schemas"]["AgentSkill"];

definePageMeta({ layout: "platform" });
useHead({ title: "营销 Agent" });

const marketing = useMarketingConfig();
const { state, config, error } = marketing;

/** 能不能发送：只有拿到配置且 `available` 时。交给输入框（MKT-T-14）禁用发送按钮用。 */
const canSend = computed(() => state.value === "ready" && config.value?.available === true);

/** `available:false` 时的说明；按 `unavailableReason` 分流，不匹配文案。 */
const unavailableNote = computed(() => {
  if (state.value !== "ready" || !config.value || config.value.available) return "";
  if (config.value.unavailableReason === "agent_disabled") return "Agent 功能暂未开启，现在可以浏览模板，暂时无法发送。";
  return "营销 Agent 暂时无法发送，可以先浏览模板。";
});

const auth = useAuthStore();
const workspace = useWorkspaceStore();
/** 只读成员：芯片照常显示但点不动（理由在悬停里；输入框下方另有完整说明，MKT-UX-10）。 */
const readOnly = computed(() => workspace.current?.role === "viewer");
const prefsStore = useAgentPrefsStore();
const apiBase = useRuntimeConfig().public.apiBase as string;
const composer = ref<InstanceType<typeof AgentComposer>>();
const skills = ref<AgentSkill[]>([]);
const chips = computed(() => quickSkillEntries(skills.value));
/** 封面取不到的技能：退回图标。 */
const brokenCovers = ref(new Set<string>());

function chipCover(skill: AgentSkill): string {
  return skill.coverMediaId && !brokenCovers.value.has(skill.code) ? skillCoverUrl(apiBase, skill.code, 160) : "";
}

function markBroken(code: string) {
  brokenCovers.value = new Set([...brokenCovers.value, code]);
}

async function loadSkills() {
  if (!auth.isAuthenticated) return;
  try {
    skills.value = await prefsStore.loadSkills(false, "marketing");
  } catch {
    // 取不到技能目录：只是没有快捷芯片，输入框与模板墙照常可用。
    skills.value = [];
  }
}

onMounted(() => {
  void marketing.load();
  void loadSkills();
});
</script>

<template>
  <main class="workbench-main marketing-page">
    <!-- 总开关关着：直达也只说「暂未开放」，不露出任何半成品 -->
    <section v-if="state === 'disabled'" class="marketing-closed" role="status">
      <Megaphone :size="28" :stroke-width="1.5" />
      <h1>营销 Agent 暂未开放</h1>
      <p>这个功能还没有上线，可以先去「创造」开始一次创作。</p>
      <NuxtLink to="/create" class="marketing-closed-link">去创造</NuxtLink>
    </section>

    <template v-else>
      <!-- ① 标题区 -->
      <header class="marketing-hero">
        <h1>营销 Agent</h1>
        <!-- 副标题文案待运营定（MKT-F-02），先用需求里的说明 -->
        <p>选创意、Hook 与风格，让 Agent 补全生成方案</p>
      </header>

      <p v-if="state === 'idle' || state === 'loading'" class="marketing-loading" role="status">
        <LoaderCircle :size="16" class="spin" /><span>正在加载</span>
      </p>

      <div v-else-if="state === 'error'" class="marketing-error" role="alert">
        <span>{{ error }}</span>
        <button type="button" class="marketing-retry" @click="marketing.reload()">
          <RotateCw :size="14" /><span>重试</span>
        </button>
      </div>

      <template v-else-if="config">
        <p v-if="unavailableNote" class="marketing-unavailable" role="status">{{ unavailableNote }}</p>

        <!-- ② 营销输入框（MKT-T-14；槽位 MKT-T-12、选项弹层 MKT-T-13 在输入框内部）。发送委托给输入框。 -->
        <section class="marketing-composer-slot" data-mount="marketing-composer" :data-can-send="canSend" aria-label="营销输入框">
          <AgentComposer ref="composer" context="marketing" :marketing-config="config" :marketing-available="canSend" />
        </section>

        <!-- ③ 快捷技能芯片（MKT-T-14）。未登录看不到技能目录（B-5），整块不渲染。点 = 选中技能，不发送。 -->
        <nav v-if="chips.length" class="marketing-chips-slot" data-mount="marketing-skill-chips" aria-label="快捷技能">
          <button
            v-for="skill in chips"
            :key="skill.code"
            type="button"
            class="marketing-chip"
            :disabled="readOnly"
            :title="readOnly ? '你在该工作区是只读，无法发起创作' : (skill.description || skill.name)"
            @click="composer?.applyMarketingSkill(skill)"
          >
            <img v-if="chipCover(skill)" :src="chipCover(skill)" alt="" class="marketing-chip-cover" loading="lazy" @error="markBroken(skill.code)">
            <Zap v-else :size="14" />
            <span>{{ skill.name }}</span>
          </button>
          <button type="button" class="marketing-chip marketing-chip-all" :disabled="readOnly" @click="composer?.openSkills()">
            <span>全部技能</span><ArrowRight :size="13" />
          </button>
        </nav>

        <!-- ④ 模板墙挂载位（MKT-T-15）。公开，未登录可浏览。 -->
        <section class="marketing-wall-slot" data-mount="marketing-template-wall" aria-label="模板">
          <!-- 点卡片只填入输入框（useMarketingFill → 输入框消费），不发送、不建会话（MK-16） -->
          <MarketingTemplateWall :config="config" />
        </section>
      </template>
    </template>
  </main>
</template>

<style scoped>
.marketing-hero { margin: 40px auto 0; text-align: center; }
.marketing-hero h1 { margin: 0; color: var(--portal-text); font-size: 30px; font-weight: 700; }
.marketing-hero p { margin: 10px 0 0; color: var(--portal-muted); font-size: 13px; }

/*
 * 输入框比创造页宽一些（左侧多一列槽位区，对标截图约 920px）；芯片一行居中；模板墙与精选作品同宽（占满内容区）。
 * ⚠️ 输入框挂载位要在模板墙之上叠放：弹层（技能 / 选项）向下弹时会盖住芯片与模板墙。
 */
.marketing-composer-slot { position: relative; z-index: 3; width: min(920px, 100%); margin: 24px auto 0; }
.marketing-chips-slot { width: min(920px, 100%); display: flex; flex-wrap: wrap; justify-content: center; gap: 10px; margin: 18px auto 0; }
.marketing-chip {
  min-height: 36px;
  display: inline-flex;
  align-items: center;
  gap: 7px;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  padding: 0 12px 0 6px;
  color: var(--portal-text-soft);
  background: var(--portal-surface);
  font-size: 12px;
  cursor: pointer;
}
.marketing-chip:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
/* 悬浮态用中性色，与上面的输入框统一（2026-10-25，marketing dev/03 R-12）。 */
.marketing-chip:hover:not(:disabled) { border-color: var(--portal-line-strong); color: var(--portal-text); background: var(--portal-surface-2); }
.marketing-chip > svg:first-child { margin-left: 6px; }
.marketing-chip-cover { width: 26px; height: 26px; border-radius: 5px; object-fit: cover; }
.marketing-chip-all { padding: 0 12px; border-color: transparent; background: transparent; }
.marketing-wall-slot { margin-top: 56px; }

.marketing-loading { display: flex; align-items: center; justify-content: center; gap: 8px; margin: 32px 0 0; color: var(--portal-muted); font-size: 13px; }
.marketing-error, .marketing-unavailable {
  width: min(880px, 100%);
  margin: 24px auto 0;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  padding: 10px 14px;
  font-size: 13px;
}
.marketing-error { display: flex; align-items: center; justify-content: space-between; gap: 12px; color: var(--portal-danger); }
.marketing-unavailable { color: var(--portal-muted); background: var(--portal-surface); text-align: center; }
.marketing-retry {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-height: 30px;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  padding: 0 12px;
  color: var(--portal-text);
  background: var(--portal-surface);
  font-size: 12px;
  cursor: pointer;
}
.marketing-retry:hover { background: var(--portal-hover); }

.marketing-closed { display: grid; justify-items: center; gap: 10px; margin: 96px auto 0; color: var(--portal-muted); text-align: center; }
.marketing-closed h1 { margin: 6px 0 0; color: var(--portal-text); font-size: 22px; font-weight: 700; }
.marketing-closed p { margin: 0; font-size: 13px; }
.marketing-closed-link {
  margin-top: 8px;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  padding: 7px 16px;
  color: var(--portal-text);
  font-size: 13px;
  text-decoration: none;
}
.marketing-closed-link:hover { background: var(--portal-hover); }

.spin { animation: marketing-spin 1s linear infinite; }
@keyframes marketing-spin { to { transform: rotate(360deg); } }
</style>
