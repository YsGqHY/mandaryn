package transcriptsvc

import (
	"context"
	"errors"
	"testing"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/asr"
	"vip.oberon.mandaryn/internal/contract"
)

// warmableEngine 模拟 asr.Router 的「健康 + 预热」两面：Warm 之后状态切到 warmTo。
type warmableEngine struct {
	fakeEngine
	state   string
	err     *apierr.Err
	warms   int
	warmTo  string
	warmErr *apierr.Err
}

func (w *warmableEngine) Health(context.Context) asr.HealthStatus {
	return asr.HealthStatus{State: w.state, Err: w.err}
}

func (w *warmableEngine) Warm(context.Context) error {
	w.warms++
	w.state, w.err = w.warmTo, w.warmErr
	if w.warmErr != nil {
		return w.warmErr
	}
	return nil
}

// TestRequireEngineWarmsWhenNotReady：启动时未就绪（例如模型是后来才下载的），
// 发起转写时补一次预热即可开始，不必重启应用或来回切换模型来源。
func TestRequireEngineWarmsWhenNotReady(t *testing.T) {
	eng := &warmableEngine{state: contract.EngineStateLoading, warmTo: contract.EngineStateReady}
	s := &Service{engine: eng}
	if err := s.requireEngine(context.Background()); err != nil {
		t.Fatalf("预热成功后应放行，实际 %v", err)
	}
	if eng.warms != 1 {
		t.Fatalf("应补预热 1 次，实际 %d", eng.warms)
	}
}

// TestRequireEngineSkipsWarmWhenReady：已就绪不重复加载模型。
func TestRequireEngineSkipsWarmWhenReady(t *testing.T) {
	eng := &warmableEngine{state: contract.EngineStateReady}
	s := &Service{engine: eng}
	if err := s.requireEngine(context.Background()); err != nil {
		t.Fatalf("已就绪应放行，实际 %v", err)
	}
	if eng.warms != 0 {
		t.Fatalf("已就绪不应再预热，实际 %d 次", eng.warms)
	}
}

// TestRequireEngineSurfacesModelNotFound：没装模型时透出 ERR_MODEL_NOT_FOUND
// （前端据此提示「去模型页下载」），而不是「请稍后重试」（G-E2E-03）。
func TestRequireEngineSurfacesModelNotFound(t *testing.T) {
	missing := apierr.New(apierr.CodeModelNotFound).WithDetail("未安装离线模型包")
	eng := &warmableEngine{
		state:   contract.EngineStateFailed,
		err:     missing,
		warmTo:  contract.EngineStateFailed,
		warmErr: missing,
	}
	s := &Service{engine: eng}
	err := s.requireEngine(context.Background())
	if !apierr.HasCode(err, apierr.CodeModelNotFound) {
		t.Fatalf("应返回 ERR_MODEL_NOT_FOUND，实际 %v", err)
	}
	if got := apierr.From(err, "").Code; got != apierr.CodeModelNotFound {
		t.Fatalf("外层码应为 ERR_MODEL_NOT_FOUND（前端按外层码取文案），实际 %s", got)
	}
}

// TestRequireEngineOtherFailureStaysNotReady：其它加载失败仍是 ERR_ENGINE_NOT_READY，根因保留。
func TestRequireEngineOtherFailureStaysNotReady(t *testing.T) {
	loadErr := apierr.New(apierr.CodeEngineLoad)
	eng := &warmableEngine{
		state:   contract.EngineStateFailed,
		err:     loadErr,
		warmTo:  contract.EngineStateFailed,
		warmErr: loadErr,
	}
	s := &Service{engine: eng}
	err := s.requireEngine(context.Background())
	if got := apierr.From(err, "").Code; got != apierr.CodeEngineNotReady {
		t.Fatalf("外层码应为 ERR_ENGINE_NOT_READY，实际 %s", got)
	}
	if !errors.Is(err, apierr.New(apierr.CodeEngineLoad)) {
		t.Fatalf("根因 ERR_ENGINE_LOAD 应保留在错误链里，实际 %v", err)
	}
}
