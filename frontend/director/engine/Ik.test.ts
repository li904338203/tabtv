import { Bone, Euler, Group, Quaternion, Vector3 } from "three";
import { describe, expect, it } from "vitest";
import { directorBoneOverrideOf, directorSetWorldQuaternion, directorSolveTwoBone } from "./Ik";

/** 一条手臂：根节点（带缩放与朝向，模拟角色的 root / heightNode）→ 上臂 → 前臂（30 cm）→ 手（25 cm）。 */
function arm(bend = 20) {
  const root = new Group();
  root.position.set(1, 0.5, -2);
  root.rotation.set(0, 0.7, 0);
  root.scale.setScalar(1.1);
  const upper = new Bone();
  upper.position.set(0, 1.4, 0);
  const lower = new Bone();
  lower.position.set(0, -0.3, 0);
  lower.rotation.set((bend * Math.PI) / 180, 0, 0);
  const hand = new Bone();
  hand.position.set(0, -0.25, 0);
  root.add(upper);
  upper.add(lower);
  lower.add(hand);
  root.updateMatrixWorld(true);
  return { root, upper, lower, hand };
}

const world = (o: { getWorldPosition(v: Vector3): Vector3 }) => o.getWorldPosition(new Vector3());

describe("两节解析 IK（DRT-21，DR-AC-08）", () => {
  it("够得着的目标：末端落到目标点（误差远小于 2 cm）", () => {
    // 上臂根在世界 (1, 2.04, -2)；链长 (0.3 + 0.25) × 1.1 = 0.605 m
    const offsets = [new Vector3(0.2, -0.3, 0.1), new Vector3(-0.3, -0.2, -0.1), new Vector3(0, 0.3, 0.3), new Vector3(0.35, -0.15, -0.2), new Vector3(0, -0.58, 0)];
    for (const offset of offsets) {
      const { upper, lower, hand } = arm();
      const target = world(upper).add(offset);
      const error = directorSolveTwoBone(upper, lower, hand, target, new Vector3(0, 0, -1));
      expect(error).toBeLessThan(1e-4);
      expect(world(hand).distanceTo(target)).toBeLessThan(1e-4);
    }
  });

  it("中间关节只朝原来那一侧弯（单向弯曲）", () => {
    const { upper, lower, hand } = arm(35);
    const side = (u: Vector3, m: Vector3, e: Vector3, normal: Vector3) => new Vector3().subVectors(m, u).cross(new Vector3().subVectors(e, m)).dot(normal);
    const normal = new Vector3().subVectors(world(lower), world(upper)).cross(new Vector3().subVectors(world(hand), world(lower))).normalize();
    const before = side(world(upper), world(lower), world(hand), normal);
    directorSolveTwoBone(upper, lower, hand, world(upper).add(new Vector3(0.15, -0.2, 0.15)), new Vector3(0, 0, -1));
    const after = side(world(upper), world(lower), world(hand), normal);
    expect(Math.sign(after)).toBe(Math.sign(before));
  });

  it("伸直时按给定的弯曲方向折，不会随机翻向", () => {
    const { upper, lower, hand } = arm(0);
    const hint = new Vector3(0, 0, 1);
    const target = world(upper).add(new Vector3(0, -0.4, 0));
    expect(directorSolveTwoBone(upper, lower, hand, target, hint)).toBeLessThan(1e-4);
    // 中间关节偏向 hint 那一侧
    const u = world(upper);
    const m = world(lower);
    const toTarget = new Vector3().subVectors(target, u).normalize();
    const offset = new Vector3().subVectors(m, u);
    offset.sub(toTarget.multiplyScalar(offset.dot(toTarget)));
    expect(offset.dot(hint)).toBeGreaterThan(0);
  });

  it("够不着时伸直指向目标，返回剩下的距离", () => {
    const { upper, lower, hand } = arm();
    const target = world(upper).add(new Vector3(0, 0, 2));
    const error = directorSolveTwoBone(upper, lower, hand, target, new Vector3(0, 0, -1));
    // 链长 (0.3 + 0.25) × 1.1 = 0.605 m
    expect(error).toBeCloseTo(2 - 0.605, 2);
    const direction = new Vector3().subVectors(world(hand), world(upper)).normalize();
    expect(direction.dot(new Vector3(0, 0, 1))).toBeGreaterThan(0.999);
  });
});

describe("骨骼覆盖的换算（DRD-4）", () => {
  it("directorBoneOverrideOf：rest × q(覆盖) 还原出当前旋转", () => {
    const rest = new Quaternion().setFromEuler(new Euler(0.3, -0.2, 0.9, "XYZ"));
    const relative = new Quaternion().setFromEuler(new Euler(0.5, 0.1, -0.4, "XYZ"));
    const current = rest.clone().multiply(relative);
    const override = directorBoneOverrideOf(rest, current);
    const back = rest.clone().multiply(new Quaternion().setFromEuler(new Euler(override.x * Math.PI / 180, override.y * Math.PI / 180, override.z * Math.PI / 180, "XYZ")));
    expect(back.angleTo(current)).toBeLessThan(1e-4);
    // 没转就是 0，且不出现 -0
    expect(directorBoneOverrideOf(rest, rest.clone())).toEqual({ x: 0, y: 0, z: 0 });
  });

  it("directorSetWorldQuaternion：只改局部旋转，让世界朝向回到给定值", () => {
    const { lower, hand } = arm();
    const wanted = new Quaternion().setFromEuler(new Euler(0.2, 1.1, -0.3, "XYZ"));
    lower.rotation.set(0.9, 0.1, 0.2);
    directorSetWorldQuaternion(hand, wanted);
    expect(hand.getWorldQuaternion(new Quaternion()).angleTo(wanted)).toBeLessThan(1e-6);
  });
});
