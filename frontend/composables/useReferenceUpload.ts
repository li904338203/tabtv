import { referenceMediaType, referenceUploadForm, type ReferenceMediaType } from "~/utils/referenceUpload";

/**
 * 上传一个参考文件，返回服务端给的媒体 ID（MKT-T-12：Agent 输入框附件与营销槽位共用这一份）。
 *
 * ⚠️ 只在用户操作时调用（选文件 / 点重试）；页面打开不上传。
 * ⚠️ 响应缺 ID 当失败：否则那张图显示「已上传」而发出去的引用是空的。
 */
export function useReferenceUpload() {
  async function upload(file: File, options: { mediaType?: ReferenceMediaType; projectId?: string } = {}): Promise<string> {
    const mediaType = options.mediaType ?? referenceMediaType(file);
    const response = await useApi()<{ id?: string }>("/media/references", {
      method: "POST",
      body: referenceUploadForm(file, { mediaType, projectId: options.projectId })
    });
    if (!response?.id) throw new Error("上传接口响应缺少 ID");
    return String(response.id);
  }
  return { upload };
}
