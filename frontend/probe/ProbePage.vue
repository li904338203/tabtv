<script setup lang="ts">
/**
 * 三平台探针（TT-T-03，dev/05 V-1 / V-4）：只在 TABTV_PROBE=1 构建中注册为 /__probe。
 * 回答两个问题：
 *   V-1 Wails AssetServer 下播放大视频，WebView 会不会发 Range、能不能收到 206、拖动是否到位；
 *   V-4 当前网页内核有没有 WebCodecs（智能剪辑与导演录制依赖它），能否真的编出 1 帧。
 */
import { onMounted, ref } from "vue";

interface Hit { at: string; range: string; status: number }
const video = ref<HTMLVideoElement | null>(null);
const rows = ref<{ item: string; result: string }[]>([]);
const done = ref(false);

function put(item: string, result: string) {
  rows.value.push({ item, result });
}

function wait(ms: number) {
  return new Promise(r => setTimeout(r, ms));
}

async function probeRange() {
  const el = video.value!;
  await new Promise<void>((resolve, reject) => {
    el.onloadedmetadata = () => resolve();
    el.onerror = () => reject(new Error(`video error ${el.error?.code}`));
    el.src = "/api/_probe/video";
  });
  put("duration", `${el.duration.toFixed(1)}s`);
  const target = el.duration * 0.8;
  el.currentTime = target;
  await new Promise<void>(resolve => {
    el.onseeked = () => resolve();
    setTimeout(resolve, 8000);
  });
  put("seek 到 80%", `currentTime=${el.currentTime.toFixed(1)}（目标 ${target.toFixed(1)}）→ ${Math.abs(el.currentTime - target) < 2 ? "到位" : "未到位"}`);
  try {
    await el.play();
    await wait(1500);
    put("seek 后播放", `currentTime=${el.currentTime.toFixed(1)}，paused=${el.paused}`);
  } catch (e) {
    put("seek 后播放", `失败：${(e as Error).message}`);
  }
  const hits: Hit[] = await (await fetch("/api/_probe/log")).json();
  put("视频请求次数", String(hits.length));
  put("带 Range 的请求", String(hits.filter(h => h.range).length));
  put("206 响应", String(hits.filter(h => h.status === 206).length));
  put("请求明细", hits.map(h => `${h.range || "(无 Range)"}→${h.status}`).join("；"));
}

/** 来源前端多数视频走 blob → object URL（useAuthenticatedMedia）：这条路在各内核上能不能播、能不能拖。 */
async function probeBlob() {
  const el = document.createElement("video");
  el.muted = true;
  const blob = await (await fetch("/api/_probe/video")).blob();
  put("blob 大小", `${(blob.size / 1048576).toFixed(1)} MB`);
  const url = URL.createObjectURL(blob);
  try {
    await new Promise<void>((resolve, reject) => {
      el.onloadedmetadata = () => resolve();
      el.onerror = () => reject(new Error(`video error ${el.error?.code}`));
      el.src = url;
      setTimeout(() => reject(new Error("加载超时")), 15000);
    });
    const target = el.duration * 0.8;
    el.currentTime = target;
    await new Promise<void>(resolve => { el.onseeked = () => resolve(); setTimeout(resolve, 8000); });
    put("blob 播放 seek", `currentTime=${el.currentTime.toFixed(1)}（目标 ${target.toFixed(1)}）→ ${Math.abs(el.currentTime - target) < 2 ? "到位" : "未到位"}`);
  } catch (e) {
    put("blob 播放", `失败：${(e as Error).message}`);
  } finally {
    URL.revokeObjectURL(url);
  }
}

async function probeWebCodecs() {
  const g = globalThis as Record<string, unknown>;
  for (const k of ["VideoEncoder", "VideoDecoder", "AudioEncoder", "AudioDecoder", "VideoFrame"]) {
    put(k, typeof g[k] === "function" ? "有" : "无");
  }
  if (typeof g.VideoEncoder !== "function") return;
  try {
    const config = { codec: "avc1.42001f", width: 640, height: 360, bitrate: 1_000_000, framerate: 30 };
    const sup = await VideoEncoder.isConfigSupported(config);
    put("H.264 编码配置", sup.supported ? "支持" : "不支持");
    if (!sup.supported) return;
    let bytes = 0;
    const enc = new VideoEncoder({ output: chunk => { bytes += chunk.byteLength; }, error: e => put("编码错误", e.message) });
    enc.configure(config);
    const canvas = new OffscreenCanvas(640, 360);
    const ctx = canvas.getContext("2d")!;
    ctx.fillStyle = "#3a6"; ctx.fillRect(0, 0, 640, 360);
    const frame = new VideoFrame(canvas, { timestamp: 0 });
    enc.encode(frame, { keyFrame: true });
    frame.close();
    await enc.flush();
    enc.close();
    put("编码 1 帧", bytes > 0 ? `成功（${bytes} 字节）` : "无输出");
  } catch (e) {
    put("编码 1 帧", `失败：${(e as Error).message}`);
  }
}

onMounted(async () => {
  put("userAgent", navigator.userAgent);
  try { await probeRange(); } catch (e) { put("视频探针", `失败：${(e as Error).message}`); }
  try { await probeBlob(); } catch (e) { put("blob 探针", `失败：${(e as Error).message}`); }
  await probeWebCodecs();
  done.value = true;
  // 把结果交给 Go 侧落盘并退出（无人值守跑三平台时用）。
  await fetch("/api/_probe/report", { method: "POST", body: JSON.stringify(rows.value) }).catch(() => {});
});
</script>

<template>
  <div style="padding: 24px; font: 14px/1.6 system-ui">
    <h2>tabtv 三平台探针</h2>
    <video ref="video" controls muted style="width: 480px; background: #000" />
    <table data-probe-done :data-done="done" style="margin-top: 16px; border-collapse: collapse">
      <tr v-for="r in rows" :key="r.item">
        <td style="padding: 4px 12px; border: 1px solid #888">{{ r.item }}</td>
        <td style="padding: 4px 12px; border: 1px solid #888">{{ r.result }}</td>
      </tr>
    </table>
    <p v-if="done">探针完成</p>
  </div>
</template>
