package speaker

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

// Options 是领域包的依赖注入点。
type Options struct {
	// Holder 必填；每次查询都取 Current()（切数据路径会换 DB 句柄，禁止缓存 *DB）。
	Holder *storage.Holder
	// Now 时钟（单测注入固定时间）；nil 用 time.Now。
	Now func() time.Time

	// Extractor 声纹提取器。**允许为 nil**：本机未装声纹模型包时（DEV-ISSUES-D ENV-10）
	// 声纹功能整体降级 —— `Run` 返回明确错误码，而改名 / 合并 / 拆分 / 指派等
	// 纯数据库操作必须仍然可用（FR-06-5 不依赖推理）。
	Extractor Extractor
	// Audio 片段级音频读取端口；Extractor 非 nil 时必填（否则无法取样本）。
	Audio AudioLoader

	// MaxSpeakers 会话内说话人数上限（`app_settings.max_speakers`）。
	// 返回 0 表示不限制。nil → 直接读 app_settings 单例行。
	MaxSpeakers func(ctx context.Context) (int, error)

	// MinSpeechMs 过短片段门槛（ARCH §4.4：< 1.2s 不提取声纹）；<=0 用默认 1200。
	MinSpeechMs int64
	// ClusterThreshold / MergeThreshold 覆盖默认阈值（仅测试与诊断用）。
	ClusterThreshold float64
	MergeThreshold   float64

	// OnRolesChanged 角色变更回调（EV-07 `roles:changed`）。
	//
	// L2 不直接发 Wails 事件（装配层负责），因此这里只把**载荷素材**交出去：
	// 改名 / 合并 / 拆分 / 指派成功后各调用一次。
	OnRolesChanged func(contract.RoleChanged)
}

// Repo 是说话人与角色的领域实现。
type Repo struct {
	holder           *storage.Holder
	now              func() time.Time
	extractor        Extractor
	audio            AudioLoader
	maxSpeakersFn    func(ctx context.Context) (int, error)
	minSpeechMs      int64
	clusterThreshold float64
	mergeThreshold   float64
	onRolesChanged   func(contract.RoleChanged)

	// running 记录正在跑分离的会话，保证「同一会话同时只有一个分离任务」
	// （与转写任务的并发上限 1 同构，避免两次 Run 交叉写 speaker_id）。
	runMu   sync.Mutex
	running map[string]struct{}
}

// 默认参数。
const (
	// defaultMinSpeechMs 过短片段门槛：1.2s（ARCH §4.4）。
	// 更短的片段声纹不可靠，硬提会把噪声聚成一个「假说话人」。
	defaultMinSpeechMs = 1200
	// maxRoleNameRunes 与 storage.Role.Name 的 size:64 对齐。
	maxRoleNameRunes = 64
	// maxColorTokenRunes 与 storage.Role.ColorToken 的 size:32 对齐。
	maxColorTokenRunes = 32
	// roleColorCount 角色配色令牌轮转周期（前端 --fd-role-1..8）。
	roleColorCount = 8
)

// New 构造说话人领域对象。
func New(opts Options) (*Repo, error) {
	if opts.Holder == nil {
		return nil, errors.New("speaker: holder is required")
	}
	r := &Repo{
		holder:           opts.Holder,
		now:              opts.Now,
		extractor:        opts.Extractor,
		audio:            opts.Audio,
		maxSpeakersFn:    opts.MaxSpeakers,
		minSpeechMs:      opts.MinSpeechMs,
		clusterThreshold: opts.ClusterThreshold,
		mergeThreshold:   opts.MergeThreshold,
		onRolesChanged:   opts.OnRolesChanged,
		running:          make(map[string]struct{}),
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.minSpeechMs <= 0 {
		r.minSpeechMs = defaultMinSpeechMs
	}
	if r.clusterThreshold <= 0 {
		r.clusterThreshold = DefaultClusterThreshold
	}
	if r.mergeThreshold <= 0 {
		r.mergeThreshold = DefaultMergeThreshold
	}
	return r, nil
}

// HasExtractor 报告声纹能力是否可用（前端据此决定「可分离」按钮是否置灰）。
func (r *Repo) HasExtractor() bool { return r.extractor != nil }

// db 取当前活跃连接（禁止缓存 *storage.DB：切路径会换句柄）。
func (r *Repo) db(ctx context.Context) *gorm.DB { return r.holder.Current().GORM.WithContext(ctx) }

// nowMs 返回当前毫秒时间戳。
func (r *Repo) nowMs() int64 { return r.now().UnixMilli() }

// maxSpeakers 读取会话内说话人上限；失败按「不限制」处理并只记元数据日志。
//
// 刻意不把读取失败变成错误：上限是「约束」而不是「数据」，
// 读不到设置时退化成不限制，比让整次分离失败更符合直觉。
func (r *Repo) maxSpeakers(ctx context.Context) int {
	if r.maxSpeakersFn != nil {
		n, err := r.maxSpeakersFn(ctx)
		if err != nil {
			return 0
		}
		return clampMaxSpeakers(n)
	}
	var row storage.AppSettings
	if err := r.db(ctx).First(&row, "id = ?", 1).Error; err != nil {
		return 0
	}
	return clampMaxSpeakers(row.MaxSpeakers)
}

// clampMaxSpeakers 把越界值收敛到合法范围。
//
// API-7.6 只接受 0 或 2–8；读到 1 或 99（旧库 / 手工改库）时按「不限制」处理，
// 而不是让聚类进入「每句一个新说话人」或「所有人都是一人」的极端。
func clampMaxSpeakers(n int) int {
	if n < 2 || n > 8 {
		return 0
	}
	return n
}

// emitRolesChanged 把角色变更载荷交给装配层的回调。
func (r *Repo) emitRolesChanged(role storage.Role) {
	if r.onRolesChanged == nil {
		return
	}
	r.onRolesChanged(contract.RoleChanged{
		RoleID:     role.ID,
		Name:       role.Name,
		ColorToken: role.ColorToken,
	})
}

// —— ID 生成 ——

// crockford 是 Crockford Base32 字母表（去掉 I/L/O/U，避免人工抄写歧义）。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newSpeakerID 生成声纹身份 ID：`sp_` + 26 位 ULID（TBL-05，size:32）。
func newSpeakerID(t time.Time) string { return "sp_" + newULID(t) }

// newRoleID 生成角色 ID：`rl_` + 26 位 ULID（TBL-07）。
func newRoleID(t time.Time) string { return "rl_" + newULID(t) }

// newULID 生成 26 位 ULID 布局字符串（前 10 位毫秒时间戳 + 后 16 位随机）。
//
// 与 internal/recording/ids.go 的实现同构（那边是包内私有、且本域不允许改它），
// 因此这里保留一份最小实现：只依赖标准库，不新增第三方依赖。
func newULID(t time.Time) string {
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		binary.BigEndian.PutUint64(entropy[2:], uint64(t.UnixNano()))
	}
	var out [26]byte
	ms := uint64(t.UnixMilli())
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&0x1f]
		ms >>= 5
	}
	var acc uint64
	var bits uint
	idx := 10
	for _, b := range entropy {
		acc = acc<<8 | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(acc>>bits)&0x1f]
			idx++
		}
	}
	return string(out[:])
}

// —— 错误构造 ——

// errInvalid 构造参数错误（不写任何业务内容到 Detail）。
func errInvalid(format string, args ...any) *apierr.Err {
	return apierr.New(apierr.CodeInvalidArgument).WithDetail(format, args...)
}

// errNotFound 构造「找不到」错误。
func errNotFound(format string, args ...any) *apierr.Err {
	return apierr.New(apierr.CodeNotFound).WithDetail(format, args...)
}

// wrapWrite 把数据库错误映射成 ERR_DB_WRITE。
//
// 返回值刻意是 `error` 而不是 `*apierr.Err`：`*apierr.Err` 的 nil 装进 error 接口后
// **不等于 nil**（typed-nil），调用方 `if err != nil` 会把成功当成失败。
// （docs/DEV-ISSUES-S7.md S7-04 记录了这个坑。）
func wrapWrite(err error) error {
	if err == nil {
		return nil
	}
	return apierr.Wrap(err, apierr.CodeDBWrite)
}

// trimID 归一化外部传入的 ID。
func trimID(s string) string { return strings.TrimSpace(s) }

// runKey 归一化会话 key。
func runKey(sessionID string) string { return fmt.Sprintf("session:%s", sessionID) }
