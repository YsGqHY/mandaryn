package recording

import (
	"errors"
	"io"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"vip.oberon.mandaryn/internal/capture"
	"vip.oberon.mandaryn/internal/storage"
)

// —— 测试替身 ——

// fakeChannel 是 capture.Channel 的合成实现。
//
// 为什么不用真设备：CI 与开发机上不能保证有回环设备，真设备的采样率、
// 抖动、权限都不可控，测试会变成 flaky。这里用「可编程样本生成器 + 固定延迟」
// 精确控制输入，才能断言「重采样比例」「混音结果」「静音判定」这些数值性质。
type fakeChannel struct {
	format capture.AudioFormat
	// fill 填充样本（长度已按声道对齐）。
	fill func(dst []float32)
	// delay 每次 ReadPCM 的模拟耗时（0 = 尽快返回，用于「音频比实时快」的测试）。
	delay time.Duration
	// failAfter 读多少次后返回 errBoom（模拟设备失效）。
	failAfter int64
	// failErr 失效时返回的错误。
	failErr error
	// pushErr 若为 true，把 failErr 推给 Err() 通道（模拟被动通知）。
	pushErr bool

	closed    chan struct{}
	closeOnce sync.Once
	errCh     chan error

	mu    sync.Mutex
	reads int64
}

var errBoom = errors.New("fake channel boom")

func newFakeChannel(format capture.AudioFormat, fill func(dst []float32)) *fakeChannel {
	return &fakeChannel{
		format: format,
		fill:   fill,
		closed: make(chan struct{}),
		errCh:  make(chan error, 1),
	}
}

func (c *fakeChannel) ReadPCM(dst []float32) (int, error) {
	select {
	case <-c.closed:
		return 0, io.EOF
	default:
	}
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-c.closed:
			return 0, io.EOF
		}
	}

	c.mu.Lock()
	c.reads++
	reads := c.reads
	c.mu.Unlock()

	if c.failAfter > 0 && reads > c.failAfter {
		err := c.failErr
		if err == nil {
			err = errBoom
		}
		if c.pushErr {
			select {
			case c.errCh <- err:
			default:
			}
		}
		return 0, err
	}

	// 返回的样本数必须是声道数的整数倍，否则混音会读越界。
	ch := c.format.Channels
	if ch <= 0 {
		ch = 1
	}
	n := len(dst) - len(dst)%ch
	if n <= 0 {
		return 0, nil
	}
	c.fill(dst[:n])
	return n, nil
}

func (c *fakeChannel) Format() capture.AudioFormat { return c.format }

func (c *fakeChannel) Err() <-chan error { return c.errCh }

func (c *fakeChannel) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// fillConst 恒定振幅（语音带内的直流近似，便于断言电平与 DC 增益）。
func fillConst(v float32) func(dst []float32) {
	return func(dst []float32) {
		for i := range dst {
			dst[i] = v
		}
	}
}

// fillZero 数字静音。
func fillZero() func(dst []float32) { return fillConst(0) }

// fillSine 生成正弦（左右声道不同相，用于验证混音后不抵消成 0）。
func fillSine(sampleRate int, channels int, hz float64, amp float32) func(dst []float32) {
	var phase float64
	step := 2 * 3.14159265358979 * hz / float64(sampleRate)
	return func(dst []float32) {
		for i := 0; i < len(dst); i += channels {
			v := amp * float32(math.Sin(phase))
			for c := 0; c < channels; c++ {
				if c < len(dst)-i {
					dst[i+c] = v
				}
			}
			phase += step
		}
	}
}

func sin(x float64) float64 { return math.Sin(x) }

// —— 测试装配 ——

// newTestStore 在临时目录建一个真实 SQLite 库并返回 Store。
//
// 用真库而不是内存假实现：录制域会踩 GORM 的零值更新、唯一索引、行数校验
// 这些只有真 SQLite 才暴露的问题（见 docs/DEV-ISSUES-S2.md S2-01）。
func newTestStore(t *testing.T) *GORMStore {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewGORMStore(storage.NewHolder(db))
}

// newTestRecorder 构造一个挂在临时库 + 临时 dataRoot 上的录制器。
func newTestRecorder(t *testing.T, opts func(*Options)) *Recorder {
	t.Helper()
	store := newTestStore(t)
	root := t.TempDir()

	base := Options{
		Store:    store,
		DataRoot: func() (string, error) { return root, nil },
		Now:      func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	}
	if opts != nil {
		opts(&base)
	}
	rec, err := New(base)
	if err != nil {
		t.Fatalf("构造录制器失败：%v", err)
	}
	return rec
}

// waitFor 轮询等待条件成立（避免用固定 sleep 造成的 flaky）。
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时（%s）：%s", timeout, what)
}
