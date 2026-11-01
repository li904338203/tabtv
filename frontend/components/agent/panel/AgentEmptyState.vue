<script setup lang="ts">
/**
 * 空状态（设计 03 §4）：新会话、尚无消息时。推荐词随画布内容变（空画布 / 有图片 / 有选中节点），
 * 选择规则在 `utils/agentMessages.emptySuggestions`。
 *
 * 2026-11-01 按用户要求（设计 03 R-13、R-15、R-16）：
 * - 整块贴在消息区底部、紧挨输入框（不再上下居中）；
 * - 样式照参考图：顶上一行「圆形图标 + 标题」居中，推荐词是整行列表 —— 左边小图标（`suggestionIcon` 按关键词认）、
 *   中间文字、右边箭头，宽度与输入框对齐，悬停整行变底色；原来标题下的计费说明去掉了；
 * - 点击推荐词**只填进输入框、不发送**（`pick` → 面板调输入框的 `fillText`），用户看过、改过再自己点发送。
 */
import type { Component } from "vue";
import { ArrowRight, BookOpen, CircleHelp, Columns2, Film, LayoutList, Lightbulb, Link2, Music, Palette, PenLine, Sparkles, Video, WandSparkles } from "@lucide/vue";
import { emptySuggestions, suggestionIcon, type SuggestionIcon } from "~/utils/agentMessages";
import { AGENT_PANEL_BRIDGE, NOOP_BRIDGE } from "~/utils/agentPanelBridge";

defineProps<{ disabledReason?: string }>();
const emit = defineEmits<{ pick: [text: string] }>();
const canvas = useCanvasStore();
const bridge = inject(AGENT_PANEL_BRIDGE, NOOP_BRIDGE);

const ICONS: Record<SuggestionIcon, Component> = {
  music: Music,
  camera: Video,
  continue: PenLine,
  palette: Palette,
  link: Link2,
  compare: Columns2,
  polish: WandSparkles,
  story: BookOpen,
  storyboard: LayoutList,
  idea: Lightbulb,
  help: CircleHelp,
  video: Film,
  spark: Sparkles
};

// 全屏对话页不读画布 store（可能是上一个画布留下的），按空画布给建议。
const suggestions = computed(() => (bridge.canvasLoaded
  ? emptySuggestions(
    canvas.nodes.map(node => ({ id: node.id, nodeType: node.data.nodeType, title: node.data.title })),
    canvas.selectedNodeIds.length ? canvas.selectedNodeIds : canvas.selectedNodeId ? [canvas.selectedNodeId] : []
  )
  : emptySuggestions([], [])));
const items = computed(() => suggestions.value.map(text => ({ text, icon: ICONS[suggestionIcon(text)] })));
</script>

<template>
  <div class="agent-empty">
    <div class="agent-empty-head">
      <span class="agent-empty-orb" aria-hidden="true"><Sparkles :size="14" /></span>
      <p class="agent-empty-title">{{ bridge.canvasLoaded ? '我可以帮你规划并生成这张画布上的内容' : '我可以帮你规划并生成这个项目的内容' }}</p>
    </div>
    <div class="agent-empty-list">
      <button
        v-for="item in items"
        :key="item.text"
        type="button"
        class="agent-empty-item"
        :disabled="Boolean(disabledReason)"
        :title="disabledReason || '填入输入框，确认后再发送'"
        @click="emit('pick', item.text)"
      >
        <component :is="item.icon" :size="16" class="agent-empty-item-icon" />
        <span class="agent-empty-item-text">{{ item.text }}</span>
        <ArrowRight :size="15" class="agent-empty-item-arrow" />
      </button>
    </div>
  </div>
</template>
