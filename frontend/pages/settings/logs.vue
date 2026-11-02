<script setup lang="ts">
/**
 * 设置 → 调用日志（TT-F-36）：发给模型上游的请求（已脱敏），排查「为什么生成失败」用。
 *
 * ⚠️ 成败看 errorType 不看 HTTP 状态码：上游的业务错误常藏在 HTTP 200 里，超时 / 断连根本没有状态码。
 * ⚠️ 只给已脱敏的请求体：Key 在落库前就打码了，这里不会出现。
 */
type LogItem = {
  id: string; provider: string; model: string; operation: string; url: string; httpStatus: number;
  errorType: string; durationMs: number; requestBody: string; responseBody: string; createdAt: string; generationId?: string;
};

const items = ref<LogItem[]>([]);
const cursor = ref("");
const loading = ref(false);
const failedOnly = ref(false);
const model = ref("");
const detail = ref<LogItem | null>(null);
const loadError = ref("");

const ERROR_LABELS: Record<string, string> = {
  none: "成功", http: "上游返回错误", timeout: "超时", connect: "连不上", dns: "域名解析失败", stream: "流式中断"
};

async function load(reset = true) {
  loading.value = true;
  loadError.value = "";
  try {
    const query: Record<string, string> = { limit: "30" };
    if (failedOnly.value) query.failed = "true";
    if (model.value) query.model = model.value;
    if (!reset && cursor.value) query.cursor = cursor.value;
    const resp = await useApi()<{ items: LogItem[]; nextCursor: string }>("/admin/request-logs/provider", { query });
    items.value = reset ? resp.items : [...items.value, ...resp.items];
    cursor.value = resp.nextCursor;
  } catch (e) {
    loadError.value = (e as { message?: string }).message || "日志加载失败";
  } finally {
    loading.value = false;
  }
}

function time(s: string) {
  return new Date(s).toLocaleString("zh-CN", { hour12: false });
}

watch([failedOnly, model], () => { void load(); });
onMounted(() => { void load(); });
</script>

<template>
  <div class="settings-panel">
    <div class="section-head">
      <div><h2>调用日志</h2><p>发给模型上游的请求，按时间倒序 · 保留期在「常规」里设置</p></div>
      <div class="log-filters">
        <el-input v-model.lazy="model" placeholder="按模型编码筛选" clearable style="width: 200px" />
        <el-checkbox v-model="failedOnly">只看失败</el-checkbox>
      </div>
    </div>
    <p v-if="loadError" class="settings-error">{{ loadError }}</p>
    <el-table :data="items" size="small" class="log-table" @row-click="(row: LogItem) => (detail = row)">
      <el-table-column label="时间" width="170"><template #default="{ row }">{{ time(row.createdAt) }}</template></el-table-column>
      <el-table-column prop="model" label="模型" min-width="160" />
      <el-table-column prop="operation" label="操作" width="130" />
      <el-table-column label="结果" width="130">
        <template #default="{ row }">
          <span :class="{ bad: row.errorType !== 'none' }">{{ ERROR_LABELS[row.errorType] ?? row.errorType }}</span>
        </template>
      </el-table-column>
      <el-table-column prop="httpStatus" label="状态码" width="80" />
      <el-table-column label="耗时" width="90"><template #default="{ row }">{{ (row.durationMs / 1000).toFixed(1) }}s</template></el-table-column>
    </el-table>
    <div class="log-more">
      <el-button v-if="cursor" :loading="loading" size="small" @click="load(false)">加载更多</el-button>
      <span v-else-if="!loading && !items.length" class="portal-empty">还没有调用记录</span>
    </div>

    <el-drawer :model-value="!!detail" size="50%" title="请求详情" @close="detail = null">
      <template v-if="detail">
        <p class="log-url"><code>{{ detail.url }}</code></p>
        <h4>请求体（已脱敏）</h4>
        <pre class="log-body">{{ detail.requestBody || "（无）" }}</pre>
        <h4>响应 / 错误</h4>
        <pre class="log-body">{{ detail.responseBody || "（无）" }}</pre>
      </template>
    </el-drawer>
  </div>
</template>

<style scoped>
.log-filters { display: flex; align-items: center; gap: 12px; }
.log-table { margin-top: 12px; cursor: pointer; }
.log-table .bad { color: var(--portal-danger); }
.log-more { display: flex; justify-content: center; margin-top: 12px; }
.log-url code { word-break: break-all; font-size: 12px; }
.log-body { max-height: 40vh; overflow: auto; padding: 10px; border-radius: 8px; background: var(--portal-code-bg); font-size: 12px; white-space: pre-wrap; word-break: break-all; }
.settings-error { color: var(--portal-danger); font-size: 13px; }
</style>
