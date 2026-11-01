import { describe, expect, it } from "vitest";

import { extensionFor, safeFilename } from "./download";

describe("下载文件名", () => {
  it("按 Content-Type 给扩展名 —— 不带扩展名的文件双击打不开", () => {
    expect(extensionFor("image/png")).toBe("png");
    expect(extensionFor("image/jpeg")).toBe("jpg");
    expect(extensionFor("video/mp4")).toBe("mp4");
    expect(extensionFor("audio/mpeg")).toBe("mp3");
    expect(extensionFor("text/plain;charset=utf-8")).toBe("txt");
    expect(extensionFor("IMAGE/PNG")).toBe("png");
  });

  it("认不出的类型给 bin，不猜", () => {
    expect(extensionFor("application/x-unknown")).toBe("bin");
    expect(extensionFor("")).toBe("bin");
    expect(extensionFor(null)).toBe("bin");
  });

  it("去掉系统不允许的字符（项目名是用户输入的）", () => {
    expect(safeFilename("分镜/第1镜:草稿?")).toBe("分镜_第1镜_草稿_");
    expect(safeFilename('a<b>c"d|e*f\\g')).toBe("a_b_c_d_e_f_g");
    expect(safeFilename("换行\n制表\t")).toBe("换行_制表_");
  });

  it("空名用兜底，不产生一个只有扩展名的文件", () => {
    expect(safeFilename("")).toBe("AITv作品");
    expect(safeFilename("   ")).toBe("AITv作品");
    expect(safeFilename("", "作品")).toBe("作品");
  });

  it("按字符截断，中文不会截出半个字", () => {
    const name = safeFilename("长".repeat(200));
    expect(Array.from(name)).toHaveLength(80);
    expect(name).toBe("长".repeat(80));
  });
});
