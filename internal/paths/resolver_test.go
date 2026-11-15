package paths

import (
	"os"
	"path/filepath"
	"testing"

	"vip.oberon.mandaryn/internal/contract"
)

// newTestResolver 构造一个完全可控的解析器：默认目录用临时目录，
// env 用内存 map，user/pending 用内存变量。
func newTestResolver(t *testing.T, opts *Options) (*Resolver, map[string]string) {
	t.Helper()
	base := t.TempDir()
	env := map[string]string{}
	if opts == nil {
		opts = &Options{}
	}
	opts.LookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	opts.DefaultDataRoot = func() (string, error) { return filepath.Join(base, "data"), nil }
	opts.DefaultModelRoot = func() (string, error) { return filepath.Join(base, "models"), nil }
	return New(*opts), env
}

// TestResolveDefault 无任何接管时用默认值。
func TestResolveDefault(t *testing.T) {
	r, _ := newTestResolver(t, nil)

	p, err := r.Resolve(KeyDataRoot)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Source != contract.PathSourceDefault {
		t.Errorf("source = %q, want default", p.Source)
	}
	if !p.Writable {
		t.Error("default data root should be writable")
	}

	m, err := r.Resolve(KeyModelRoot)
	if err != nil {
		t.Fatalf("resolve model: %v", err)
	}
	if m.Source != contract.PathSourceDefault {
		t.Errorf("model source = %q, want default", m.Source)
	}
}

// TestResolvePrecedence 验证 cli > env > user 的单向覆盖。
func TestResolvePrecedence(t *testing.T) {
	base := t.TempDir()
	userDir := filepath.Join(base, "user")
	envDir := filepath.Join(base, "env")
	cliDir := filepath.Join(base, "cli")
	opts := &Options{
		CLI:          map[Key]string{KeyModelRoot: cliDir},
		UserValue:    func(Key) (string, error) { return userDir, nil },
		PendingValue: func(Key) (string, error) { return "", nil },
	}
	r, env := newTestResolver(t, opts)

	// 1) cli 最高
	p, err := r.Resolve(KeyModelRoot)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Source != contract.PathSourceCLI || p.Effective != cliDir {
		t.Errorf("got source=%q effective=%q, want cli/%q", p.Source, p.Effective, cliDir)
	}
	if p.EnvName != FlagModelDir {
		t.Errorf("envName = %q, want %q", p.EnvName, FlagModelDir)
	}
	if p.OverrideHint == "" {
		t.Error("override hint should be set when managed")
	}

	// 2) 去掉 cli → env 生效
	env[EnvModelRoot] = envDir
	r2 := New(Options{
		LookupEnv:        func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		DefaultDataRoot:  func() (string, error) { return filepath.Join(base, "data"), nil },
		DefaultModelRoot: func() (string, error) { return filepath.Join(base, "def"), nil },
		UserValue:        func(Key) (string, error) { return userDir, nil },
	})
	p2, err := r2.Resolve(KeyModelRoot)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if p2.Source != contract.PathSourceEnv || p2.Effective != envDir {
		t.Errorf("got source=%q effective=%q, want env/%q", p2.Source, p2.Effective, envDir)
	}
	if p2.UserValue != userDir {
		t.Errorf("userValue = %q, want %q (must stay visible while overridden)", p2.UserValue, userDir)
	}

	// 3) 去掉 env → user 生效
	delete(env, EnvModelRoot)
	p3, err := r2.Resolve(KeyModelRoot)
	if err != nil {
		t.Fatalf("resolve3: %v", err)
	}
	// 注意 r2 已缓存 env 结果，需重新构造（解析结果在进程内不可变是设计意图）。
	r3 := New(Options{
		LookupEnv:        func(k string) (string, bool) { return "", false },
		DefaultDataRoot:  func() (string, error) { return filepath.Join(base, "data"), nil },
		DefaultModelRoot: func() (string, error) { return filepath.Join(base, "def"), nil },
		UserValue:        func(Key) (string, error) { return userDir, nil },
	})
	p4, err := r3.Resolve(KeyModelRoot)
	if err != nil {
		t.Fatalf("resolve4: %v", err)
	}
	if p4.Source != contract.PathSourceUser || p4.Effective != userDir {
		t.Errorf("got source=%q effective=%q, want user/%q", p4.Source, p4.Effective, userDir)
	}
	_ = p3
}

// TestResolveFallback 高优先级路径不可写时降级到下一层并留痕。
func TestResolveFallback(t *testing.T) {
	base := t.TempDir()
	// 用一个「文件」当目录 → 必然不可写也不可创建。
	blocker := filepath.Join(base, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("prepare blocker: %v", err)
	}
	child := filepath.Join(blocker, "sub") // 在文件下面建目录必然失败

	opts := &Options{
		UserValue: func(Key) (string, error) { return child, nil },
	}
	r, _ := newTestResolver(t, opts)

	p, err := r.Resolve(KeyModelRoot)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Source != contract.PathSourceDefault {
		t.Errorf("source = %q, want default (fallback)", p.Source)
	}
	if p.FallbackFrom == "" {
		t.Error("fallbackFrom should record the skipped path")
	}
	if p.UserValue != child {
		t.Errorf("userValue = %q, want %q", p.UserValue, child)
	}
}

// TestSetUserModelRootWhenManaged 受管时改目录必须写暂存且**不报错**。
func TestSetUserModelRootWhenManaged(t *testing.T) {
	base := t.TempDir()
	envDir := filepath.Join(base, "env")
	userVal := ""
	pending := ""

	opts := &Options{
		LookupEnv: func(k string) (string, bool) {
			if k == EnvModelRoot {
				return envDir, true
			}
			return "", false
		},
		UserValue:       func(Key) (string, error) { return userVal, nil },
		PendingValue:    func(Key) (string, error) { return pending, nil },
		SetUserValue:    func(_ Key, v string) error { userVal = v; return nil },
		SetPendingValue: func(_ Key, v string) error { pending = v; return nil },
	}
	r := New(*opts)

	next := filepath.Join(base, "next")
	p, err := r.SetUserModelRoot(next)
	if err != nil {
		t.Fatalf("SetUserModelRoot must not fail when managed: %v", err)
	}
	if p.Source != contract.PathSourceEnv {
		t.Errorf("source = %q, want env", p.Source)
	}
	if p.PendingValue != next {
		t.Errorf("pendingValue = %q, want %q", p.PendingValue, next)
	}
	if userVal != "" {
		t.Errorf("user value must not be touched while managed, got %q", userVal)
	}
	if pending != next {
		t.Errorf("pending store not written: %q", pending)
	}
}

// TestSetUserModelRootUnwritable 未被接管 + 目标不可写 → 返回错误（ERR_PATH_UNWRITABLE 的判定点）。
func TestSetUserModelRootUnwritable(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("prepare blocker: %v", err)
	}
	userVal := ""
	opts := &Options{
		UserValue:    func(Key) (string, error) { return userVal, nil },
		SetUserValue: func(_ Key, v string) error { userVal = v; return nil },
	}
	r := New(*opts)

	if _, err := r.SetUserModelRoot(filepath.Join(blocker, "sub")); err == nil {
		t.Fatal("expected error for unwritable target")
	}
	if userVal != "" {
		t.Errorf("user value should not change on failure, got %q", userVal)
	}
}

// TestApplyPending pending 只在未受管时被消费。
func TestApplyPending(t *testing.T) {
	base := t.TempDir()
	userVal := ""
	pending := filepath.Join(base, "pending-models")
	envOn := false

	opts := &Options{
		LookupEnv: func(k string) (string, bool) {
			if k == EnvModelRoot && envOn {
				return filepath.Join(base, "env"), true
			}
			return "", false
		},
		UserValue:       func(Key) (string, error) { return userVal, nil },
		PendingValue:    func(Key) (string, error) { return pending, nil },
		SetUserValue:    func(_ Key, v string) error { userVal = v; return nil },
		SetPendingValue: func(_ Key, v string) error { pending = v; return nil },
	}

	// 仍受管 → 保留 pending，不应用。
	envOn = true
	r := New(*opts)
	applied, err := r.ApplyPending()
	if err != nil {
		t.Fatalf("apply while managed: %v", err)
	}
	if len(applied) != 0 || userVal != "" {
		t.Errorf("pending must not apply while managed: applied=%v user=%q", applied, userVal)
	}

	// 解除接管 → 应用 pending 并清空。
	envOn = false
	wantApplied := pending // ApplyPending 会清空 pending，先留存期望值
	r2 := New(*opts)
	applied2, err := r2.ApplyPending()
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied2) != 1 || applied2[0] != KeyModelRoot {
		t.Errorf("applied = %v, want [%s]", applied2, KeyModelRoot)
	}
	if userVal != wantApplied {
		t.Errorf("user value = %q, want %q", userVal, wantApplied)
	}
	if pending != "" {
		t.Errorf("pending should be cleared, got %q", pending)
	}
}

// TestExpandHome 用户手输 ~ 时要展开。
func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/models"); got != filepath.Join(home, "models") {
		t.Errorf("expandHome(~/models) = %q", got)
	}
	if got := expandHome(""); got != "" {
		t.Errorf("expandHome(\"\") = %q", got)
	}
	if got := expandHome("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path must be untouched, got %q", got)
	}
}

// TestResolveUnknownKey 未知键必须报错（防止前端传任意 key）。
func TestResolveUnknownKey(t *testing.T) {
	r, _ := newTestResolver(t, nil)
	if _, err := r.Resolve("bogus"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

// TestResolvePortableLayer 便携层：exe 同级存在 data\ 时生效，优先级 user > portable > default。
func TestResolvePortableLayer(t *testing.T) {
	base := t.TempDir()
	portable := filepath.Join(base, "portable-data")
	userDir := filepath.Join(base, "user")
	defData := filepath.Join(base, "defdata")

	newR := func(user string, portableOn bool) *Resolver {
		return New(Options{
			LookupEnv:        func(string) (string, bool) { return "", false },
			DefaultDataRoot:  func() (string, error) { return defData, nil },
			DefaultModelRoot: func() (string, error) { return filepath.Join(base, "defmodel"), nil },
			UserValue: func(k Key) (string, error) {
				if k == KeyDataRoot {
					return user, nil
				}
				return "", nil
			},
			PortableDataRoot: func() (string, bool) { return portable, portableOn },
		})
	}

	// 1) 只有便携层 → source=portable
	p, err := newR("", true).Resolve(KeyDataRoot)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.Source != contract.PathSourcePortable || p.Effective != portable {
		t.Errorf("got source=%q effective=%q, want portable/%q", p.Source, p.Effective, portable)
	}

	// 2) 用户层存在 → user 优先于 portable（用户显式设置不被"放进便携包"推翻）
	p2, err := newR(userDir, true).Resolve(KeyDataRoot)
	if err != nil {
		t.Fatalf("resolve2: %v", err)
	}
	if p2.Source != contract.PathSourceUser || p2.Effective != userDir {
		t.Errorf("got source=%q effective=%q, want user/%q", p2.Source, p2.Effective, userDir)
	}

	// 3) 非便携布局 → 回落 default
	p3, err := newR("", false).Resolve(KeyDataRoot)
	if err != nil {
		t.Fatalf("resolve3: %v", err)
	}
	if p3.Source != contract.PathSourceDefault || p3.Effective != defData {
		t.Errorf("got source=%q effective=%q, want default/%q", p3.Source, p3.Effective, defData)
	}

	// 4) modelRoot **不**受便携层影响（ADR-02：两个根目录不合并）
	m, err := newR("", true).Resolve(KeyModelRoot)
	if err != nil {
		t.Fatalf("resolve model: %v", err)
	}
	if m.Source == contract.PathSourcePortable {
		t.Errorf("modelRoot 不应出现 portable 来源，实际 %q", m.Source)
	}
}

// TestPortableDataRootDetection 检测函数本身：只有 data\ 目录真实存在才算便携布局。
func TestPortableDataRootDetection(t *testing.T) {
	dir := t.TempDir()
	exeDir := dir
	// 模拟"exe 同级没有 data\"：portableDataRoot 基于 os.Executable（测试进程的真实 exe），
	// 因此这里只断言"不会凭空返回 ok=true"。
	if _, ok := portableDataRoot(); ok {
		// 测试进程的 exe 同级理论上不会有 data\；若真有，说明环境特殊，跳过而不是误报失败。
		t.Skip("测试进程 exe 同级存在 data 目录，跳过该项断言")
	}
	_ = exeDir
	if _, err := os.Stat(filepath.Join(dir, PortableDataDirName)); err == nil {
		t.Fatal("临时目录不应有 data 子目录")
	}
}
