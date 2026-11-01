<script setup lang="ts">
/**
 * 客户端接入指南（MCPT-11F）：按客户端分页签展示配置片段，内容来自 `GET /me/mcp/client-snippets`。
 *
 * ⚠️ `plaintext` 只在「刚建完密钥」的弹层里传进来：那时把占位符换成明文供复制，
 *    结果只是一个 computed（组件内存），弹层关闭即随组件一起销毁。页面上常驻的这一份**永远是占位符**。
 */
import { Copy } from "@lucide/vue";
import { ElMessage } from "element-plus";
import {
  renderSnippets,
  snippetFormatLabel,
  snippetsEmptyText,
  snippetsUrlNotice,
  type McpClientSnippets,
} from "~/utils/mcpSnippets";

const props = defineProps<{
  snippets: McpClientSnippets | null;
  plaintext?: string | null;
  /** 加载失败的文案（由页面按四态给）；为空表示没有失败。 */
  error?: string | null;
  retryable?: boolean;
}>();
const emit = defineEmits<{ retry: [] }>();

const items = computed(() => renderSnippets(props.snippets, props.plaintext));
const emptyText = computed(() => snippetsEmptyText(props.snippets));
const urlNotice = computed(() => snippetsUrlNotice(props.snippets));
const selected = ref("");
const current = computed(() => items.value.find((i) => i.client === selected.value) ?? items.value[0] ?? null);
const codeEl = ref<HTMLElement | null>(null);

async function copy() {
  if (!current.value) return;
  try {
    await navigator.clipboard.writeText(current.value.content);
    ElMessage.success("已复制");
  } catch {
    // ⚠️ 复制失败时让用户手动选中，不退回「下载为文件」（文件会落盘，明文就离开了内存）。
    if (codeEl.value) window.getSelection()?.selectAllChildren(codeEl.value);
    ElMessage.warning("复制没有成功，已选中内容，请手动复制");
  }
}
</script>

<template>
  <section class="mcp-snippets">
    <div v-if="error" class="portal-notice error">
      {{ error }}
      <button v-if="retryable" type="button" class="portal-link-button" @click="emit('retry')">重试</button>
    </div>
    <div v-else-if="emptyText" class="portal-empty">{{ emptyText }}</div>
    <template v-else-if="current">
      <p v-if="urlNotice" class="portal-notice">{{ urlNotice }}</p>
      <nav class="mcp-snippet-tabs" aria-label="选择客户端">
        <button
          v-for="item in items"
          :key="item.client"
          type="button"
          :class="{ active: item.client === current.client }"
          :aria-pressed="item.client === current.client"
          @click="selected = item.client"
        >{{ item.label }}</button>
      </nav>
      <div class="mcp-snippet-body">
        <header>
          <small>{{ snippetFormatLabel(current.format) }} · {{ current.location }}</small>
          <button type="button" class="secondary-command" @click="copy"><Copy :size="13" />复制</button>
        </header>
        <pre ref="codeEl"><code>{{ current.content }}</code></pre>
        <p v-if="current.note" class="mcp-snippet-note">{{ current.note }}</p>
      </div>
      <p class="mcp-snippet-note">
        <template v-if="plaintext">片段里已填入刚创建的密钥，关闭弹层后不再显示。</template>
        <template v-else>片段里的 {{ snippets?.keyPlaceholder }} 换成你的访问密钥（创建密钥时可直接复制已填好的片段）。</template>
        各客户端的配置格式以兼容矩阵的实测结果为准。
      </p>
    </template>
  </section>
</template>
