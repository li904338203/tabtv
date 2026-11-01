<script setup lang="ts">
/**
 * 创建成功后的明文弹层（MCPT-11F，MCP-F-03）。
 *
 * ⚠️ 明文**只活在这个弹层里**：由页面以 prop 传入（页面也只在一个局部 ref 里持有它），
 *    关闭即由页面清空；不进 store、不进 localStorage / sessionStorage、不打日志、不放 URL。
 * ⚠️ 只能用按钮关闭（不响应点背景 / Esc）：误关一次明文就再也取不回，只能撤销重建。
 */
import { Copy, TriangleAlert } from "@lucide/vue";
import { ElMessage } from "element-plus";
import McpClientSnippets from "~/components/account/McpClientSnippets.vue";
import { keyPrefixText, type McpAccessKey } from "~/utils/mcpKeys";
import type { McpClientSnippets as Snippets } from "~/utils/mcpSnippets";

const props = defineProps<{
  plaintext: string;
  accessKey: McpAccessKey;
  snippets: Snippets | null;
}>();
const emit = defineEmits<{ close: [] }>();
const secretEl = ref<HTMLElement | null>(null);

async function copy() {
  try {
    await navigator.clipboard.writeText(props.plaintext);
    ElMessage.success("已复制");
  } catch {
    if (secretEl.value) window.getSelection()?.selectAllChildren(secretEl.value);
    ElMessage.warning("复制没有成功，已选中密钥，请手动复制");
  }
}

// 弹层开着时独占键盘（同 PreviewDialog）：只拦传播，不在 Esc 上关闭。
function onKeydown(event: KeyboardEvent) {
  event.stopPropagation();
}
onMounted(() => window.addEventListener("keydown", onKeydown, true));
onBeforeUnmount(() => window.removeEventListener("keydown", onKeydown, true));
</script>

<template>
  <Teleport to="body">
    <div class="portal-overlay">
      <section class="mcp-dialog" role="dialog" aria-modal="true" aria-label="访问密钥已创建">
        <header class="mcp-dialog-head">
          <div><h2>访问密钥已创建</h2><p>{{ accessKey.clientName }} · {{ keyPrefixText(accessKey.prefix) }}</p></div>
        </header>
        <p class="mcp-warning"><TriangleAlert :size="15" />只显示这一次，关闭后无法再查看。请现在复制并保存到客户端配置里。</p>
        <div class="mcp-secret">
          <code ref="secretEl">{{ plaintext }}</code>
          <button type="button" class="product-primary-command" @click="copy"><Copy :size="14" />复制密钥</button>
        </div>
        <h3 class="mcp-dialog-subhead">直接复制已填好密钥的配置</h3>
        <McpClientSnippets :snippets="snippets" :plaintext="plaintext" />
        <footer class="mcp-dialog-foot">
          <button type="button" class="product-primary-command" @click="emit('close')">我已保存，关闭</button>
        </footer>
      </section>
    </div>
  </Teleport>
</template>
