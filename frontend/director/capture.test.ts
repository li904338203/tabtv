import { describe, expect, it } from "vitest";
import { directorCameraSnapshot, directorCaptureForm, directorCurrentCameraId, directorDepthFilename } from "./capture";
import type { DirectorObject } from "~/utils/directorScene";

const camera = (id: string, extra: Partial<DirectorObject> = {}): DirectorObject => ({
  id, name: `机位 ${id}`, kind: "camera",
  transform: { position: { x: 0, y: 1.6, z: 4 }, rotation: { x: -5, y: 0, z: 0 }, scale: { x: 1, y: 1, z: 1 } },
  camera: { focalLengthMm: 50, aspect: "9:16" },
  ...extra
});

describe("截图上传（DRT-17）", () => {
  it("请求体带文件、种类、机位、刚存上的版本与机位快照", async () => {
    const form = directorCaptureForm({ blob: new Blob(["png"], { type: "image/png" }), kind: "capture", cameraId: "c1", sceneVersion: 7, camera: directorCameraSnapshot(camera("c1")) });
    expect(form.get("kind")).toBe("capture");
    expect(form.get("cameraId")).toBe("c1");
    expect(form.get("sceneVersion")).toBe("7");
    expect((form.get("file") as File).name).toBe("capture.png");
    expect(JSON.parse(String(form.get("camera")))).toEqual({
      name: "机位 c1", focalLengthMm: 50, sensorWidthMm: 36, aspect: "9:16", near: 0.1, far: 1000, roll: 0,
      position: { x: 0, y: 1.6, z: 4 }, rotation: { x: -5, y: 0, z: 0 }
    });
  });

  it("「截当前机位」：正透过的机位 → 选中的机位 → 输出机位 → 第一个机位", () => {
    const ids = ["c1", "c2", "c3"];
    expect(directorCurrentCameraId({ viewCameraId: "c2", selected: camera("c3"), outputCameraId: "c1", cameraIds: ids })).toBe("c2");
    expect(directorCurrentCameraId({ viewCameraId: null, selected: camera("c3"), outputCameraId: "c1", cameraIds: ids })).toBe("c3");
    expect(directorCurrentCameraId({ viewCameraId: null, selected: undefined, outputCameraId: "c1", cameraIds: ids })).toBe("c1");
    expect(directorCurrentCameraId({ viewCameraId: "gone", selected: undefined, outputCameraId: null, cameraIds: ids })).toBe("c1");
    expect(directorCurrentCameraId({ viewCameraId: null, selected: undefined, outputCameraId: null, cameraIds: [] })).toBeNull();
  });
  it("directorDepthFilename：近远（米，两位小数）写进文件名，机位名去空白、去掉文件系统不认的字符", () => {
    expect(directorDepthFilename("机位 1", 0.8, 12.4)).toBe("机位1-depth-near0.80-far12.40.png");
    expect(directorDepthFilename("A/B:C", 1, 2)).toBe("A_B_C-depth-near1.00-far2.00.png");
    expect(directorDepthFilename("   ", 1, 2)).toBe("机位-depth-near1.00-far2.00.png");
  });
});
