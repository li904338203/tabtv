<script setup lang="ts">
/**
 * 「创造」页骨架（AGT-40，设计 02 §2、§5、§6、§8）。
 *
 * ⚠️ **不挂 `auth` 中间件**：精选作品是公开的，未登录也能看；需要登录的动作
 *    （新建空白画布）在点击时才跳登录。
 * 输入框：Agent 模式（默认，AGT-43）+ 直接生成三种模式（视频 / 图片 / 音频，AGT-42）。
 * 快捷入口（设计 02 §5）：「图片 / 视频 / 音频生成」**切换输入框模式并聚焦，不跳转**；
 * 「创意助手」与运营配置的技能入口（`isQuickEntry`，按 sort 取前 6）切到 Agent 模式并插入技能胶囊；
 * 没有任何技能入口时不显示第二条分隔线及其后的部分。技能目录只在登录后读取（只读）。
 */
import type { components } from "@tabtv/api-client";
import { Image as ImageIcon, LayoutGrid, LoaderCircle, Music, Sparkles, Video, Zap } from "@lucide/vue";
import AgentComposer from "~/components/agent/composer/AgentComposer.vue";
import type { QuickMode } from "~/utils/quickGenerate";
import { loginPath } from "~/utils/safeRedirect";
import { quickSkillEntries } from "~/utils/skillEntries";

definePageMeta({ layout: "platform" });

type Project = components["schemas"]["Project"];
type AgentSkill = components["schemas"]["AgentSkill"];

/** 「创意助手」插入的技能（画布通用技能）；目录里没有就不带技能，由 Agent 自行判断。 */
const ASSISTANT_SKILL = "canvas_basics";

const auth = useAuthStore();
const prefsStore = useAgentPrefsStore();
const skills = ref<AgentSkill[]>([]);
// 「勾了快捷入口、按 sort 取前 6」与营销页同一条规则（utils/skillEntries）。
const skillEntries = computed(() => quickSkillEntries(skills.value));
const creating = ref(false);
const createError = ref("");
const composer = ref<InstanceType<typeof AgentComposer>>();
const quickEntries: Array<{ mode: QuickMode; label: string; icon: typeof Video }> = [
  { mode: "image", label: "图片生成", icon: ImageIcon },
  { mode: "video", label: "视频生成", icon: Video },
  { mode: "audio", label: "音频生成", icon: Music }
];

function useQuickMode(mode: QuickMode) {
  composer.value?.setMode(mode);
  composer.value?.focus();
}

function useAssistant() {
  const skill = skills.value.find(item => item.code === ASSISTANT_SKILL);
  void composer.value?.applySkill(skill ? { code: skill.code, name: skill.name } : null);
}

function useSkillEntry(skill: AgentSkill) {
  void composer.value?.applySkill({ code: skill.code, name: skill.name });
}

onMounted(async () => {
  if (!auth.isAuthenticated) return;
  try {
    skills.value = await prefsStore.loadSkills();
  } catch {
    // 取不到技能目录：只是没有技能入口，「创意助手」照样能用（不带技能）。
  }
});

/** 新建空项目进画布 —— 与首页 / AI 视频首页「新建项目」同一个请求。 */
async function openBlankCanvas() {
  if (!auth.isAuthenticated) {
    await navigateTo(loginPath("/create"));
    return;
  }
  if (creating.value) return;
  creating.value = true;
  createError.value = "";
  try {
    const project = await useApi()<Project>("/projects", { method: "POST", body: { name: "未命名" } });
    if (project.id) await navigateTo(`/video/canvas/${project.id}`);
  } catch (cause) {
    createError.value = (cause as { message?: string })?.message || "新建失败，请稍后重试";
  } finally {
    creating.value = false;
  }
}
</script>

<template>
  <main class="workbench-main create-page">
    <header class="create-hero">
      <h1>和 AI 一起创作</h1>
      <p>说出你的想法，从这里开始一次创作</p>
    </header>

    <!-- 输入框：Agent 模式（默认，AGT-43）+ 直接生成（AGT-42） -->
    <AgentComposer ref="composer" context="create" />

    <nav class="create-shortcuts" aria-label="快捷入口">
      <button type="button" class="create-shortcut" :disabled="creating" @click="openBlankCanvas">
        <LoaderCircle v-if="creating" :size="16" class="spin" />
        <LayoutGrid v-else :size="16" />
        <span>{{ creating ? '正在创建' : '空白画布' }}</span>
      </button>
      <span class="create-shortcut-divider" aria-hidden="true" />
      <button type="button" class="create-shortcut" @click="useAssistant">
        <Sparkles :size="16" />
        <span>创意助手</span>
      </button>
      <button v-for="entry in quickEntries" :key="entry.mode" type="button" class="create-shortcut" @click="useQuickMode(entry.mode)">
        <component :is="entry.icon" :size="16" />
        <span>{{ entry.label }}</span>
      </button>
      <template v-if="skillEntries.length">
        <span class="create-shortcut-divider" aria-hidden="true" />
        <button v-for="skill in skillEntries" :key="skill.code" type="button" class="create-shortcut" :title="skill.description" @click="useSkillEntry(skill)">
          <Zap :size="16" />
          <span>{{ skill.name }}</span>
        </button>
      </template>
    </nav>
    <p v-if="createError" class="create-error" role="alert">{{ createError }}</p>

  </main>
</template>

<style scoped>
.create-hero { margin: 40px auto 0; text-align: center; }
.create-hero h1 { margin: 0; color: var(--portal-text); font-size: 30px; font-weight: 700; }
.create-hero p { margin: 10px 0 0; color: var(--portal-muted); font-size: 13px; }
.create-shortcut-divider { width: 1px; height: 22px; align-self: center; background: var(--portal-line); }
.create-shortcuts { width: min(880px, 100%); display: flex; flex-wrap: wrap; justify-content: center; gap: 10px; margin: 18px auto 0; }
.create-shortcut {
  min-height: 36px;
  display: inline-flex;
  align-items: center;
  gap: 7px;
  border: 1px solid var(--portal-line);
  border-radius: 8px;
  padding: 0 14px;
  color: var(--portal-text-soft);
  background: var(--portal-surface);
  font-size: 12px;
  cursor: pointer;
}
/* 悬浮态用中性色，与上面的输入框统一（2026-10-25，marketing dev/03 R-12）。 */
.create-shortcut:hover:not(:disabled) { border-color: var(--portal-line-strong); color: var(--portal-text); background: var(--portal-surface-2); }
.create-shortcut:disabled { color: var(--portal-disabled-text); cursor: not-allowed; }
.create-error { margin: 10px 0 0; color: var(--portal-danger); font-size: 12px; text-align: center; }
</style>
