<script setup lang="ts">
/**
 * 技能选择（设计 02 §4.3、设计 03 §2.3）：三栏 —— 分类 / 列表 / 悬停预览。
 *
 * - 数据来自 `GET /agent/skills`（缓存在 agentPrefs store，单飞）；
 * - 会话**已加载过**的技能置顶（`loadedCodes`）；
 * - 不选技能也能发送：由 Agent 自行判断加载哪个（这里只是提供一个明确指定的入口）；
 * - 由正文里的 `/关键字` 打开时，关键字来自输入框（`query`），键盘 ↑↓ 回车由输入框转发过来（`move` / `confirm`）；
 *   由按钮打开时显示自己的搜索框。
 * - 第一期不做「我的 / 收藏 / 创建技能」（设计 02 §4.3）。
 * - 场景（MKT-T-13，MKT-UX-02、MKT-AC-15）：`scene` 不传 = 原来那条请求（创造页、画布面板，行为不变）；
 *   营销页传 `marketing`，只列营销场景的技能。列表项带模型标签、预览卡带封面 —— 都是数据驱动的：
 *   技能没绑模型 / 没封面时什么都不多出来，所以对创造页现有技能逐字不变。
 * - `embedded`：窄屏「选项」全屏面板里内嵌显示（不再绝对定位、撑满父容器）。
 */
import type { components } from "@tabtv/api-client";
import { Check, CornerDownLeft, Search } from "@lucide/vue";
import { moveIndex } from "~/utils/composerKeys";
import { skillCoverUrl } from "~/utils/marketingOptions";

export type AgentSkill = components["schemas"]["AgentSkill"];
type SkillScene = components["schemas"]["AgentSkillScene"];

const props = withDefaults(defineProps<{
  /** 来自正文 `/关键字` 的过滤词；按钮打开时为 undefined（显示搜索框）。 */
  query?: string;
  loadedCodes?: string[];
  /** 场景；不传 = 不带参数的原请求（创造页）。 */
  scene?: SkillScene;
  /** 内嵌在全屏面板里（窄屏）：不绝对定位、撑满父容器。 */
  embedded?: boolean;
}>(), { query: undefined, loadedCodes: () => [], scene: undefined, embedded: false });
const emit = defineEmits<{ select: [skill: AgentSkill]; close: [] }>();

const skills = ref<AgentSkill[]>([]);
const loading = ref(false);
const error = ref("");
const category = ref("");
const search = ref("");
const activeIndex = ref(0);
const hovered = ref<AgentSkill | null>(null);
const root = ref<HTMLElement>();

const fromTrigger = computed(() => props.query !== undefined);
const keyword = computed(() => (fromTrigger.value ? props.query ?? "" : search.value).trim().toLowerCase());

const categories = computed(() => {
  const seen = new Set<string>();
  for (const skill of skills.value) if (skill.category) seen.add(skill.category);
  return [...seen];
});

const visible = computed(() => {
  const loaded = new Set(props.loadedCodes);
  return skills.value
    .filter(skill => !category.value || skill.category === category.value)
    .filter(skill => !keyword.value
      || skill.name.toLowerCase().includes(keyword.value)
      || skill.code.toLowerCase().includes(keyword.value)
      || skill.description.toLowerCase().includes(keyword.value))
    .sort((a, b) => Number(loaded.has(b.code)) - Number(loaded.has(a.code)) || a.sort - b.sort);
});

const preview = computed(() => hovered.value ?? visible.value[activeIndex.value] ?? null);

watch(visible, () => {
  if (activeIndex.value >= visible.value.length) activeIndex.value = visible.value.length ? 0 : -1;
});

const prefsStore = useAgentPrefsStore();

async function load() {
  loading.value = true;
  error.value = "";
  try {
    skills.value = await prefsStore.loadSkills(false, props.scene);
  } catch (cause) {
    error.value = cause instanceof Error && cause.message && cause.message !== "请求失败" ? cause.message : "技能列表没有取到，稍后再试";
  } finally {
    loading.value = false;
  }
}

const apiBase = useRuntimeConfig().public.apiBase as string;
/** 封面取不到（停用 / 媒体被清）的技能：退回图标，而不是显示裂图。 */
const brokenCovers = ref(new Set<string>());

function coverOf(skill: AgentSkill, width: 160 | 320): string {
  return skill.coverMediaId && !brokenCovers.value.has(skill.code) ? skillCoverUrl(apiBase, skill.code, width) : "";
}

function markBroken(code: string) {
  brokenCovers.value = new Set([...brokenCovers.value, code]);
}

/** 模型标签：技能绑定的视频 / 图片 / 推理模型显示名（服务端已解析，取不到时是 code）。 */
function modelsOf(skill: AgentSkill): string {
  const bindings = skill.modelBindings ?? {};
  return [bindings.video, bindings.image, bindings.reasoning].map(item => item?.name || item?.code || "").filter(Boolean).join(" · ");
}

function tagsOf(skill: AgentSkill): string[] {
  return (skill.tags ?? []).map(tag => (typeof tag === "string" ? tag : "")).filter(Boolean);
}

function move(delta: number) {
  hovered.value = null;
  activeIndex.value = moveIndex(activeIndex.value, delta, visible.value.length);
  nextTick(() => root.value?.querySelector(".skill-item.active")?.scrollIntoView({ block: "nearest" }));
}

function confirm() {
  const skill = visible.value[activeIndex.value];
  if (skill) emit("select", skill);
  else emit("close");
}

function onOutside(event: PointerEvent) {
  if (root.value && event.target instanceof Node && !root.value.contains(event.target)) emit("close");
}

onMounted(() => {
  void load();
  document.addEventListener("pointerdown", onOutside, true);
});
onBeforeUnmount(() => document.removeEventListener("pointerdown", onOutside, true));

defineExpose({ move, confirm });
</script>

<template>
  <div ref="root" class="skill-picker" :class="{ embedded }" role="dialog" aria-label="选择技能" @keydown.esc.stop="emit('close')">
    <div v-if="!fromTrigger" class="skill-search">
      <Search :size="14" />
      <input v-model="search" type="search" placeholder="搜索技能" aria-label="搜索技能" @keydown.down.prevent="move(1)" @keydown.up.prevent="move(-1)" @keydown.enter.prevent="confirm">
    </div>
    <div class="skill-columns">
      <ul class="skill-categories" aria-label="分类">
        <li>
          <button type="button" :class="{ active: !category }" @click="category = ''">
            <Check v-if="!category" class="cat-mark" :size="13" /><i v-else class="cat-dot" />全部
          </button>
        </li>
        <li v-for="item in categories" :key="item">
          <button type="button" :class="{ active: category === item }" :title="item" @click="category = item">
            <Check v-if="category === item" class="cat-mark" :size="13" /><i v-else class="cat-dot" />{{ item }}
          </button>
        </li>
      </ul>
      <ul class="skill-list" role="listbox" aria-label="技能">
        <li v-if="loading" class="skill-empty">加载中…</li>
        <li v-else-if="error" class="skill-empty error">{{ error }}</li>
        <li v-else-if="!visible.length" class="skill-empty">{{ keyword ? '没有匹配的技能' : '还没有可用的技能' }}</li>
        <li
          v-for="(skill, index) in visible"
          :key="skill.code"
          class="skill-item"
          :class="{ active: index === activeIndex }"
          role="option"
          :aria-selected="index === activeIndex"
          @mouseenter="hovered = skill; activeIndex = index"
          @mouseleave="hovered = null"
          @mousedown.prevent
          @click="emit('select', skill)"
        >
          <span class="skill-text">
            <span class="skill-name">{{ skill.name }}</span>
            <span v-if="modelsOf(skill)" class="skill-models">{{ modelsOf(skill) }}</span>
          </span>
          <small v-if="loadedCodes.includes(skill.code)">本会话已用</small>
          <CornerDownLeft v-if="index === activeIndex" class="skill-enter" :size="13" aria-hidden="true" />
        </li>
      </ul>
      <aside class="skill-preview" aria-live="polite">
        <div v-if="preview" class="skill-card">
          <img v-if="coverOf(preview, 320)" :key="preview.code" class="skill-cover" :src="coverOf(preview, 320)" alt="" @error="markBroken(preview.code)">
          <div v-else class="skill-cover skill-cover-empty" aria-hidden="true" />
          <strong>{{ preview.name }}</strong>
          <p v-if="modelsOf(preview)" class="skill-card-models">{{ modelsOf(preview) }}</p>
          <p class="skill-card-desc">{{ preview.description || '暂无说明' }}</p>
          <div class="skill-chips">
            <span v-if="preview.category">{{ preview.category }}</span>
            <span v-for="tag in tagsOf(preview)" :key="tag">{{ tag }}</span>
            <span v-if="preview.steps?.length">共 {{ preview.steps.length }} 步</span>
          </div>
        </div>
        <p v-else class="skill-tags">不选技能也能发送：Agent 会自己判断用哪个</p>
      </aside>
    </div>
  </div>
</template>

<style scoped>
.skill-picker {
  position: absolute;
  left: 0;
  right: 0;
  bottom: calc(100% + 8px);
  z-index: 90;
  border: 1px solid var(--portal-line-strong);
  border-radius: 14px;
  color: var(--portal-text);
  background: var(--portal-elevated);
  box-shadow: 0 18px 48px var(--portal-shadow-strong);
  font-size: 12px;
  overflow: hidden;
  display: flex;
  flex-direction: column;
  /* 由输入框按可用空间给出（见 utils/pickerPlacement）；没给时不限 */
  max-height: var(--picker-max, none);
}
/* 上方放不下时向下弹 */
.skill-picker.picker-bottom { bottom: auto; top: calc(100% + 8px); }
.skill-search { display: flex; align-items: center; gap: 6px; padding: 8px 10px; border-bottom: 1px solid var(--portal-line-soft); color: var(--portal-muted); }
.skill-search input { flex: 1; border: 0; outline: 0; color: var(--portal-text); background: transparent; font: inherit; }
/* 分类列 144px：分类名最长 7 个字（「视频分析与修改」）+ 前面的勾 / 圆点；88px 时会折成两三行（2026-11-01 细分分类后） */
.skill-columns { display: grid; grid-template-columns: 144px minmax(0, 1fr) minmax(240px, 1fr); height: 340px; flex: 0 1 auto; min-height: 96px; }
.skill-categories, .skill-list { margin: 0; padding: 8px 6px; list-style: none; overflow-y: auto; }
/* 分类：选中打勾、其余一个小圆点（2026-11-01 按参考样式），不再用分隔线 */
.skill-categories button {
  width: 100%;
  display: flex;
  align-items: center;
  gap: 8px;
  border: 0;
  border-radius: 8px;
  padding: 7px 8px;
  color: var(--portal-text-soft);
  background: transparent;
  text-align: left;
  font: inherit;
  font-size: 13px;
  cursor: pointer;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.skill-categories button:hover { color: var(--portal-text); background: var(--portal-hover); }
.skill-categories button.active { color: var(--portal-text); font-weight: 600; }
.cat-mark { flex: none; }
.cat-dot { flex: none; width: 13px; height: 13px; position: relative; }
.cat-dot::after { content: ""; position: absolute; left: 5px; top: 5px; width: 3px; height: 3px; border-radius: 50%; background: var(--portal-faint); }
/* 技能列表：名称 + 一行模型，选中项右侧提示回车 */
.skill-item { display: flex; align-items: center; gap: 8px; border-radius: 10px; padding: 9px 12px; color: var(--portal-text-soft); cursor: pointer; }
.skill-item + .skill-item { margin-top: 2px; }
.skill-item.active { color: var(--portal-text); background: var(--portal-hover); }
.skill-text { flex: 1; min-width: 0; display: grid; gap: 3px; }
.skill-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; font-weight: 500; }
.skill-models { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--portal-muted); font-size: 11px; }
.skill-enter { flex: none; color: var(--portal-muted); }
.skill-item small { color: var(--portal-muted); }
.skill-empty { padding: 10px 8px; color: var(--portal-muted); }
.skill-empty.error { color: var(--portal-danger); }
/* 预览：一张圆角卡片，封面在上（竖版封面按中心裁成宽图，出图时主体就是居中的） */
.skill-preview { padding: 10px; overflow-y: auto; }
.skill-card { display: grid; align-content: start; gap: 8px; padding: 8px 8px 12px; border-radius: 12px; background: var(--portal-panel); }
.skill-cover { width: 100%; height: 132px; border-radius: 8px; object-fit: cover; background: var(--portal-hover); }
.skill-card strong { padding: 0 4px; font-size: 13px; }
.skill-card p { margin: 0; padding: 0 4px; line-height: 18px; }
.skill-card-models { color: var(--portal-muted); font-size: 11px; }
.skill-card-desc { color: var(--portal-text-soft); }
.skill-chips { display: flex; flex-wrap: wrap; gap: 6px; padding: 2px 4px 0; }
.skill-chips span { border: 1px solid var(--portal-line); border-radius: 6px; padding: 1px 7px; color: var(--portal-muted); font-size: 11px; line-height: 18px; }
.skill-tags { margin: 0; color: var(--portal-muted); }
/* 亮色主题下白底封面会「消失」：加 1px 描边（dev/03 §9） */
:root[data-theme="light"] .skill-cover { box-shadow: 0 0 0 1px var(--portal-line); }
/* 窄屏全屏面板内嵌：不再浮在输入框上方 */
.skill-picker.embedded { position: static; height: 100%; max-height: none; border: 0; border-radius: 0; box-shadow: none; }
.skill-picker.embedded .skill-columns { height: auto; flex: 1 1 auto; grid-template-columns: 144px minmax(0, 1fr); }
.skill-picker.embedded .skill-preview { display: none; }
</style>
