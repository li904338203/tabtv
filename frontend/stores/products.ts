export type ProductStatus = "enabled" | "preview" | "disabled";

export type ProductSummary = {
  code: string;
  name: string;
  description: string;
  icon: string;
  entryPath: string;
  status: ProductStatus;
  launchReady: boolean;
  sortOrder: number;
  capabilities: string[];
};

const fallbackProducts: ProductSummary[] = [
  {
    code: "video",
    // 侧栏与「产品已关闭」提示里的名字（2026-11-01 按用户要求由「AI 视频」改为「画布创造」；code / 路由不变）
    name: "画布创造",
    description: "基于画布完成脚本、分镜、图片与视频生成",
    icon: "video",
    entryPath: "/video",
    status: "enabled",
    launchReady: true,
    sortOrder: 10,
    capabilities: ["canvas", "script", "storyboard", "image_generation", "video_generation"]
  },
];

export const useProductsStore = defineStore("products", () => {
  const products = ref<ProductSummary[]>(fallbackProducts);
  const loading = ref(false);
  const loaded = ref(false);

  const visibleProducts = computed(() => products.value
    .filter(product => product.status !== "disabled")
    .sort((left, right) => left.sortOrder - right.sortOrder));

  /**
   * ⚠️ **新系统不请求产品目录**。老系统调 `/platform/products` 支持三产品的
   * 开关与排序；新系统是**单产品**（写作与数字人已排除，FULL-01），
   * 而 AD-04 产品模块管理**明确不做** —— 单产品系统的"产品开关"唯一能做的
   * 操作就是把唯一的产品关掉。
   *
   * 保留这个函数是为了不动 12 处调用点；它现在只是把 loaded 置真。
   */
  async function load() {
    loaded.value = true
  }

  function find(code: string) {
    return products.value.find(product => product.code === code);
  }

  return { products, visibleProducts, loading, loaded, load, find };
});
