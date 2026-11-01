/**
 * 素模 GLB 与姿势数据的加载与缓存（DRT-12，dev/03 §9）。都是同源静态文件（`public/director/`），不需要鉴权。
 *
 * 缓存跨引擎实例保留（Promise 缓存）：关闭编辑器时引擎释放的是 GPU 资源（几何与材质的上传副本），
 * 这里留着的是 CPU 端的解析结果，下次打开不必重新下载、重新解析；渲染时 three 会重新上传。
 * 加载失败的 Promise 不缓存：网络抖动之后再打开编辑器能重试，而不是一直拿到同一个失败。
 */
import { GLTFLoader, type GLTF } from "three/addons/loaders/GLTFLoader.js";
import type { DirectorModelCode } from "~/utils/directorCatalog";
import type { DirectorBoneRotation } from "~/utils/directorRig";

/** `public/director/poses.json` 的一项（DRT-10）。`lift` 是离地高度占身高的比例（腾空姿势才有）。 */
export type DirectorPoseData = {
  code: string;
  name: string;
  category: string;
  bones: Record<string, DirectorBoneRotation>;
  lift?: number;
  thumbnail?: string;
};

const MODEL_BASE = "/director/models/";
const POSES_URL = "/director/poses.json";

const models = new Map<DirectorModelCode, Promise<GLTF>>();
let poses: Promise<Map<string, DirectorPoseData>> | null = null;

/** 加载一个素模（缓存）。 */
export function directorLoadMannequin(code: DirectorModelCode): Promise<GLTF> {
  let p = models.get(code);
  if (!p) {
    p = new GLTFLoader().loadAsync(`${MODEL_BASE}${code}.glb`);
    models.set(code, p);
    p.catch(() => models.delete(code));
  }
  return p;
}

/** 前端不认识的姿势码按这一个显示（DRT-20）。 */
export const DIRECTOR_FALLBACK_POSE = "stand_neutral";

/**
 * 按姿势码取姿势。认不出的码（AI 提案里的新姿势、比前端新的场景）退回自然站立 —— 退回绑定姿势的话是个
 * T 字形，看着像模型坏了；界面另外说明「姿势 X 暂不支持」（DRT-20）。空码保持空（只用骨骼覆盖）。
 * 角色与群众阵列共用这一个判定：各写一份的话同一个码在两处显示成两个样子。
 */
export function directorPoseFor(poses: Map<string, DirectorPoseData> | null | undefined, code: string): DirectorPoseData | undefined {
  if (!poses || !code) return undefined;
  return poses.get(code) ?? poses.get(DIRECTOR_FALLBACK_POSE);
}

/** 加载姿势库（缓存），按姿势码索引。 */
export function directorLoadPoses(): Promise<Map<string, DirectorPoseData>> {
  if (!poses) {
    const p = fetch(POSES_URL)
      .then((r) => {
        if (!r.ok) throw new Error(`姿势库加载失败（HTTP ${r.status}）`);
        return r.json() as Promise<DirectorPoseData[]>;
      })
      .then((list) => new Map(list.map((pose) => [pose.code, pose])));
    poses = p;
    p.catch(() => {
      if (poses === p) poses = null;
    });
  }
  return poses;
}
