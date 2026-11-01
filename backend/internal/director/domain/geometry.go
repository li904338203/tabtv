package domain

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 求解器用到的几何：向量、欧拉角与视角换算。
//
// ⚠️ 欧拉角口径与前端引擎（three.js `Object3D.rotation`，默认顺序 XYZ）**逐字一致**：
// 场景里存的是度数，矩阵 = Rx(x)·Ry(y)·Rz(z)。机位的朝向用「偏航 → 俯仰」更好算，
// 写进场景前要换算成 XYZ 顺序的欧拉角；直接把偏航写进 y、俯仰写进 x 的话，
// 偏航不为 0 时前端看到的是一个歪掉的机位，而且不报错（DR-AC-29 用投影断言钉住这一点）。

type vec3f struct{ x, y, z float64 }

func (a vec3f) add(b vec3f) vec3f              { return vec3f{a.x + b.x, a.y + b.y, a.z + b.z} }
func (a vec3f) sub(b vec3f) vec3f              { return vec3f{a.x - b.x, a.y - b.y, a.z - b.z} }
func (a vec3f) scale(k float64) vec3f          { return vec3f{a.x * k, a.y * k, a.z * k} }
func (a vec3f) dot(b vec3f) float64            { return a.x*b.x + a.y*b.y + a.z*b.z }
func (a vec3f) length() float64                { return math.Sqrt(a.dot(a)) }
func (a vec3f) horizontal() vec3f              { return vec3f{a.x, 0, a.z} }
func (a vec3f) horizontalDist(b vec3f) float64 { return a.sub(b).horizontal().length() }

func (a vec3f) cross(b vec3f) vec3f {
	return vec3f{a.y*b.z - a.z*b.y, a.z*b.x - a.x*b.z, a.x*b.y - a.y*b.x}
}

// normalized 返回单位向量；长度近似为 0 时返回 fallback。
func (a vec3f) normalized(fallback vec3f) vec3f {
	l := a.length()
	if l < 1e-9 {
		return fallback
	}
	return a.scale(1 / l)
}

var (
	axisX = vec3f{1, 0, 0}
	axisY = vec3f{0, 1, 0}
	axisZ = vec3f{0, 0, 1}
)

func deg(rad float64) float64 { return rad * 180 / math.Pi }
func rad(deg float64) float64 { return deg * math.Pi / 180 }

// mat3 是行优先的 3×3 矩阵：m[i][j] 是第 i 行第 j 列，列向量 = 局部坐标轴在世界里的方向。
type mat3 [3][3]float64

func (m mat3) mulVec(v vec3f) vec3f {
	return vec3f{
		m[0][0]*v.x + m[0][1]*v.y + m[0][2]*v.z,
		m[1][0]*v.x + m[1][1]*v.y + m[1][2]*v.z,
		m[2][0]*v.x + m[2][1]*v.y + m[2][2]*v.z,
	}
}

func (m mat3) mul(n mat3) mat3 {
	var out mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				out[i][j] += m[i][k] * n[k][j]
			}
		}
	}
	return out
}

func (m mat3) transpose() mat3 {
	var out mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			out[i][j] = m[j][i]
		}
	}
	return out
}

func rotX(a float64) mat3 {
	c, s := math.Cos(a), math.Sin(a)
	return mat3{{1, 0, 0}, {0, c, -s}, {0, s, c}}
}

func rotY(a float64) mat3 {
	c, s := math.Cos(a), math.Sin(a)
	return mat3{{c, 0, s}, {0, 1, 0}, {-s, 0, c}}
}

func rotZ(a float64) mat3 {
	c, s := math.Cos(a), math.Sin(a)
	return mat3{{c, -s, 0}, {s, c, 0}, {0, 0, 1}}
}

// eulerXYZMatrix 是 three.js 顺序 XYZ 的旋转矩阵（入参为度）：Rx(x)·Ry(y)·Rz(z)。
func eulerXYZMatrix(xDeg, yDeg, zDeg float64) mat3 {
	return rotX(rad(xDeg)).mul(rotY(rad(yDeg))).mul(rotZ(rad(zDeg)))
}

// eulerXYZFromMatrix 是 three.js `Euler.setFromRotationMatrix`（顺序 XYZ）的同一套分解，返回度。
func eulerXYZFromMatrix(m mat3) (xDeg, yDeg, zDeg float64) {
	m13 := math.Max(-1, math.Min(1, m[0][2]))
	y := math.Asin(m13)
	var x, z float64
	if math.Abs(m13) < 0.9999999 {
		x = math.Atan2(-m[1][2], m[2][2])
		z = math.Atan2(-m[0][1], m[0][0])
	} else {
		x = math.Atan2(m[2][1], m[1][1])
		z = 0
	}
	return deg(x), deg(y), deg(z)
}

// cameraEuler 把「偏航 yaw（绕世界 Y）→ 俯仰 pitch（绕自身 X，抬头为正）」换算成场景里存的 XYZ 欧拉角。
// 机位 rotation 全 0 时朝 −Z 看（scene.go 包注释）；横滚不在这里，单独存在 `camera.roll`（引擎绕视线轴转）。
func cameraEuler(yawDeg, pitchDeg float64) (xDeg, yDeg, zDeg float64) {
	return eulerXYZFromMatrix(rotY(rad(yawDeg)).mul(rotX(rad(pitchDeg))))
}

// cameraBasis 由机位的 XYZ 欧拉角与横滚给出右、上、视线三个方向（世界坐标）。
func cameraBasis(rot vec3f, rollDeg float64) (right, up, forward vec3f) {
	m := eulerXYZMatrix(rot.x, rot.y, rot.z).mul(rotZ(rad(rollDeg)))
	return m.mulVec(axisX), m.mulVec(axisY), m.mulVec(vec3f{0, 0, -1})
}

// facingYaw 返回「面朝 dir」时角色的 rotation.y（度）：角色 rotation.y = 0 时面朝 +Z。
func facingYaw(dir vec3f) float64 {
	h := dir.horizontal()
	if h.length() < 1e-9 {
		return 0
	}
	return deg(math.Atan2(h.x, h.z))
}

// forwardOfYaw 是 rotation.y（度）对应的角色正前方（水平单位向量）。
func forwardOfYaw(yawDeg float64) vec3f {
	r := rad(yawDeg)
	return vec3f{math.Sin(r), 0, math.Cos(r)}
}

// aspectRatio 把画幅码（如 "16:9"）换成宽 ÷ 高；不认识的画幅返回错误，不猜。
func aspectRatio(code string) (float64, error) {
	if _, ok := AspectOf(code); !ok {
		return 0, fmt.Errorf("不认识的画幅 %q", code)
	}
	w, h, ok := strings.Cut(code, ":")
	if !ok {
		return 0, fmt.Errorf("画幅 %q 不是 宽:高", code)
	}
	wf, err1 := strconv.ParseFloat(w, 64)
	hf, err2 := strconv.ParseFloat(h, 64)
	if err1 != nil || err2 != nil || wf <= 0 || hf <= 0 {
		return 0, fmt.Errorf("画幅 %q 不是 宽:高", code)
	}
	return wf / hf, nil
}

// verticalFOV 返回竖直视角（弧度）。
//
// ⚠️ 与前端 `directorVerticalFov` 是**同一个算法**（DRT-11，共享 fixture `camera/fov.json`）：
// `sensorMm` 是传感器**长边**（three 的 filmGauge 语义）—— 横幅时长边是宽，
// 画面高 = 长边 ÷ 宽高比；竖幅时长边就是高。两边各写一种的话，AI 按中景摆的机位，
// 截出来是近景或全景，而两边各自看都没错。
func verticalFOV(focalMm, sensorMm float64, aspect string) (float64, error) {
	if focalMm <= 0 || sensorMm <= 0 {
		return 0, fmt.Errorf("焦距与传感器尺寸必须为正")
	}
	ratio, err := aspectRatio(aspect)
	if err != nil {
		return 0, err
	}
	filmHeight := sensorMm
	if ratio >= 1 {
		filmHeight = sensorMm / ratio
	}
	return 2 * math.Atan(filmHeight/(2*focalMm)), nil
}

// round4 保留 4 位小数（米或度）：写进场景的数不带一长串浮点尾巴，精度仍到 0.1 毫米。
func round4(f float64) float64 {
	r := math.Round(f*1e4) / 1e4
	if r == 0 {
		return 0 // 把 -0 归成 0
	}
	return r
}
