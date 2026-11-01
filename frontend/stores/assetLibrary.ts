import { defineStore } from "pinia";

/**
 * 我的资产的跨组件信号（画布右键菜单保存 / 评级之后，左侧「资产」页签要刷新）。
 * 列表本身由资产面板自己取（按需加载的组件里），这里只放一个版本号，免得画布页首屏包带上整套资产逻辑。
 */
export const useAssetLibraryStore = defineStore("assetLibrary", () => {
  const version = ref(0);
  function touch() {
    version.value += 1;
  }
  return { version, touch };
});
