<script setup lang="ts">
/**
 * 设置 → 关于（TT-F-37）：版本与第三方许可证。检查更新在 TT-T-31 接入（这里不放占位按钮）。
 */
const version = ref("");
onMounted(async () => {
  version.value = (await useApi()<{ version?: string }>("/health").catch(() => ({ version: "" }))).version ?? "";
});
</script>

<template>
  <div class="settings-panel">
    <div class="section-head"><div><h2>关于 tabtv</h2><p>开源的 AI 画布与视频创作工具 · Apache-2.0</p></div></div>
    <dl class="about-list">
      <dt>版本</dt>
      <dd>{{ version || "—" }}</dd>
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
</style>
