<script setup lang="ts">
/**
 * 文本节点的卡片：手写文本（「尝试：自己编写内容」）与**生成结果**共用。按需加载（编辑器只在有文本节点时才取，
 * 画布页首屏包体积有上限，scripts/director-bundle-check.mjs）。
 *
 * 交互（2026-10-30 用户要求）：
 * - 平时是 **Markdown 预览**，单击 / 悬停 / 按住拖动都和别的节点一样（能拖动节点）；滚轮滚动内容；
 * - **双击进入编辑，编辑也在预览里进行**（所见即所得：Tiptap + @tiptap/markdown，内容以 Markdown 存取）；
 *   点卡片外面或按 Esc 退出编辑。编辑中加 `nodrag`，在里面拖选文字不会带动节点。
 *
 * 存哪里：
 * - 手写：`params.text`；
 * - 生成结果（`output` 有值）：改了之后存 `params.text` + `params.textEditedFor = 那次生成的 ID`，下游按改过的文字取输入
 *   （utils/canvas `editedGenerationText`）；重新生成后 ID 变了，显示新结果。
 */
import { EditorContent, useEditor } from "@tiptap/vue-3";
import StarterKit from "@tiptap/starter-kit";
import { Markdown } from "@tiptap/markdown";
import { Placeholder } from "@tiptap/extensions";
import type { CanvasNodeData } from "~/types/canvas";
import { editedGenerationText } from "~/utils/canvas";

const props = defineProps<{ nodeId: string; params: CanvasNodeData["params"]; autofocus?: boolean; output?: string; generationId?: string }>();
const canvas = useCanvasStore();
const root = ref<HTMLElement | null>(null);

/** 当前应当显示的 Markdown 原文。 */
const source = computed(() => (props.output === undefined
  ? String(props.params.text ?? "")
  : editedGenerationText(props.params, props.generationId) ?? props.output));

const editing = ref(Boolean(props.autofocus));

function save(markdown: string) {
  if (markdown === source.value) return;
  canvas.markTouched(props.nodeId);
  canvas.updateParams(props.nodeId, props.output === undefined
    ? { ...props.params, text: markdown }
    : { ...props.params, text: markdown, textEditedFor: props.generationId ?? "" });
}

const editor = useEditor({
  content: source.value,
  contentType: "markdown",
  editable: editing.value,
  extensions: [
    // 链接在画布里点了不跳转（单击是选中 / 拖动节点）
    StarterKit.configure({ link: { openOnClick: false } }),
    Markdown,
    Placeholder.configure({ placeholder: "输入内容..." })
  ],
  onUpdate: ({ editor: e }) => {
    if (editing.value) save(e.getMarkdown());
  }
});

// 外部变了（重新生成出了新结果、撤销、协作方改了）且不在编辑中：换成新内容
watch(source, (next) => {
  const e = editor.value;
  if (!e || editing.value || next === e.getMarkdown()) return;
  e.commands.setContent(next, { contentType: "markdown", emitUpdate: false });
});

function startEdit() {
  const e = editor.value;
  if (!e) return;
  editing.value = true;
  e.setEditable(true);
  void nextTick(() => e.commands.focus("end"));
}
function stopEdit() {
  const e = editor.value;
  if (!e || !editing.value) return;
  save(e.getMarkdown());
  editing.value = false;
  e.setEditable(false);
}
function onPointerDownOutside(event: PointerEvent) {
  if (editing.value && root.value && event.target instanceof Node && !root.value.contains(event.target)) stopEdit();
}
function onKeydown(event: KeyboardEvent) {
  if (event.key === "Escape") {
    event.stopPropagation();
    stopEdit();
  }
}
onMounted(() => {
  document.addEventListener("pointerdown", onPointerDownOutside, true);
  if (editing.value) void nextTick(() => editor.value?.commands.focus("end"));
});
onBeforeUnmount(() => {
  document.removeEventListener("pointerdown", onPointerDownOutside, true);
  if (editing.value && editor.value) save(editor.value.getMarkdown());
});
</script>

<template>
  <div
    ref="root"
    class="node-rich-text nowheel"
    :class="{ editing, nodrag: editing, 'is-output': output !== undefined }"
    :data-testid="output === undefined ? 'node-manual-text' : 'node-text-result'"
    :title="editing ? '' : '双击编辑'"
    @dblclick.stop="startEdit"
    @keydown="onKeydown"
  >
    <EditorContent :editor="editor" class="node-rich-text-content" />
  </div>
</template>

<style scoped>
.node-rich-text {
  height: 300px; min-height: 160px; overflow-y: auto; resize: vertical; border-radius: 8px;
  padding: 16px 20px; color: var(--cv-n90, var(--canvas-text)); font-size: 14px; line-height: 1.7;
}
/* 预览：不选文字（按住是拖节点），光标是默认箭头 */
.node-rich-text:not(.editing) { cursor: default; user-select: none; -webkit-user-select: none; }
.node-rich-text.editing { box-shadow: inset 0 0 0 2px var(--canvas-cyan); cursor: text; }
.node-rich-text :deep(.ProseMirror) { min-height: 100%; outline: none; overflow-wrap: anywhere; word-break: break-word; }
.node-rich-text :deep(.ProseMirror > :first-child) { margin-top: 0; }
.node-rich-text :deep(.ProseMirror > :last-child) { margin-bottom: 0; }
.node-rich-text :deep(p) { margin: 0 0 .6em; }
.node-rich-text :deep(h1) { margin: .6em 0 .4em; font-size: 22px; font-weight: 700; line-height: 1.35; }
.node-rich-text :deep(h2) { margin: .6em 0 .4em; font-size: 19px; font-weight: 700; line-height: 1.4; }
.node-rich-text :deep(h3) { margin: .5em 0 .3em; font-size: 16px; font-weight: 700; }
.node-rich-text :deep(h4), .node-rich-text :deep(h5), .node-rich-text :deep(h6) { margin: .5em 0 .3em; font-size: 14px; font-weight: 700; }
.node-rich-text :deep(ul), .node-rich-text :deep(ol) { margin: 0 0 .6em; padding-left: 1.4em; }
.node-rich-text :deep(li > p) { margin: 0 0 .2em; }
.node-rich-text :deep(blockquote) { margin: 0 0 .6em; padding-left: 12px; border-left: 3px solid var(--canvas-line); color: var(--cv-n70, var(--canvas-muted)); }
.node-rich-text :deep(code) { padding: 1px 5px; border-radius: 4px; background: var(--cv-n22, rgba(0, 0, 0, .06)); font-size: 12.5px; }
.node-rich-text :deep(pre) { margin: 0 0 .6em; padding: 10px 12px; overflow-x: auto; border-radius: 8px; background: var(--cv-n22, rgba(0, 0, 0, .06)); }
.node-rich-text :deep(pre code) { padding: 0; background: transparent; }
.node-rich-text :deep(hr) { margin: .8em 0; border: 0; border-top: 1px solid var(--canvas-line); }
.node-rich-text :deep(a) { color: var(--canvas-cyan); }
.node-rich-text :deep(p.is-editor-empty:first-child::before) {
  content: attr(data-placeholder); float: left; height: 0; color: var(--cv-n40, var(--canvas-muted)); pointer-events: none; font-size: 15px;
}
</style>
