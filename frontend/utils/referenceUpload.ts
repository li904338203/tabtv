/**
 * 参考文件上传（`POST /media/references`）的请求体 —— 纯函数（MKT-T-12）。
 *
 * 只有 `file` 必填（MKT-T-01）；`mediaType` 按文件类型传（服务端会再按文件头核对，声明的 MIME 不可信）；
 * `projectId` 当前实现不读，有就带上（保留兼容）。真正发请求的是 `composables/useReferenceUpload`。
 */
export type ReferenceMediaType = "image" | "video" | "audio";

/** 按浏览器给的 MIME 判类型；不是图片 / 视频 / 音频返回 undefined（调用方拒收并说明）。 */
export function referenceMediaType(file: { type: string }): ReferenceMediaType | undefined {
  if (file.type.startsWith("image/")) return "image";
  if (file.type.startsWith("video/")) return "video";
  if (file.type.startsWith("audio/")) return "audio";
  return undefined;
}

export function referenceUploadForm(file: Blob, options: { mediaType?: ReferenceMediaType; projectId?: string } = {}): FormData {
  const body = new FormData();
  body.append("file", file);
  if (options.mediaType) body.append("mediaType", options.mediaType);
  if (options.projectId) body.append("projectId", options.projectId);
  return body;
}
