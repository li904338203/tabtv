<script setup lang="ts">
import {
  pickMentionSuggestions,
  scriptMentionTypeLabel,
  segmentScriptMentions,
  type ScriptMentionAsset,
  type ScriptMentionSegment,
  type ScriptMentionType
} from "~/utils/scriptMentions";

const props = withDefaults(defineProps<{
  modelValue: string;
  assets?: ScriptMentionAsset[];
  placeholder?: string;
  /** 无障碍标签：同一个编辑器也用于最终提示词，标签不能写死成"画面描述" */
  label?: string;
}>(), {
  assets: () => [],
  placeholder: "描述镜头画面，输入 @ 引用人物、场景或道具",
  label: "画面描述"
});

const emit = defineEmits<{
  "update:modelValue": [value: string];
  dismiss: [];
  submit: [];
}>();

const editor = ref<HTMLElement>();
const mentionRange = shallowRef<Range>();
const mentionQuery = ref("");
const activeMentionIndex = ref(0);
const isComposing = ref(false);

const mentionSuggestions = computed(() =>
  mentionRange.value ? pickMentionSuggestions(props.assets, mentionQuery.value) : []);

const isEmpty = computed(() => !props.modelValue.replaceAll("\u200B", "").trim());

function mentionType(value?: string | null): ScriptMentionType {
  if (value === "character" || value === "scene" || value === "prop") return value;
  return "unknown";
}

function mentionClass(value?: string | null) {
  return `mention-${mentionType(value)}`;
}

function createMentionElement(segment: ScriptMentionSegment) {
  const element = document.createElement("span");
  element.className = `script-description-mention ${mentionClass(segment.type)}`;
  element.contentEditable = "false";
  element.dataset.scriptMention = "true";
  element.dataset.token = segment.text;
  if (segment.assetId) element.dataset.assetId = segment.assetId;
  element.title = `${scriptMentionTypeLabel(mentionType(segment.type))}：${segment.name ?? segment.text}`;
  element.textContent = segment.text;
  return element;
}

function renderEditor(value: string) {
  const root = editor.value;
  if (!root) return;
  const fragment = document.createDocumentFragment();
  for (const segment of segmentScriptMentions(value, props.assets)) {
    if (!segment.mention) {
      fragment.appendChild(document.createTextNode(segment.text));
      continue;
    }
    fragment.appendChild(createMentionElement(segment));
    fragment.appendChild(document.createTextNode("\u200B"));
  }
  root.replaceChildren(fragment);
}

function editorText() {
  return (editor.value?.innerText ?? "")
    .replaceAll("\u200B", "")
    .replaceAll("\u00A0", " ")
    .replaceAll("\r\n", "\n");
}

function publishValue() {
  const value = editorText();
  if (value !== props.modelValue) emit("update:modelValue", value);
}

function resetMentionPicker() {
  mentionRange.value = undefined;
  mentionQuery.value = "";
  activeMentionIndex.value = 0;
}

function caretTextPosition() {
  const root = editor.value;
  const selection = window.getSelection();
  if (!root || !selection?.rangeCount || !selection.isCollapsed) return;
  const selectionRange = selection.getRangeAt(0);
  if (!root.contains(selectionRange.endContainer)) return;

  let node: Node = selectionRange.endContainer;
  let offset = selectionRange.endOffset;
  if (node.nodeType === Node.ELEMENT_NODE) {
    const previous = node.childNodes[Math.min(offset, node.childNodes.length) - 1];
    if (!previous || previous.nodeType !== Node.TEXT_NODE) return;
    node = previous;
    offset = previous.textContent?.length ?? 0;
  }
  if (node.nodeType !== Node.TEXT_NODE) return;
  return { node, offset };
}

function updateMentionPicker() {
  const position = caretTextPosition();
  if (!position) return resetMentionPicker();
  const source = position.node.textContent ?? "";
  const beforeCursor = source.slice(0, position.offset);
  const match = beforeCursor.match(/@(?:(?:角色|场景|道具)[:：])?([\p{L}\p{N}_-]*)$/u);
  if (!match) return resetMentionPicker();

  const range = document.createRange();
  range.setStart(position.node, position.offset - match[0].length);
  range.setEnd(position.node, position.offset);
  mentionRange.value = range;
  mentionQuery.value = match[1] ?? "";
  activeMentionIndex.value = 0;
}

function chooseMention(asset: ScriptMentionAsset) {
  const range = mentionRange.value;
  if (!range) return;
  const duplicateName = props.assets.filter(item => item.name === asset.name).length > 1;
  const type = mentionType(asset.assetType);
  const prefix = duplicateName && type !== "unknown"
    ? `${scriptMentionTypeLabel(type) === "人物" ? "角色" : scriptMentionTypeLabel(type)}:`
    : "";
  const token = `@${prefix}${asset.name}`;
  const mention = createMentionElement({
    text: token,
    mention: true,
    name: asset.name,
    type,
    assetId: asset.id
  });
  const trailingSpace = document.createTextNode(" ");
  const fragment = document.createDocumentFragment();
  fragment.append(mention, trailingSpace);
  range.deleteContents();
  range.insertNode(fragment);

  const selection = window.getSelection();
  range.setStartAfter(trailingSpace);
  range.collapse(true);
  selection?.removeAllRanges();
  selection?.addRange(range);
  editor.value?.focus({ preventScroll: true });
  resetMentionPicker();
  publishValue();
}

function handleInput() {
  publishValue();
  if (!isComposing.value) updateMentionPicker();
}

function handleCompositionStart() {
  isComposing.value = true;
}

function handleCompositionEnd() {
  isComposing.value = false;
  handleInput();
}

function handleKeydown(event: KeyboardEvent) {
  if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
    event.preventDefault();
    emit("submit");
    return;
  }
  if (event.key === "Escape") {
    event.preventDefault();
    if (mentionRange.value) {
      // 这一下 Esc 是用来关资产菜单的，不能再冒泡上去把整个弹窗也关了
      event.stopPropagation();
      resetMentionPicker();
      return;
    }
    emit("dismiss");
    return;
  }
  if (!mentionSuggestions.value.length) return;
  if (event.key === "ArrowDown" || event.key === "ArrowUp") {
    event.preventDefault();
    const offset = event.key === "ArrowDown" ? 1 : -1;
    activeMentionIndex.value = (activeMentionIndex.value + offset + mentionSuggestions.value.length) % mentionSuggestions.value.length;
    return;
  }
  if (event.key === "Enter" || event.key === "Tab") {
    event.preventDefault();
    const target = mentionSuggestions.value[activeMentionIndex.value];
    if (target) chooseMention(target);
  }
}

function handleKeyup(event: KeyboardEvent) {
  if (mentionSuggestions.value.length && ["ArrowDown", "ArrowUp", "Enter", "Tab"].includes(event.key)) return;
  updateMentionPicker();
}

function handlePaste(event: ClipboardEvent) {
  const text = event.clipboardData?.getData("text/plain");
  const selection = window.getSelection();
  if (text == null || !selection?.rangeCount || !editor.value?.contains(selection.anchorNode)) return;
  event.preventDefault();
  const range = selection.getRangeAt(0);
  const textNode = document.createTextNode(text);
  range.deleteContents();
  range.insertNode(textNode);
  range.setStartAfter(textNode);
  range.collapse(true);
  selection.removeAllRanges();
  selection.addRange(range);
  handleInput();
}

function handleMousedown(event: MouseEvent) {
  const target = event.target as HTMLElement | null;
  if (target?.closest("[data-script-mention]")) event.preventDefault();
}

function focusEnd() {
  const root = editor.value;
  if (!root) return;
  renderEditor(props.modelValue);
  root.focus({ preventScroll: true });
  const range = document.createRange();
  range.selectNodeContents(root);
  range.collapse(false);
  const selection = window.getSelection();
  selection?.removeAllRanges();
  selection?.addRange(range);
  root.scrollTop = root.scrollHeight;
}

watch(() => props.modelValue, (value) => {
  if (document.activeElement !== editor.value && editorText() !== value) renderEditor(value);
});

watch(() => props.assets, () => {
  if (document.activeElement !== editor.value) renderEditor(props.modelValue);
}, { deep: true });

onMounted(() => renderEditor(props.modelValue));

defineExpose({ focusEnd });
</script>

<template>
  <section class="script-description-rich-editor">
    <div
      ref="editor"
      class="script-description-editor-input"
      :class="{ empty: isEmpty }"
      contenteditable="true"
      role="textbox"
      aria-multiline="true"
      :aria-label="label"
      :data-placeholder="placeholder"
      spellcheck="false"
      @input="handleInput"
      @click="updateMentionPicker"
      @keyup="handleKeyup"
      @keydown="handleKeydown"
      @mousedown="handleMousedown"
      @paste="handlePaste"
      @compositionstart="handleCompositionStart"
      @compositionend="handleCompositionEnd"
    />
    <div v-if="mentionSuggestions.length" class="script-description-mention-list">
      <button
        v-for="(asset, index) in mentionSuggestions"
        :key="`${asset.id ?? 'draft'}-${asset.assetType}-${asset.name}`"
        type="button"
        :class="[{ active: activeMentionIndex === index }, mentionClass(asset.assetType)]"
        @mouseenter="activeMentionIndex = index"
        @mousedown.prevent="chooseMention(asset)"
      >
        <span />
        <strong>@{{ asset.name }}</strong>
        <small>{{ scriptMentionTypeLabel(mentionType(asset.assetType)) }}</small>
      </button>
    </div>
  </section>
</template>

<style scoped>
.script-description-rich-editor { position: relative; overflow: hidden; border-radius: 7px; background: #252526; }
.script-description-editor-input { width: 100%; height: 100%; box-sizing: border-box; overflow: auto; outline: 0; padding: 12px 14px; color: #eeeeef; caret-color: #f4f4f5; cursor: text; font: 14px/1.75 inherit; font-variant-ligatures: none; letter-spacing: 0; tab-size: 4; white-space: pre-wrap; overflow-wrap: anywhere; word-break: break-word; scrollbar-width: none; }
.script-description-editor-input::-webkit-scrollbar { width: 0; height: 0; }
.script-description-editor-input.empty::before { content: attr(data-placeholder); color: #77777c; pointer-events: none; }
/* @实体的高亮块是 createMentionElement 用 document.createElement 建的，拿不到 scoped
   的 data-v 属性，写成普通 scoped 选择器编译后会变成 .script-description-mention[data-v-x]，
   永远匹配不上。用 :deep() 把作用域限定在编辑器容器上、放开内部元素的属性要求。 */
.script-description-editor-input :deep(.script-description-mention) { display: inline; border-radius: 3px; padding: 0 2px; color: #b89be8; background: rgba(184,155,232,.1); cursor: pointer; user-select: none; }
.script-description-editor-input :deep(.script-description-mention.mention-character) { color: #4fd0e4; background: rgba(79,208,228,.1); }
.script-description-editor-input :deep(.script-description-mention.mention-scene) { color: #7bcf96; background: rgba(123,207,150,.1); }
.script-description-editor-input :deep(.script-description-mention.mention-prop) { color: #e3b45f; background: rgba(227,180,95,.1); }
.script-description-mention-list { width: min(380px, calc(100% - 28px)); max-height: 232px; position: absolute; z-index: 3; left: 14px; bottom: 12px; display: grid; overflow: auto; border: 1px solid #454549; border-radius: 7px; padding: 4px; background: #1d1d1f; box-shadow: 0 14px 30px rgba(0,0,0,.5); }
.script-description-mention-list button { min-height: 34px; display: grid; grid-template-columns: 8px minmax(0,1fr) auto; align-items: center; gap: 9px; border: 0; border-radius: 4px; padding: 0 10px; color: #cfcfd2; background: transparent; text-align: left; }
.script-description-mention-list button.active { background: #303033; }
.script-description-mention-list button > span { width: 7px; height: 7px; border-radius: 50%; background: #b89be8; }
.script-description-mention-list button.mention-character > span { background: #4fd0e4; }
.script-description-mention-list button.mention-scene > span { background: #7bcf96; }
.script-description-mention-list button.mention-prop > span { background: #e3b45f; }
.script-description-mention-list strong { overflow: hidden; font-size: 12px; font-weight: 520; text-overflow: ellipsis; white-space: nowrap; }
.script-description-mention-list small { color: #7d7d82; font-size: 10px; }
</style>
