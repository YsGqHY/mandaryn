package app

// 版本号：唯一事实源是 build/config.yml 的 info.version（前端经 __APP_VERSION__ 读取）。
//
// 为什么是变量而不是 const：Go 侧拿不到构建期 YAML，只能由构建脚本用
// `-ldflags "-X 'vip.oberon.mandaryn/internal/app.Version=<ver>'"` 注入。
// 未注入时保留 "dev"，更新检查遇到它只会说"无法判断"（不弹假更新）。
var Version = "dev"
