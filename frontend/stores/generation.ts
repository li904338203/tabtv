import type { GenerationInput } from '~/utils/canvas';
import { acceptInputsOf, roleCountsOf } from '~/utils/canvas';
import { decodeGenerationOutput, generationOutputPlan } from "~/utils/generationOutput";
import { watch as watchSource } from "vue";
import { authorizedMediaRequest } from "~/utils/authorizedMedia";
import { isTerminalStatus, pollExhausted } from "~/utils/asyncState";
import { eventTarget, workspaceTopic } from "~/utils/realtime";
import { creditBalanceOf } from "~/utils/accountPanel";

/**
 * 受理请求 —— **形状由新后端决定**（`generation/transport.create`）。
 *
 * ⚠️ 不再用老契约的 `GenerateReq`：那描述的是老后端
 * （`model` / `inputs[].type` / `inputs[].nodeId`），与新后端逐字不同。
 * 照老类型写，TS 会告诉你 `modelCode` 不存在 —— 而真正错的是那个类型。
 */
type GenerateRequest = {
  projectId: string
  nodeId: string
  operation: string
  modelCode: string
  params?: unknown
  inputs?: GenerationInput[]
  count?: number
  durationSec?: number
  text?: string
  retryOfId?: string
};
/** 受理响应。⚠️ 新后端返回 `id`（老后端 `generationId`）。 */
type GenerateResponse = {
  id?: string
  generationId?: string
  status?: string
  creditsCost?: number
  deadlineAt?: string
};
/**
 * 生成详情。⚠️ 与老后端的差异：`id`（非 `generationId`）、`errorMessage`（非 `error`）。
 *
 * ⚠️ **详情里根本没有 `outputs`** —— 产物在**另一个端点**
 * （`GET /generations/{id}/outputs`）。老后端是把 outputs 内嵌在详情里的，
 * 所以老前端读 `response.outputs[0]`，而它在新后端恒为 undefined ——
 * **任务成功、积分扣了、节点上什么都不显示，且没有任何报错**。
 */
type GenerationResponse = {
  id?: string
  generationId?: string
  projectId?: string
  nodeId?: string
  status?: string
  progress?: number
  resultType?: string
  creditsCost?: number
  errorMessage?: string | null
  error?: string
  deadlineAt?: string
  createdAt?: string
  updatedAt?: string
};

/**
 * 一个产物（`GET /generations/{id}/outputs` 的元素）。
 *
 * ⚠️ **文本产物没有 `url`**：正文直接在 `text` 字段里。
 *    只按 url 取的话，文本节点这一整类**永远拿不到结果** ——
 *    而图片/视频恰好有 url，所以看起来"只有文本节点坏了"。
 * ⚠️ `url` 指向**媒体域**的受保护端点（`/api/v1/media/{id}`），
 *    不是 `/generations/{id}/outputs/{i}`（那个回的是元信息 JSON，
 *    拿去当图片渲染就是一张裂图）。
 */
type GenerationOutput = {
  index?: number
  type?: string
  mediaId?: string | null
  text?: string | null
  url?: string | null
};
/** 报价响应。⚠️ 新后端的字段是 `credits`（老后端 `creditsCost`）。 */
type EstimateResponse = {
  credits?: number
  creditsCost?: number
  unit?: string
  estimatedSeconds?: number
  deadlineSeconds?: number
  outputNodeType?: string
};

export const useGenerationStore = defineStore("generation", () => {
  const canvas = useCanvasStore();
  const auth = useAuthStore();
  const config = useRuntimeConfig();
  const estimates = ref<Record<string, number>>({});
  const balance = ref(0);
  // 连接、握手、续期、退避都在 realtime store；这里只关心「本页的工作区 topic 订上了没有」。
  const realtime = useRealtimeStore();
  // 当前订着的工作区 topic；空串 = 没订（未打开画布或还没有工作区）。
  const topic = ref("");
  /**
   * ⚠️ 只有**本页的工作区 topic 真的订上了**才算在线：被拒的 topic 服务端照样回 subscribe.ok（CD-73），
   *    而别的 topic 订上了也不代表生成事件收得到。`CanvasBoard` 用它写 `data-realtime-state`。
   */
  const connectionState = computed<"offline" | "connecting" | "online">(() => {
    if (topic.value && realtime.isSubscribed(topic.value)) return "online";
    return topic.value && realtime.state === "connecting" ? "connecting" : "offline";
  });
  // 本页正在跟踪的任务：generationId → 节点。轮询、推送、重连对账都只认这里面的。
  const active = new Map<string, string>();
  // 正在对账的任务：推送、轮询、重连可能同时要对同一个任务对账，各发一次没有意义。
  const reconciling = new Set<string>();
  let activeProjectId = "";
  let opened = false;
  let releaseTopic: (() => void) | undefined;
  let detach: Array<() => void> = [];

  // ⚠️ 工作区切换时退订旧 topic、订新 topic：此前 topic 在 auth.ok 那一刻写死，
  //    切了工作区之后收的还是上一个工作区的事件，本工作区的任务一条推送都没有。
  //    同步触发：切换与随后的请求之间不留一个「还订着旧工作区」的窗口。
  watchSource(() => useWorkspaceStore().currentId, () => syncTopic(), { flush: "sync" });

  /**
   * ⚠️ **报价与受理的请求体不是同一个形状**（老后端是同一个）：
   *
   *   受理 `POST /generations`   → `inputs` 是**数组**，每项的 `kind` 是**来源类型**
   *                                 （`generation`/`reference`/`text`/`material`），换算见 `acceptInputsOf`
   *   报价 `POST /generations/estimate` → `inputs` 是 **`{角色:条数}` 的 map**（`roleCountsOf`）
   *
   * 把受理的数组原样发给报价，服务端 **40003「请求体不是合法 JSON」** ——
   * 而那句话指向 JSON 语法，实际 JSON 完全合法，只是 `inputs` 的类型对不上。
   */
  async function estimate(nodeId: string, request: GenerateRequest) {
    const response = await useApi()<EstimateResponse>("/generations/estimate", {
      method: "POST",
      body: {
        modelCode: request.modelCode,
        operation: request.operation,
        count: request.count ?? 1,
        durationSec: request.durationSec,
        text: request.text,
        inputs: roleCountsOf(request.inputs ?? []),
      },
    });
    // ⚠️ 新后端的字段是 `credits`（老后端 `creditsCost`）——
    //    读错的话报价**永远显示 0 积分**，而用户会以为这次生成是免费的。
    estimates.value[nodeId] = response.credits ?? response.creditsCost ?? 0;
    return estimates.value[nodeId];
  }

  async function generate(nodeId: string, request: GenerateRequest) {
    const created = await useApi()<GenerateResponse>("/generations", {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() },
      // ⚠️ inputs 必须换成契约的来源类型 —— 画布里的 kind 是媒体类型，原样发过去一律「不支持的输入类型」
      body: { ...request, inputs: acceptInputsOf(request.inputs ?? []) }
    });
    // ⚠️ 新后端返回的是 **`id`**（老后端 `generationId`）。
    const id = created.id ?? created.generationId;
    if (!id) throw new Error("接口响应缺少生成 ID");
    watch(nodeId, id, created.status ?? "queued");
    void poll(nodeId, id);
    await loadBalance();
    return id;
  }

  async function cancel(nodeId: string, generationId: string) {
    await useApi()<unknown>(`/generations/${generationId}/cancel`, { method: "POST" });
    active.delete(generationId);
    canvas.updateGeneration(nodeId, {
      status: "failed",
      errorCode: "cancelled",
      error: "用户已取消"
    });
    canvas.scheduleSave();
    await loadBalance();
  }

  async function retry(nodeId: string, generationId: string) {
    const created = await useApi()<GenerateResponse>(`/generations/${generationId}/retry`, {
      method: "POST",
      headers: { "Idempotency-Key": crypto.randomUUID() }
    });
    // ⚠️ 同受理：新后端回的是 **`id`**。只认 `generationId` 的话每次重试都在这里抛错 ——
    //    而服务端已经受理并扣了费，节点上却不显示进度，用户会再点一次（又一次扣费）。
    const id = created.id ?? created.generationId;
    if (!id) throw new Error("接口响应缺少生成 ID");
    watch(nodeId, id, created.status ?? "queued");
    void poll(nodeId, id);
    await loadBalance();
  }

  async function poll(nodeId: string, generationId: string) {
    for (let attempt = 0; attempt < 180 && active.has(generationId); attempt += 1) {
      const response = await useApi()<GenerationResponse>(`/generations/${generationId}`);
      await applyResponse(nodeId, response);
      if (isTerminalStatus(response.status)) return;
      await new Promise(resolve => setTimeout(resolve, 1000));
    }
    if (active.has(generationId)) {
      // ⚠️ **轮询耗尽不等于任务失败**（GAP-14）：浏览器停止查询与服务端
      //    停止工作是两件事 —— 任务多半还在跑，产物随后会落库。
      //    标成 failed 的话用户会去重试，而那是**再花一次钱**；
      //    更糟的是前端无权判定失败，那会与服务端的退款账目对不上。
      //    措辞里不能有"失败"二字。
      canvas.updateGeneration(nodeId, pollExhausted());
      // ⚠️ **停止轮询但不停止跟踪**：视频动辄跑好几分钟，轮询三分钟就到头了。
      //    从跟踪里拿掉的话，随后到的终态推送与重连对账都认不出它，节点就一直停在「仍在处理」。
    }
  }

  async function applyResponse(nodeId: string, response: GenerationResponse) {
    // ⚠️ 新后端：`id`（不是 `generationId`）、`errorMessage`（不是 `error`）。
    //    读错的话节点上**永远拿不到 ID 与失败原因** —— 状态能变，
    //    但"为什么失败"是空的，用户只看到一个红叉。
    const generationId = response.id ?? response.generationId;
    // ⚠️ 新后端的「已取消」是**独立终态**（不再是 failed + errorCode=cancelled）。节点上沿用本页点取消时的
    //    呈现（failed + errorCode=cancelled → 「已取消」）：照原样写 cancelled 的话节点既不显示已取消，
    //    这个任务也不会被当成落定 —— 在别处（任务中心）取消的任务在画布上一直挂着。
    const cancelled = response.status === "cancelled";
    canvas.updateGeneration(nodeId, {
      id: generationId,
      status: cancelled ? "failed" : response.status,
      ...(cancelled ? { errorCode: "cancelled" } : {}),
      progress: response.progress,
      error: response.errorMessage ?? response.error ?? (cancelled ? "已取消" : undefined)
    });
    // ⚠️ 节点不在当前画布上（换了项目，上一个项目的任务刚好落定）就不存盘：保存不判断有没有改动，
    //    存一次版本号就进一位，在别人也开着这个项目时害他下一次保存 409。
    const onCanvas = canvas.nodes.some(node => node.id === nodeId);
    if (response.status === "success") {
      // ⚠️ 产物要**单独取一次**：详情响应里没有 outputs（见 GenerationResponse 的注释）。
      if (generationId) await applyOutput(nodeId, await firstOutput(generationId));
      if (generationId) active.delete(generationId);
      if (onCanvas) canvas.scheduleSave();
      await loadBalance();
    } else if (response.status === "failed" || cancelled) {
      if (generationId) active.delete(generationId);
      if (onCanvas) canvas.scheduleSave();
      await loadBalance();
    }
  }

  function watch(nodeId: string, generationId: string, status: string) {
    active.set(generationId, nodeId);
    canvas.updateGeneration(nodeId, {
      id: generationId,
      status,
      progress: 0,
      errorCode: undefined,
      error: undefined
    });
    ensureRealtime();
  }

  function open(projectId: string) {
    // 换了项目：上一个项目的任务不再跟踪（它们的轮询随之停下）。回到那个项目时 restore() 会按画布重新接上。
    if (projectId !== activeProjectId) active.clear();
    activeProjectId = projectId;
    if (!opened) {
      opened = true;
      detach = [
        realtime.on("generation.update", frame => {
          const target = eventTarget(frame, activeProjectId, active);
          if (target) void reconcile(target.nodeId, target.generationId);
        }),
        // 真的订上了才对账 —— 订上之前那段时间里落定的任务，推送已经错过了
        //（协议约定：重连后先订阅、再用 HTTP 对账）。没订上（被拒）就靠轮询。
        realtime.onReconnected(() => {
          if (topic.value && realtime.isSubscribed(topic.value)) reconcileAll();
        }),
        // 连接意外断开：先对一次账，之后靠轮询兜底，直到重连。
        realtime.onDisconnected(() => reconcileAll()),
        // Agent 在画布开着时受理的任务（远端批次 link_generation）：与 restore() 同一套逻辑接上跟踪。
        // ⚠️ 不接的话推送只认已跟踪的任务（eventTarget），节点停在「排队中」直到刷新页面。
        // 已在跟踪（重复事件）就不再起第二条轮询。
        canvas.onGenerationLinked(({ nodeId, generationId }) => {
          if (active.has(generationId)) return;
          const node = canvas.nodes.find(item => item.id === nodeId);
          if (node) void restoreNode(node).catch(() => undefined);
        })
      ];
    }
    syncTopic();
    ensureRealtime();
    void loadBalance();
  }

  function close() {
    opened = false;
    for (const off of detach) off();
    detach = [];
    syncTopic();
    // 别处还订着 topic（如画布协作）就不断开连接；没人用了才断。
    if (realtime.activeTopics().length === 0) realtime.disconnect();
  }

  /**
   * 让订阅跟上「打开着画布 + 当前工作区」。
   * ⚠️ 拿不到工作区就不订：订了也是空的，还会让界面显示「在线」—— 退回轮询，如实显示离线。
   */
  function syncTopic() {
    const workspaceId = opened ? useWorkspaceStore().currentId : "";
    const next = workspaceId ? workspaceTopic(workspaceId) : "";
    if (next === topic.value) return;
    releaseTopic?.();
    releaseTopic = undefined;
    topic.value = next;
    if (next) releaseTopic = realtime.subscribe(next);
  }

  function ensureRealtime() {
    if (activeProjectId) realtime.connect();
  }

  function reconcileAll() {
    for (const [generationId, nodeId] of active) void reconcile(nodeId, generationId);
  }

  /**
   * 用 HTTP 对一次账：推送到达、订阅建立、连接断开时都走这里。
   *
   * ⚠️ 推送只是「该去对账了」的信号，结果一律以详情接口为准（GO-26）：事件里的 outputs 与端点形状不同、
   *    没有文本产物的正文，失败原因也以库里为准 —— 照事件渲染的话文本节点永远拿不到结果。
   */
  async function reconcile(nodeId: string, generationId: string) {
    if (reconciling.has(generationId)) return;
    reconciling.add(generationId);
    try {
      const response = await useApi()<GenerationResponse>(`/generations/${generationId}`);
      await applyResponse(nodeId, response);
    } catch {
      // 对账失败不处置：轮询还在跑；轮询耗尽之后，下一次推送或重连还会再来对账。
    } finally {
      reconciling.delete(generationId);
    }
  }

  async function restore() {
    for (const node of canvas.nodes) await restoreNode(node);
    ensureRealtime();
  }

  /**
   * 按节点上的 `result` 接上一个任务：成功的取产物，进行中的开始跟踪（轮询 + 推送对账）。
   * 打开画布时逐个节点走一遍（restore），远端批次挂上任务时只走那一个节点。
   */
  async function restoreNode(node: (typeof canvas.nodes)[number]) {
    const generation = node.data.generation;
    if (!generation) return;
    if (generation.status === "success") {
      // ⚠️ **文本节点没有 outputUrl**，而 `outputText` 存盘时被剥掉了
      //    （canvasData 里不留正文），所以刷新之后只能按 generationId 重新取 ——
      //    只认 outputUrl 的话，文本节点的结果**每次刷新就没了**，
      //    而任务明明是成功的、积分也扣过了。
      if (generation.id) {
        await applyOutput(node.id, await firstOutput(generation.id));
      } else if (generation.outputUrl) {
        // 老画布数据里可能只有 url 没有 id，按 url 兜底。
        await applyOutput(node.id, { type: generation.outputType, url: generation.outputUrl });
      }
    } else if (generation.id && ["queued", "running", "unknown"].includes(generation.status ?? "")) {
      // ⚠️ `unknown` 是「轮询耗尽、仍在处理」—— 节点上写着「稍后刷新查看结果」，
      //    刷新之后却不再去查的话，这个节点就永远停在那句话上。
      active.set(generation.id, node.id);
      ensureRealtime();
      void poll(node.id, generation.id);
    }
  }

  /**
   * 取这次生成的第一个产物。
   *
   * ⚠️ 取不到**不抛**：产物端点挂了不该让节点从"成功"倒回"失败" ——
   *    任务真的成功了、积分真的扣了，倒回去用户会重试、再扣一次。
   */
  async function firstOutput(generationId: string): Promise<GenerationOutput | undefined> {
    try {
      const list = await useApi()<GenerationOutput[]>(`/generations/${generationId}/outputs`);
      return list?.[0];
    } catch {
      return undefined;
    }
  }

  async function applyOutput(nodeId: string, output?: GenerationOutput) {
    // 「这个产物该怎么渲染」的判定抽在 utils/generationOutput 里单测。
    const plan = generationOutputPlan(output);
    if (plan.kind === "none") return;
    const node = canvas.nodes.find(item => item.id === nodeId);

    // ⚠️ 文本产物**直接带正文、没有 url**，不能走下面那条 fetch。
    if (plan.kind === "text") {
      const previous = node?.data.generation?.previewUrl;
      if (previous?.startsWith("blob:")) URL.revokeObjectURL(previous);
      canvas.updateGeneration(nodeId, {
        outputUrl: undefined,
        outputType: "text",
        outputText: plan.text,
        previewUrl: undefined
      });
      return;
    }

    // ⚠️ 请求头走 `authorizedMediaRequest` 这个**唯一来源**，不在这里自己拼 ——
    //    少带 `X-Workspace-Id` 的话 `/api/v1/media/{id}` 回 400，
    //    而节点上只是"成功了但不显示图"。
    //    （这里不用 `useAuthenticatedMedia`：它会注册 onScopeDispose，
    //      在 store 方法里按次调用是错的。）
    //    access 过期（401）时续期后重试一次 —— 这条路不经过 useApi。
    const workspaceId = useWorkspaceStore().currentId;
    const response = await auth.withTokenRefresh(token => {
      const init = authorizedMediaRequest(plan.url, token, String(config.public.apiBase), workspaceId);
      return fetch(init.url, { headers: init.headers });
    });
    if (!response.ok) throw new Error("生成结果加载失败");
    const decoded = await decodeGenerationOutput(response, plan.declaredType ?? node?.data.nodeType);

    const previousPreview = node?.data.generation?.previewUrl;
    if (previousPreview?.startsWith("blob:")) URL.revokeObjectURL(previousPreview);

    if (decoded.kind === "text") {
      canvas.updateGeneration(nodeId, {
        outputUrl: plan.url,
        outputType: decoded.kind,
        outputText: decoded.text,
        previewUrl: undefined
      });
      return;
    }
    canvas.updateGeneration(nodeId, {
      outputUrl: plan.url,
      outputType: decoded.kind,
      outputText: undefined,
      previewUrl: URL.createObjectURL(decoded.blob),
      // 媒体 ID 随结果存进画布：复制项目后生成 ID 不能再当输入，下游靠它按「参考」引用这张图。
      // ⚠️ 只在拿到时写 —— 副本按 url 恢复时没有它，写成 undefined 会把已有的抹掉。
      ...(output?.mediaId ? { mediaId: String(output.mediaId) } : {})
    });
  }

  async function loadBalance() {
    try {
      // ⚠️ 契约是 `{ balance, grants }`：此前读老结构的 `total`，恒为 undefined（画布顶栏的 ⚡ 不显示数字）
      const response = await useApi()<unknown>("/credits");
      balance.value = creditBalanceOf(response) ?? 0;
    } catch {
      balance.value = 0;
    }
  }

  return {
    estimates,
    balance,
    connectionState,
    estimate,
    generate,
    cancel,
    retry,
    open,
    close,
    restore,
    // 发布到画布的导演台截图（DRT-17）：节点是当场建的，`restore()` 只在页面挂载时跑过一次 ——
    // 不对新节点单独 restore 的话，它们直到刷新都是空的，看着像发布失败（DRB-7）
    restoreNode,
    loadBalance
  };
});
