// Package paths 是 dataRoot / modelRoot 的**单点裁决器**（ADR-02 / DOC-02 §4.8）。
//
// 为什么必须集中：cryptox 要往 dataRoot 放主密钥、logx 要往 dataRoot/logs 写日志、
// storage 要打开 dataRoot/mandaryn.db。三者若各自读环境变量，一旦某层不可用而
// 回退时机不同，就会出现「数据库在 A、密钥在 B、日志在 C」——数据看似丢失且无法自愈。
// 因此：**一次解析、三处复用**。
//
// 优先级固定为 cli > env > user > default，单向覆盖，不做合并。
// 解析结果带 `source`，前端只按 source 决定 UI 状态，不自行推断优先级。
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"vip.oberon.mandaryn/internal/contract"
)

// Key 是路径键。
type Key = string

// 两个根目录（DOC-04 §1.1）。二者的迁移语义不同，**不得合并**。
const (
	KeyDataRoot  Key = contract.PathKeyDataRoot
	KeyModelRoot Key = contract.PathKeyModelRoot
)

// 环境变量名：接管 dataRoot / modelRoot。
const (
	EnvDataRoot  = "MANDARYN_DATA_DIR"
	EnvModelRoot = "MANDARYN_MODEL_ROOT"
)

// 命令行旗标名（由 main/app 解析后注入 Options.CLI）。
const (
	FlagDataDir  = "--data-dir"
	FlagModelDir = "--model-dir"
)

// Options 是 Resolver 的依赖注入点。
//
// 用函数字段而不是具体类型，是为了让 internal/paths 不依赖 storage（否则会成环：
// storage 需要 Resolver 来定位数据库文件）与 Wails。测试也可直接替换这些函数。
type Options struct {
	// CLI 已解析的命令行值（键为 KeyDataRoot / KeyModelRoot）。空表示未指定。
	CLI map[Key]string

	// LookupEnv 默认 os.LookupEnv，测试可替换。
	LookupEnv func(string) (string, bool)

	// DefaultDataRoot / DefaultModelRoot 返回**平台默认**路径（不含任何接管）。
	DefaultDataRoot  func() (string, error)
	DefaultModelRoot func() (string, error)

	// PortableDataRoot 返回「便携模式」的数据目录（exe 同级 data\），ok=false 表示不是便携布局。
	//
	// 只在 dataRoot 的分层里参与（优先级 user > portable > default）。nil 时用默认实现
	// （见 portableDataRoot），这样装配层不必显式接线，测试可替换成固定行为。
	PortableDataRoot func() (string, bool)

	// UserValue 返回「用户设置层」保存的值（dataRoot: storage.json；modelRoot: app_settings.model_root）。
	// 返回空串表示用户没设过。可为 nil（视作未设）。
	UserValue func(Key) (string, error)

	// PendingValue 返回「受管期间的暂存值」（仅 modelRoot 有）。
	PendingValue func(Key) (string, error)

	// SetUserValue / SetPendingValue 写入用户层 / 暂存层。
	SetUserValue    func(Key, string) error
	SetPendingValue func(Key, string) error
}

// Resolver 解析并缓存路径。进程生命周期内解析结果不变（唯一例外：受管时写 pending）。
type Resolver struct {
	opts Options

	mu    sync.RWMutex
	cache map[Key]contract.ResolvedPath
}

// New 构造解析器；nil 的函数字段会填入默认实现。
func New(opts Options) *Resolver {
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.DefaultDataRoot == nil {
		opts.DefaultDataRoot = defaultDataRoot
	}
	if opts.DefaultModelRoot == nil {
		opts.DefaultModelRoot = defaultModelRoot
	}
	if opts.PortableDataRoot == nil {
		opts.PortableDataRoot = portableDataRoot
	}
	return &Resolver{opts: opts, cache: make(map[Key]contract.ResolvedPath, 2)}
}

// Resolve 返回指定键的解析结果（首次调用后缓存）。
func (r *Resolver) Resolve(key Key) (contract.ResolvedPath, error) {
	r.mu.RLock()
	if p, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return p, nil
	}
	r.mu.RUnlock()

	p, err := r.resolve(key)
	if err != nil {
		return contract.ResolvedPath{}, err
	}

	r.mu.Lock()
	r.cache[key] = p
	r.mu.Unlock()
	return p, nil
}

// Effective 只要生效路径，便于启动早期调用。
func (r *Resolver) Effective(key Key) (string, error) {
	p, err := r.Resolve(key)
	if err != nil {
		return "", err
	}
	return p.Effective, nil
}

// HasDataRootOverride also reports unavailable overrides: a failed probe must not
// consume a migration request while deployment still owns the path.
func (r *Resolver) HasDataRootOverride() bool {
	if strings.TrimSpace(r.opts.CLI[KeyDataRoot]) != "" {
		return true
	}
	value, _ := r.opts.LookupEnv(EnvDataRoot)
	return strings.TrimSpace(value) != ""
}

// Invalidate 丢弃缓存（用户改了目录后调用；实际生效仍要求重启，见 DOC-02 §4.8）。
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	r.cache = make(map[Key]contract.ResolvedPath, 2)
	r.mu.Unlock()
}

func (r *Resolver) resolve(key Key) (contract.ResolvedPath, error) {
	if key != KeyDataRoot && key != KeyModelRoot {
		return contract.ResolvedPath{}, fmt.Errorf("paths: unknown key %q", key)
	}

	userValue := r.readUser(key)
	pending := r.readPending(key)

	// 逐层取值：cli → env → user → default（单向覆盖）。
	type layer struct {
		source contract.ResolvedPath // 临时载体，Final 阶段只取 source/effective/name
		name   string
		value  string
	}
	var layers []layer

	if v := strings.TrimSpace(r.opts.CLI[key]); v != "" {
		layers = append(layers, layer{
			source: contract.ResolvedPath{Source: contract.PathSourceCLI},
			name:   flagName(key), value: v,
		})
	}
	envName := envName(key)
	if v, ok := r.opts.LookupEnv(envName); ok && strings.TrimSpace(v) != "" {
		layers = append(layers, layer{
			source: contract.ResolvedPath{Source: contract.PathSourceEnv},
			name:   envName, value: strings.TrimSpace(v),
		})
	}
	if userValue != "" {
		layers = append(layers, layer{
			source: contract.ResolvedPath{Source: contract.PathSourceUser},
			name:   "", value: userValue,
		})
	}
	// 便携层：exe 同级存在 data\ 目录即采用（优先级低于 user、高于 default）。
	//
	// 只对 dataRoot 生效：modelRoot 的默认位置是 ~/.mandaryn（模型可能几十 GB、
	// 可被迁移到别的磁盘），把它也塞进便携目录会让"把 exe 拷到 U 盘"意外搬家模型
	// （ADR-02：两个根目录语义不同，不合并）。
	if key == KeyDataRoot {
		if v, ok := r.opts.PortableDataRoot(); ok && strings.TrimSpace(v) != "" {
			layers = append(layers, layer{
				source: contract.ResolvedPath{Source: contract.PathSourcePortable},
				name:   "", value: v,
			})
		}
	}
	// 默认层是**兜底**，不是前置条件：它解析失败（例如 %AppData% 缺失、
	// HOME 未定义）时只应把默认层从候选里剔除，不能让已经拿到 cli/env/user
	// 值的解析整体失败——否则一次环境变量缺失就会否决用户显式指定的路径。
	//
	// 只有在「最终没有任何可用层」时才把默认层的错误报出去。
	var defErr error
	def, err := r.defaultFor(key)
	if err != nil {
		defErr = err
	} else {
		layers = append(layers, layer{
			source: contract.ResolvedPath{Source: contract.PathSourceDefault},
			name:   "", value: def,
		})
	}

	// 只剩默认层且它解析失败 → 此时确实没有任何候选，把默认层的原始错误报出去。
	if len(layers) == 0 {
		if defErr != nil {
			return contract.ResolvedPath{}, defErr
		}
		return contract.ResolvedPath{}, fmt.Errorf("paths: %s 没有任何可用的路径来源", key)
	}

	// 选层：第一个「可写」的层生效；不可写的非最后一层记录 fallbackFrom 后继续。
	out := contract.ResolvedPath{Key: key, UserValue: userValue}
	for i, l := range layers {
		abs, absErr := filepath.Abs(expandHome(l.value))
		if absErr != nil {
			abs = l.value
		}
		writable := probeWritable(abs) == nil
		last := i == len(layers)-1
		if !writable && !last {
			// 高优先级不可用 → 降级到下一层，并留下可见痕迹（绝不静默失败）。
			if out.FallbackFrom == "" {
				out.FallbackFrom = abs
			}
			continue
		}
		out.Effective = abs
		out.Source = l.source.Source
		out.EnvName = l.name
		out.Writable = writable
		break
	}

	// 受 env/cli 接管时，附带用户层与暂存层的可见性信息。
	if out.Source == contract.PathSourceEnv || out.Source == contract.PathSourceCLI {
		out.PendingValue = pending
		out.OverrideHint = overrideHint(key, out.Source)
	}
	return out, nil
}

func (r *Resolver) readUser(key Key) string {
	if r.opts.UserValue == nil || key != KeyModelRoot && key != KeyDataRoot {
		return ""
	}
	v, err := r.opts.UserValue(key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func (r *Resolver) readPending(key Key) string {
	if r.opts.PendingValue == nil || key != KeyModelRoot {
		return ""
	}
	v, err := r.opts.PendingValue(key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func (r *Resolver) defaultFor(key Key) (string, error) {
	if key == KeyDataRoot {
		return r.opts.DefaultDataRoot()
	}
	return r.opts.DefaultModelRoot()
}

// SetUserModelRoot 写入 modelRoot 用户层值（FR-07-2 / FR-07-8）。
//
// 关键语义（PRD FR-07-7 要求「不静默失败，也不假装成功」）：
//   - 未被接管 → 写入用户值，返回 source=user；
//   - 已被 env/cli 接管 → **不报错**，写入 pending 并原样返回 source=env|cli
//     与 pendingValue，让 UI 能同时展示「生效值」和「待解除后生效」。
func (r *Resolver) SetUserModelRoot(value string) (contract.ResolvedPath, error) {
	if r.opts.UserValue == nil {
		return contract.ResolvedPath{}, fmt.Errorf("paths: user store unavailable")
	}
	trimmed := strings.TrimSpace(expandHome(value))

	cur, err := r.Resolve(KeyModelRoot)
	if err != nil {
		return contract.ResolvedPath{}, err
	}
	managed := cur.Source == contract.PathSourceEnv || cur.Source == contract.PathSourceCLI

	if managed {
		if r.opts.SetPendingValue == nil {
			return contract.ResolvedPath{}, fmt.Errorf("paths: pending store unavailable")
		}
		if err := r.opts.SetPendingValue(KeyModelRoot, trimmed); err != nil {
			return contract.ResolvedPath{}, err
		}
		cur.PendingValue = trimmed
		return cur, nil
	}

	// 未被接管：先校验目标可写，再落库（ERR_PATH_UNWRITABLE 的判定点）。
	if trimmed != "" {
		abs, absErr := filepath.Abs(trimmed)
		if absErr == nil {
			trimmed = abs
		}
		if err := probeWritable(trimmed); err != nil {
			return cur, err
		}
	}
	if err := r.opts.SetUserValue(KeyModelRoot, trimmed); err != nil {
		return contract.ResolvedPath{}, err
	}
	r.Invalidate()
	return r.Resolve(KeyModelRoot)
}

// ClearUserModelRoot 清空用户值（受管时只清暂存值），回落 default。
func (r *Resolver) ClearUserModelRoot() (contract.ResolvedPath, error) {
	cur, err := r.Resolve(KeyModelRoot)
	if err != nil {
		return contract.ResolvedPath{}, err
	}
	if cur.Source == contract.PathSourceEnv || cur.Source == contract.PathSourceCLI {
		if r.opts.SetPendingValue != nil {
			if err := r.opts.SetPendingValue(KeyModelRoot, ""); err != nil {
				return contract.ResolvedPath{}, err
			}
		}
		cur.PendingValue = ""
		return cur, nil
	}
	if r.opts.SetUserValue != nil {
		if err := r.opts.SetUserValue(KeyModelRoot, ""); err != nil {
			return contract.ResolvedPath{}, err
		}
	}
	r.Invalidate()
	return r.Resolve(KeyModelRoot)
}

// ApplyPending 在启动时消费暂存值：只有当用户层**当前生效**（未被接管）时，
// 把 pending 写进用户值并清空 pending。返回实际应用的键。
func (r *Resolver) ApplyPending() ([]string, error) {
	if r.opts.PendingValue == nil || r.opts.SetUserValue == nil || r.opts.SetPendingValue == nil {
		return nil, nil
	}
	pending := r.readPending(KeyModelRoot)
	if pending == "" {
		return nil, nil
	}
	cur, err := r.Resolve(KeyModelRoot)
	if err != nil {
		return nil, err
	}
	if cur.Source == contract.PathSourceEnv || cur.Source == contract.PathSourceCLI {
		// 仍被接管：保留暂存值等下次启动。
		return nil, nil
	}
	if err := r.opts.SetUserValue(KeyModelRoot, pending); err != nil {
		return nil, err
	}
	if err := r.opts.SetPendingValue(KeyModelRoot, ""); err != nil {
		return nil, err
	}
	r.Invalidate()
	return []string{KeyModelRoot}, nil
}

// —— 平台默认路径 ——

// defaultDataRoot 返回平台默认数据根（含 AppDirName，不含任何接管层）。
func defaultDataRoot() (string, error) {
	switch runtime.GOOS {
	case "windows":
		if v := os.Getenv("AppData"); v != "" {
			return filepath.Join(v, AppDirName), nil
		}
		base, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("paths: cannot resolve user config dir: %w", err)
		}
		return filepath.Join(base, AppDirName), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("paths: cannot resolve home dir: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", AppDirName), nil
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, AppDirName), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("paths: cannot resolve home dir: %w", err)
		}
		return filepath.Join(home, ".local", "share", AppDirName), nil
	}
}

// defaultModelRoot 返回默认模型根 `~/.mandaryn`（FR-07-1）。
//
// 刻意**不**跟随 XDG/AppData：模型体积大，用户常放到外置盘，平台约定会让人猜不到；
// 固定 `~/.mandaryn` 与文档一致，且便于文档与支持给出确定路径。
func defaultModelRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("paths: cannot resolve home dir: %w", err)
	}
	return filepath.Join(home, "."+ModelDirName), nil
}

// portableDataRoot 检测便携布局：exe 同级存在 data\ 目录时返回它（ok=true）。
//
// 便携包的形态：解压后 exe 旁边有一个 data\ 目录。这样"把 exe 拷到 U 盘"就自带数据，
// 不写用户目录（也便于多副本并存）。只认**已存在**的目录：不去创建它，否则任何安装版
// 都不会有该目录，行为与现在一致；反过来若我们主动创建，就会把普通安装误判成便携。
func portableDataRoot() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	cand := filepath.Join(filepath.Dir(exe), PortableDataDirName)
	st, err := os.Stat(cand)
	if err != nil || !st.IsDir() {
		return "", false
	}
	return cand, true
}

// probeWritable 校验目录可创建且可写：建目录 → 写临时文件 → 删除。
func probeWritable(dir string) error {
	if dir == "" {
		return fmt.Errorf("paths: empty path")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("paths: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".mandaryn-write-test-*")
	if err != nil {
		return fmt.Errorf("paths: not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// expandHome 把前缀 ~ 展开为用户主目录（用户可能手输 ~/models）。
func expandHome(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func envName(key Key) string {
	if key == KeyDataRoot {
		return EnvDataRoot
	}
	return EnvModelRoot
}

func flagName(key Key) string {
	if key == KeyDataRoot {
		return FlagDataDir
	}
	return FlagModelDir
}

// overrideHint 生成「解除接管」的可复制命令（按平台区分 set / export）。
// dataRoot 只由 CLI/ENV 决定（DOC-03 §2.9.3），modelRoot 才提供解除提示。
func overrideHint(key Key, source string) string {
	if key != KeyModelRoot {
		return ""
	}
	if source == contract.PathSourceCLI {
		// 命令行优先级最高，提示「去掉启动参数」而不是「删掉变量」。
		return FlagModelDir
	}
	if runtime.GOOS == "windows" {
		return "set " + EnvModelRoot + "="
	}
	return "export " + EnvModelRoot + "="
}
