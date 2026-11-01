import { extensionFor, safeFilename, saveBlob } from "~/utils/download";

/**
 * 产物预览的状态 —— 「创作资产」与画布图片节点的双击预览**共用这一份**。
 *
 * 负责：列出一次生成的**全部**产物（GO-40）、按需取回完整字节、逐项切换、下载、释放内存。
 * 浮层的标记在 `MediaPreviewDialog`，这里只管状态。
 *
 * ⚠️ 这段逻辑原先写在 `pages/works.vue` 里。画布节点要同样的预览时抄一份的话，
 *    防串台、释放、下载命名这几处早晚会分叉 —— 修一处忘一处，而两处各自看都没问题。
 *
 * 取的是**完整**产物（不是列表缩略图）：受保护端点，要 fetch（带鉴权头与工作区头）成 blob
 * 再转 object URL 交给 <img>/<video>/<audio> —— 直链一律 401/400。
 * ⚠️ 下载直接用预览已经取回来的那份字节，不再请求第二遍（一个视频就是几十 MB）。
 */
export type PreviewOutput = { index?: number; type?: string; mediaId?: string | null; url?: string | null };
type Loaded = { url: string; blob: Blob };

export type OutputPreviewRequest = {
  /** 生成任务 ID：按它列出全部产物。没有（或列不出来）时只预览 `fallback` 那一项。 */
  generationId?: string;
  /** 兜底的那一项，通常就是节点上正显示着的那张。 */
  fallback?: PreviewOutput;
  /** 兜底那一项已经在内存里的字节：有就不再请求一次（画布节点上显示的原图）。 */
  seed?: Blob;
  /** 只看这些类型（创作资产不收文本；画布图片节点只看图片）。 */
  types?: readonly string[];
  /** 列表还没回来时浮层上显示的类型。 */
  type?: string;
  title: string;
  subtitle?: string;
  /** 下载文件名（不含扩展名）；多项时自动加序号。 */
  filename: string;
};

export function useOutputPreview() {
  const { fetchMedia } = useAuthenticatedMedia();
  const request = ref<OutputPreviewRequest | null>(null);
  const outputs = ref<PreviewOutput[]>([]);
  const index = ref(0);
  const loading = ref(false);
  const error = ref("");
  /** 已取回的产物，按序号缓存；关闭时统一释放（漏掉 revoke 的话每看一张就在内存里留一张原图）。 */
  const loaded = reactive(new Map<number, Loaded>());
  /** 防串台：快速切换时，先发出去的慢请求可能后回来，不能让它盖掉当前这张。 */
  let token = 0;

  const opened = computed(() => request.value !== null);
  const current = computed(() => outputs.value[index.value]);
  const currentSrc = computed(() => loaded.get(index.value)?.url);
  const type = computed(() => current.value?.type ?? request.value?.type);

  async function listOutputs(options: OutputPreviewRequest): Promise<PreviewOutput[]> {
    let list: PreviewOutput[] = [];
    if (options.generationId) {
      try {
        list = await useApi()<PreviewOutput[]>(`/generations/${options.generationId}/outputs`);
      } catch (cause) {
        // 列不出来但手上有兜底的那一项：照样能看，不为一个列表请求挡住整个预览
        if (!options.fallback?.url) throw cause;
      }
    }
    list = list.filter(output => output.url && output.type !== "text"
      && (!options.types || options.types.includes(output.type ?? "")));
    if (!list.length && options.fallback?.url) list = [options.fallback];
    return list;
  }

  async function open(options: OutputPreviewRequest) {
    close();
    request.value = options;
    const mine = ++token;
    loading.value = true;
    try {
      const list = await listOutputs(options);
      if (mine !== token) return;
      outputs.value = list;
      if (!list.length) throw new Error("没有可预览的产物");
      // 已在内存里的那张只认**同一个地址**的那一项，免得把 A 的字节挂到 B 上
      const seeded = list.findIndex(output => output.url === options.fallback?.url);
      if (options.seed && seeded >= 0) {
        loaded.set(seeded, { url: URL.createObjectURL(options.seed), blob: options.seed });
      }
      await show(Math.max(0, seeded));
    } catch (cause) {
      if (mine !== token) return;
      error.value = cause instanceof Error ? cause.message : "预览加载失败";
      loading.value = false;
    }
  }

  async function show(target: number) {
    const output = outputs.value[target];
    if (!output?.url) return;
    index.value = target;
    error.value = "";
    if (loaded.has(target)) {
      loading.value = false;
      return;
    }
    const mine = ++token;
    loading.value = true;
    try {
      const response = await fetchMedia(output.url);
      if (!response.ok) throw new Error(`预览加载失败（HTTP ${response.status}）`);
      const blob = await response.blob();
      if (mine !== token || !request.value) return;
      loaded.set(target, { url: URL.createObjectURL(blob), blob });
    } catch (cause) {
      if (mine !== token) return;
      error.value = cause instanceof Error ? cause.message : "预览加载失败";
    } finally {
      if (mine === token) loading.value = false;
    }
  }

  function step(delta: number) {
    const total = outputs.value.length;
    if (total < 2) return;
    void show((index.value + delta + total) % total);
  }

  function retry() {
    if (!request.value) return;
    if (outputs.value.length) void show(index.value);
    else void open(request.value);
  }

  function close() {
    token++;
    for (const entry of loaded.values()) URL.revokeObjectURL(entry.url);
    loaded.clear();
    request.value = null;
    outputs.value = [];
    index.value = 0;
    loading.value = false;
    error.value = "";
  }

  /** 下载预览中的那一项：直接用已取回的字节，文件名带扩展名。 */
  function download() {
    const entry = loaded.get(index.value);
    if (!request.value || !entry) return;
    // 多产物时带上序号，否则同一次生成的几张会下成同一个名字、互相覆盖
    const suffix = outputs.value.length > 1 ? `-${index.value + 1}` : "";
    saveBlob(entry.blob, `${safeFilename(`${request.value.filename}${suffix}`)}.${extensionFor(entry.blob.type)}`);
  }

  onBeforeUnmount(close);

  return { request, opened, outputs, index, loading, error, current, currentSrc, type, open, show, step, retry, close, download };
}
