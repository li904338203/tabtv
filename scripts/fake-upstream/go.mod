// ⚠️ 独立 module，**不属于 backend/go**：假上游只用于本地联调，
// 放进主 module 会让它进依赖图、进构建产物，甚至有机会被误配到线上。
module github.com/li904338203/tabtv/scripts/fake-upstream

go 1.25
