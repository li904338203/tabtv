<script setup lang="ts">
/**
 * 设置 → 关于（TT-F-37）：版本、检查更新与更新日志（TT-T-31）、第三方许可证。
 *
 * ⚠️ macOS 与 Linux .deb 只给「打开下载页」：未签名应用不能自己替换自己，.deb 没有权限改系统目录（TT-F-40）。
 */
import { ElButton, ElMessage, ElMessageBox } from "element-plus";
import { hostApp, openExternal } from "~/utils/desktopBridge";
import { renderMarkdown } from "~/utils/markdown";
import { updateAction, updateSummary } from "~/utils/updateStatus";

const version = ref("");
const update = useUpdateStore();
const installing = ref(false);
const hasHost = computed(() => Boolean(hostApp()?.CheckUpdate));
const action = computed(() => updateAction(update.info));
const notesHtml = computed(() => (update.info?.available && update.info.notes ? renderMarkdown(update.info.notes) : ""));

onMounted(async () => {
  version.value = (await useApi()<{ version?: string }>("/health").catch(() => ({ version: "" }))).version ?? "";
});

async function install() {
  const host = hostApp();
  if (!host?.InstallUpdate) return;
  try {
    await ElMessageBox.confirm("下载并校验新版本后应用会退出并完成安装，进行中的任务会在下次启动时继续。", "安装更新",
      { confirmButtonText: "下载并安装", cancelButtonText: "取消", type: "info" });
  } catch {
    return;
  }
  installing.value = true;
  try {
    const err = await host.InstallUpdate();
    if (err) ElMessage.error(err);
    else ElMessage.success("正在安装新版本，应用即将重启");
  } finally {
    installing.value = false;
  }
}
</script>

<template>
  <div class="settings-panel">
    <div class="section-head"><div><h2>关于 tabtv</h2><p>开源的 AI 画布与视频创作工具 · Apache-2.0</p></div></div>
    <dl class="about-list">
      <dt>版本</dt>
      <dd>
        {{ version || "—" }}
        <template v-if="update.info?.commit && update.info.commit !== 'unknown'"> · {{ update.info.commit.slice(0, 7) }}</template>
      </dd>
      <dt>更新</dt>
      <dd>
        <template v-if="hasHost">
          <div class="about-update">
            <ElButton size="small" :loading="update.checking" @click="update.check()">检查更新</ElButton>
            <ElButton v-if="action === 'install'" size="small" type="primary" :loading="installing" @click="install">下载并安装</ElButton>
            <ElButton v-else-if="action === 'open-page'" size="small" type="primary" @click="openExternal(update.info!.pageUrl)">打开下载页</ElButton>
            <span v-if="update.info" class="about-update-text">{{ updateSummary(update.info) }}</span>
          </div>
          <!-- eslint-disable-next-line vue/no-v-html -- renderMarkdown 已经过 DOMPurify -->
          <div v-if="notesHtml" class="about-notes" v-html="notesHtml" />
        </template>
        <template v-else>在桌面应用里可以检查更新。</template>
      </dd>
      <dt>数据</dt>
      <dd>全部保存在本机数据目录；除调用你配置的模型上游与检查更新外，应用不访问任何网络地址。</dd>
      <dt>第三方许可证</dt>
      <dd><NuxtLink to="/licenses" class="text-link">查看随应用附带的第三方组件与许可证</NuxtLink></dd>
    </dl>
  </div>
</template>

<style scoped>
.about-list { display: grid; grid-template-columns: 120px 1fr; gap: 12px 16px; margin-top: 16px; font-size: 13px; }
.about-list dt { color: var(--portal-faint); }
.about-list dd { margin: 0; color: var(--portal-text-soft); }
.about-update { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
.about-update-text { color: var(--portal-muted); }
.about-notes { margin-top: 12px; padding: 12px 14px; border: 1px solid var(--portal-line); border-radius: 10px; background: var(--portal-surface); line-height: 1.6; }
</style>
