<script setup lang="ts">
/**
 * 第三方许可页（DRT-09，DR-N-15、DR-AC-48；智能剪辑的字体见 SCT-13）：列出三方依赖与素材的许可与署名。
 * 清单的唯一来源是 `director/licenses.json`，由 `utils/directorLicenses.test.ts` 核对 package.json 与 node_modules。
 */
import PageCrumb from "~/components/common/PageCrumb.vue";
import licenses from "~/director/licenses.json";

definePageMeta({ layout: "platform" });
useHead({ title: "第三方许可" });

type PackageEntry = { name: string; version: string; license: string; url: string; note: string };
type AssetEntry = { path: string; license: string; source: string; attribution: string };

const packages = licenses.packages as PackageEntry[];
const assets = licenses.assets as AssetEntry[];
</script>

<template>
  <div class="licenses-page">
    <PageCrumb fallback="/">第三方许可</PageCrumb>
    <h1 class="licenses-title">第三方许可</h1>
    <p class="licenses-lead">
      3D 导演台与智能剪辑用到的开源依赖与素材。依赖与随网页下发的素材只引入 MIT、Apache-2.0、MPL-2.0 许可的内容；
      服务端排版字幕与文字用的字体是 SIL OFL-1.1，不随网页下发。
    </p>

    <section class="licenses-section">
      <h2>依赖</h2>
      <table class="licenses-table">
        <thead>
          <tr><th>名称</th><th>版本</th><th>许可</th><th>用途</th></tr>
        </thead>
        <tbody>
          <tr v-for="p in packages" :key="p.name">
            <td><a :href="p.url" target="_blank" rel="noopener noreferrer">{{ p.name }}</a></td>
            <td>{{ p.version }}</td>
            <td>{{ p.license }}</td>
            <td>{{ p.note }}</td>
          </tr>
        </tbody>
      </table>
    </section>

    <section class="licenses-section">
      <h2>素材</h2>
      <p v-if="assets.length === 0" class="licenses-empty">暂无。</p>
      <table v-else class="licenses-table">
        <thead>
          <tr><th>文件</th><th>许可</th><th>来源</th><th>署名</th></tr>
        </thead>
        <tbody>
          <tr v-for="a in assets" :key="a.path">
            <td>{{ a.path }}</td>
            <td>{{ a.license }}</td>
            <td><a :href="a.source" target="_blank" rel="noopener noreferrer">{{ a.source }}</a></td>
            <td>{{ a.attribution }}</td>
          </tr>
        </tbody>
      </table>
    </section>
  </div>
</template>

<style scoped>
.licenses-page {
  max-width: var(--portal-page-max);
  margin: 0 auto;
  padding: 16px var(--portal-page-gutter) 48px;
  color: var(--portal-text);
}
.licenses-title {
  margin: 12px 0 4px;
  font-size: 22px;
}
.licenses-lead,
.licenses-empty {
  color: var(--portal-muted);
}
.licenses-section {
  margin-top: 24px;
}
.licenses-section h2 {
  font-size: 16px;
  margin-bottom: 8px;
}
.licenses-table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
.licenses-table th,
.licenses-table td {
  padding: 8px 10px;
  border-bottom: 1px solid var(--portal-line);
  text-align: left;
}
.licenses-table th {
  color: var(--portal-muted);
  font-weight: 500;
}
.licenses-table a {
  color: var(--portal-cyan);
}
</style>
