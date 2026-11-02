/**
 * 项目包（`<项目名>.tabtv.zip`）的文件名与提示文案（TT-T-33）。纯函数，调接口在 `useProjectActions`。
 *
 * ⚠️ 文件名规则与后端 `projectpkg.FileName` 一致：路径字符换成 `_`、过长截断。
 *    另存为对话框里的建议名用它 —— 后端响应头里也有，但 blob 下载拿不到可靠的响应头。
 */
export const PROJECT_PACKAGE_SUFFIX = ".tabtv.zip";

export function projectPackageFileName(name: string): string {
  let base = (name ?? "").replace(/[/\\:*?"<>|]/g, "_").trim();
  if (!base) base = "project";
  const chars = Array.from(base);
  if (chars.length > 80) base = chars.slice(0, 80).join("");
  return base + PROJECT_PACKAGE_SUFFIX;
}

/** 文件选择器里过滤：只认 zip。 */
export function isProjectPackageFile(fileName: string): boolean {
  return /\.zip$/i.test(fileName);
}

export type ProjectImportResult = { projectId: string; name: string; warnings?: string[] };

/**
 * 导入成功后的提示。⚠️ 有 warnings（缺文件的媒体、本机没配的模型）要说出来：
 * 不说的话用户打开项目看到裂图或「缺模型」，会以为导入坏了。
 */
export function projectImportMessage(result: ProjectImportResult): { text: string; warn: boolean } {
  const warnings = result.warnings ?? [];
  if (!warnings.length) return { text: `已导入为「${result.name}」`, warn: false };
  const head = warnings.slice(0, 3).join("；");
  const more = warnings.length > 3 ? `等 ${warnings.length} 条` : "";
  return { text: `已导入为「${result.name}」，但有需要注意的地方：${head}${more}`, warn: true };
}
