// Package outbound 是**出站请求的统一闸门与留痕**（FR-08 / NFR-03 / DOC-04 TBL-12）。
//
// 两条铁律：
//  1. 「出站要留痕」：任何新增出站请求都必须落 outbound_log（域名 + 是否含音频 + 结果），
//     不落完整 URL（避免 query 里的凭据 / 参数泄漏）。
//  2. 「离线开关」：设置 MANDARYN_OFFLINE=1 后禁止**一切**出站（含模型下载与在线转写），
//     调用方必须在发起前调 Guard，拿到错误就停止。
//
// 只允许出站的场景（PRD §6.1）：模型下载（可关）、在线转写（用户显式开启）。
// 本地模式下除模型下载外不应有任何出站。
package outbound

import (
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// EnvOffline 全局离线开关的环境变量名。
const EnvOffline = "MANDARYN_OFFLINE"

// Retention 出站记录的保留时长（DOC-04 TBL-12：仅最近 24h）。
const Retention = 24 * time.Hour

// userOffline 是「用户层离线开关」的读取函数（进程级，装配阶段注册一次）。
//
// 为什么放在这里：既有出站调用方（asr / models / enginesvc）只调 Guard / Offline，
// 让它们各自去读设置会把口径散成多份。由 internal/privacy 的 Gate 在装配时
// 用 SetUserOffline 注册一次，所有调用方**无需改动**就遵守用户离线开关。
// 用原子指针而不是互斥量：读多写一次，且 Offline 会在每个出站请求前被调用。
var userOffline atomic.Pointer[func() bool]

// SetUserOffline 注册用户离线开关读取函数；传 nil 解除注册（测试与单实例场景）。
//
// 约定：fn 必须快速、无阻塞、失败时返回 false（读不到设置视为未开启），
// 且**不得**在内部再调用 Offline / Guard（会递归）。
func SetUserOffline(fn func() bool) {
	if fn == nil {
		userOffline.Store(nil)
		return
	}
	cp := fn
	userOffline.Store(&cp)
}

// EnvOfflineEnabled 只报告**环境变量层**是否开启（不含用户设置）。
func EnvOfflineEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvOffline))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Offline 报告当前是否处于全局离线模式：环境变量开启**或**用户开关开启。
//
// 环境变量接受 "1" / "true" / "yes" / "on"（大小写不敏感），其它值视为关闭。
func Offline() bool {
	if EnvOfflineEnabled() {
		return true
	}
	if fn := userOffline.Load(); fn != nil && *fn != nil {
		return (*fn)()
	}
	return false
}

// ErrOffline 是离线模式下的统一拒绝错误。
func ErrOffline() *apierr.Err {
	return apierr.New(apierr.CodeOfflineEnforced).
		WithAction("disable-offline").
		WithDetail("offline mode is enabled: all outbound requests are blocked")
}

// Guard 在发起出站请求前调用；离线时返回 ERR_OFFLINE_ENFORCED。
//
// 用法：
//
//	if err := outbound.Guard(); err != nil { return err }
func Guard() error {
	if Offline() {
		return ErrOffline()
	}
	return nil
}

// Recorder 负责把出站记录写入 outbound_log。
//
// 与 httpx 的关系：httpx 不知道业务语义，因此由**调用方**（下载器 / 在线引擎）
// 在请求前后调 Record；这样记录里能带「是否含音频」「业务类型」这类信息。
type Recorder struct {
	holder *storage.Holder

	mu   sync.Mutex
	last time.Time // 简单节流：同一秒同一域名只记一次，避免长连接刷屏
}

// NewRecorder 构造记录器。holder 可为 nil（此时只做节流判断、不落库）。
func NewRecorder(holder *storage.Holder) *Recorder {
	return &Recorder{holder: holder}
}

// Record 落一条出站记录。
//
// 故意**不返回错误**：留痕失败不应该让用户的转写 / 下载失败（NFR-04-1 同理），
// 调用方也不必处理；失败只影响「最近出站点」面板的完整性。
func (r *Recorder) Record(host, kind string, audio bool, bytes int64, result string) {
	if host == "" {
		return
	}
	r.mu.Lock()
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < time.Second {
		r.mu.Unlock()
		return
	}
	r.last = now
	r.mu.Unlock()

	if r.holder == nil {
		return
	}
	db := r.holder.Current()
	if db == nil || db.GORM == nil {
		return
	}
	row := storage.OutboundLog{
		At:     now.UnixMilli(),
		Host:   host,
		Kind:   kind,
		Audio:  audio,
		Bytes:  bytes,
		Result: result,
	}
	_ = db.GORM.Create(&row).Error
}

// List 返回最近 limit 条出站记录（新的在前），供设置页「最近出站点」展示。
func (r *Recorder) List(limit int) ([]contract.OutboundEntry, error) {
	if r.holder == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	db := r.holder.Current()
	var rows []storage.OutboundLog
	if err := db.GORM.Order("at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]contract.OutboundEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, contract.OutboundEntry{
			At:     row.At,
			Host:   row.Host,
			Kind:   row.Kind,
			Audio:  row.Audio,
			Bytes:  row.Bytes,
			Result: row.Result,
		})
	}
	return out, nil
}

// Purge 清理超过保留期的记录（启动时调用一次）。
func (r *Recorder) Purge() error {
	if r.holder == nil {
		return nil
	}
	db := r.holder.Current()
	if db == nil || db.GORM == nil {
		return nil
	}
	return storage.PurgeOldOutbound(db.GORM, time.Now(), Retention)
}

// ErrNoHost 用于调用方校验（避免把空域名记成一条无意义记录）。
var ErrNoHost = errors.New("outbound: empty host")
