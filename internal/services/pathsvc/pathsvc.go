// Package pathsvc 是路径解析的 Wails 薄壳（API-9.11 ~ API-9.15 / ADR-02）。
//
// 职责边界：只做类型转换、错误码映射与「打开目录 / 请求重启」这类平台动作；
// 优先级判定、暂存语义、可写性探测全部在 internal/paths（L2）里，本层不得重写。
package pathsvc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/paths"
	"vip.oberon.mandaryn/internal/utils/logx"
	"vip.oberon.mandaryn/internal/utils/procx"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Service 暴露路径解析结果与模型目录变更。
type Service struct {
	resolver *paths.Resolver
}

// New 构造服务。
func New(resolver *paths.Resolver) *Service {
	return &Service{resolver: resolver}
}

// GetResolvedPaths 返回 dataRoot / modelRoot 的生效值、来源层、用户值、
// 接管变量名与失效回退状态（前端只按 source 渲染，不自行推断优先级）。
func (s *Service) GetResolvedPaths(_ context.Context) ([]contract.ResolvedPath, error) {
	out := make([]contract.ResolvedPath, 0, 2)
	for _, key := range []string{paths.KeyDataRoot, paths.KeyModelRoot} {
		p, err := s.resolver.Resolve(key)
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeInvalidArgument)
		}
		out = append(out, p)
	}
	return out, nil
}

// SetModelRoot 设置模型目录（API-9.12）。
//
// 受 env/cli 接管时**不报错**：写入暂存值并原样返回（source 不变、pendingValue 非空），
// 让 UI 能同时展示「生效值」与「待解除后生效」（PRD FR-07-7/8）。
func (s *Service) SetModelRoot(_ context.Context, path string) (contract.ResolvedPath, error) {
	p, err := s.resolver.SetUserModelRoot(path)
	if err != nil {
		// 只有「未被接管 + 目标不可写」才返回 ERR_PATH_UNWRITABLE。
		if isPathError(err) {
			return p, apierr.New(apierr.CodePathUnwritable).WithCause(err)
		}
		return p, apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	logx.For("pathsvc").Info("model root user value updated",
		"source", p.Source, "effective", p.Effective, "pending", p.PendingValue != "")
	return p, nil
}

// ClearModelRoot 清除用户值与暂存值，回落默认（API-9.13）。
func (s *Service) ClearModelRoot(_ context.Context) (contract.ResolvedPath, error) {
	p, err := s.resolver.ClearUserModelRoot()
	if err != nil {
		return p, apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	return p, nil
}

// RevealPath 在系统文件管理器中打开路径（API-9.14）。
//
// 不存在则先创建（用户想「打开我刚设的目录」时目录可能尚未生成）。
// 走平台原生命令，**禁止**前端用 file:// 或浏览器能力。
func (s *Service) RevealPath(_ context.Context, key string) error {
	// logs 不是独立的根目录：它跟随 dataRoot（日志、库、密钥同根，ADR-02）。
	resolveKey := key
	if key == contract.PathKeyLogs {
		resolveKey = paths.KeyDataRoot
	}
	if resolveKey != paths.KeyDataRoot && resolveKey != paths.KeyModelRoot {
		return apierr.Newf(apierr.CodeInvalidArgument, "unknown path key: %s", key)
	}
	p, err := s.resolver.Resolve(resolveKey)
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	if p.Effective == "" {
		return apierr.New(apierr.CodeInvalidArgument)
	}
	if key == contract.PathKeyLogs {
		p.Effective = filepath.Join(p.Effective, paths.LogsDirName)
	}
	if err := os.MkdirAll(p.Effective, 0o755); err != nil {
		return apierr.New(apierr.CodePathUnwritable).WithCause(err)
	}

	var spec procx.Spec
	switch runtime.GOOS {
	case "windows":
		// explorer 打开目录；末尾不加 /select，保持「打开该目录」语义。
		spec = procx.Spec{Name: "explorer", Args: []string{filepath.Clean(p.Effective)}}
	case "darwin":
		spec = procx.Spec{Name: "open", Args: []string{p.Effective}}
	default:
		spec = procx.Spec{Name: "xdg-open", Args: []string{p.Effective}}
	}

	// fire-and-forget：不等待文件管理器退出（它可能长期驻留）。
	proc, err := procx.StartCtx(context.Background(), spec)
	if err != nil {
		return apierr.Wrap(err, apierr.CodeInvalidArgument)
	}
	_ = proc // 由 procx 托管；不需要 Wait（进程自然退出后由 OS 回收）
	return nil
}

// RestartForPaths 请求重启以应用 dataRoot 变更（API-9.15）。
//
// dataRoot 变更**只允许重启生效**（DOC-02 §4.8），因此这里登记的是一次「退出」：
// 前端应提示用户重新打开应用；我们不自行 relaunch，避免在 dev/server 模式下
// 拉起一个脱离调试器的进程。
func (s *Service) RestartForPaths(_ context.Context) error {
	app := application.Get()
	if app == nil {
		return apierr.New(apierr.CodeInvalidArgument).WithDetail("application not ready")
	}
	go func() {
		// 给前端一点时间渲染提示，再退出。
		time.Sleep(400 * time.Millisecond)
		logx.For("pathsvc").Info("quit requested to apply path changes")
		app.Quit()
	}()
	return nil
}

// isPathError 判断是否是「目标不可写」类错误（paths 层用 fmt.Errorf 包装 os 错误）。
func isPathError(err error) bool {
	if err == nil {
		return false
	}
	if os.IsPermission(err) {
		return true
	}
	var pathErr *os.PathError
	if ok := asPathError(err, &pathErr); ok {
		return true
	}
	return false
}

func asPathError(err error, target **os.PathError) bool {
	for err != nil {
		if pe, ok := err.(*os.PathError); ok {
			*target = pe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// String 便于日志输出（避免直接把结构体打进日志）。
func (s *Service) String() string { return fmt.Sprintf("pathsvc") }
