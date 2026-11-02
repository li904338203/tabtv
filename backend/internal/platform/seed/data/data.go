// Package data 是随二进制附带的内置内容（TT-T-24）：营销分类 / 选项 / 技能 / 附属文档 / 预置营销角色，
// 以及选项、技能与角色的封面（由 tools/covers 从原图缩成长边 768 的 JPEG）。
//
// 文件名对应规则：
//   - 选项与营销技能的封面：images/options/<code>.jpg；技能（通用与营销草稿）：images/skills/<code>.jpg；
//   - 预置营销角色：数据文件 images[] 里的文件名，扩展名换成 .jpg，在 images/characters/ 下。
package data

import "embed"

//go:embed marketing/*.json images
var FS embed.FS
