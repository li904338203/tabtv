import type { components as ApiComponents } from "@tabtv/api-client";
import { ElMessage, ElMessageBox } from "element-plus";

type Folder = ApiComponents["schemas"]["Folder"];

/** 卡片菜单需要的最小项目形状 —— 工作台的最近项目与全部项目的条目都满足它。 */
export type ProjectCardRef = {
  id?: string;
  name?: string;
  /** 所在文件夹；空串 / null / 缺省都表示未分类。 */
  folderId?: string | null;
};

/** 项目名上限，与后端 `PATCH /projects/{id}` 的校验一致。 */
export const PROJECT_NAME_MAX = 100;

/**
 * 项目卡片「⋯」菜单背后的操作 —— **全站唯一一份**。
 *
 * ⚠️ 工作台、AI 视频页、全部项目页三处都有同一个菜单。此前只有全部项目页有，
 *    若照着再抄两份，确认文案、报错文案、调哪个接口就会散成三份 ——
 *    改了一处另两处不会报错，而表现是「同一个删除，在首页说能恢复、在列表页说不能」。
 *
 * 每个操作返回**这次是否真的改了东西**，调用方据此刷新自己的列表
 * （三个页面的列表来源不同：/home、/projects?limit=6、滚动分页）。
 * 失败一律在这里提示，调用方不再各写一句。
 */
export function useProjectActions() {
  const folders = ref<Folder[]>([]);
  let foldersLoaded = false;

  /**
   * 文件夹列表 —— 「移动至文件夹」的子菜单要用。
   *
   * 默认只取一次（首页没必要为一个不常用的子菜单每次都多打一个请求）；
   * 建/删/改名文件夹之后传 `force` 重取。失败时退回空列表：
   * 子菜单不出现而已，不该让整个菜单打不开。
   */
  async function loadFolders(force = false) {
    if (foldersLoaded && !force) return folders.value;
    try {
      folders.value = await useApi()<Folder[]>("/folders");
      foldersLoaded = true;
    } catch {
      folders.value = [];
    }
    return folders.value;
  }

  /**
   * 改名。空串或与原名相同视为「没改」，不发请求。
   * ⚠️ 超长在这里就拦下并说清上限 —— 发出去再被后端拒，用户只看到一句「重命名失败」，
   *    不知道是名字太长。
   */
  async function rename(project: ProjectCardRef, next: string): Promise<boolean> {
    const name = next.trim();
    if (!project.id || !name || name === (project.name ?? "")) return false;
    if (name.length > PROJECT_NAME_MAX) {
      ElMessage.error(`名称不能超过 ${PROJECT_NAME_MAX} 个字符`);
      return false;
    }
    try {
      await useApi()(`/projects/${project.id}`, { method: "PATCH", body: { name } });
      return true;
    } catch {
      ElMessage.error("重命名失败");
      return false;
    }
  }

  async function duplicate(project: ProjectCardRef): Promise<boolean> {
    if (!project.id) return false;
    try {
      await useApi()(`/projects/${project.id}/duplicate`, { method: "POST" });
      ElMessage.success("已创建副本");
      return true;
    } catch {
      ElMessage.error("创建副本失败");
      return false;
    }
  }

  /** 移动到文件夹。空串表示移回未分类 —— 与后端 PATCH 的语义一致。 */
  async function moveToFolder(project: ProjectCardRef, folderId: string): Promise<boolean> {
    if (!project.id) return false;
    try {
      await useApi()(`/projects/${project.id}`, { method: "PATCH", body: { folderId } });
      ElMessage.success(folderId ? "已移动" : "已移回未分类");
      return true;
    } catch {
      ElMessage.error("移动失败");
      return false;
    }
  }

  async function remove(project: ProjectCardRef): Promise<boolean> {
    if (!project.id) return false;
    try {
      await ElMessageBox.confirm(
        // ⚠️ 删除是软删，说「无法恢复」是在骗用户 —— 项目会进回收站，30 天内可恢复
        `「${project.name}」将移入回收站，30 天内可以恢复。`,
        "删除项目",
        { confirmButtonText: "删除", cancelButtonText: "取消", type: "warning" }
      );
    } catch {
      // 取消/关闭弹窗时 reject，不是错误
      return false;
    }
    try {
      await useApi()(`/projects/${project.id}`, { method: "DELETE" });
      return true;
    } catch {
      ElMessage.error("删除失败");
      return false;
    }
  }

  return { folders, loadFolders, rename, duplicate, moveToFolder, remove };
}
