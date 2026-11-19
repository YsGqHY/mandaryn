package speaker

import (
	"context"
	"io"
	"math"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/utils/audioframes"
)

// AudioSource 是可随机读取的音频句柄（*os.File 天然满足）。
type AudioSource interface {
	io.ReadSeekCloser
}

// AudioOpener 由装配层注入：经 paths.Resolver 把会话 ID 解析成可读音频文件。
type AudioOpener func(ctx context.Context, sessionID string) (AudioSource, error)

// AudioLoader 是「按毫秒区间取单声道浮点样本」的端口。
type AudioLoader interface {
	// LoadSegment 返回 [startMs, endMs) 的 16kHz 单声道样本（已归一化到 [-1,1]）。
	LoadSegment(ctx context.Context, sessionID string, startMs, endMs int64) ([]float32, error)
}

// WAVLoader 读取已封存的 FLAC 或旧 WAV 录音；名称为兼容现有调用方保留。
type WAVLoader struct {
	open AudioOpener
}

// NewWAVLoader 构造音频片段读取器；open 为 nil 时报参数错误。
func NewWAVLoader(open AudioOpener) (*WAVLoader, error) {
	if open == nil {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("AudioOpener 不能为空")
	}
	return &WAVLoader{open: open}, nil
}

var _ AudioLoader = (*WAVLoader)(nil)

func (l *WAVLoader) openSession(ctx context.Context, sessionID string) (audioframes.FrameSource, error) {
	raw, err := l.open(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	src, err := audioframes.OpenFrameSource(raw)
	if err != nil {
		if raw != nil {
			_ = raw.Close()
		}
		return nil, err
	}
	// 声纹模型只接受 16kHz，不做重采样。
	if src.SampleRate() != SampleRate {
		_ = src.Close()
		return nil, apierr.New(apierr.CodeInvalidArgument).
			WithAction("re-record").
			WithDetail("音频采样率 %dHz 不受支持（声纹提取要求 %dHz）", src.SampleRate(), SampleRate)
	}
	return src, nil
}

func (l *WAVLoader) LoadSegment(ctx context.Context, sessionID string, startMs, endMs int64) ([]float32, error) {
	if endMs <= startMs {
		return nil, nil
	}
	src, err := l.openSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = src.Close() }()
	return loadSegmentFrames(ctx, src, startMs, endMs)
}

func loadSegmentFrames(ctx context.Context, src audioframes.FrameSource, startMs, endMs int64) ([]float32, error) {
	if endMs <= startMs {
		return nil, nil
	}
	if startMs < 0 {
		return nil, apierr.New(apierr.CodeInvalidArgument).WithDetail("音频片段起始时间不能为负")
	}
	// 16kHz 恰好每毫秒 16 帧，先裁剪再换算避免整数溢出。
	const framesPerMs = SampleRate / 1000
	if startMs > math.MaxInt64/framesPerMs {
		return nil, nil
	}
	start := startMs * framesPerMs
	end := min(endMs, math.MaxInt64/framesPerMs) * framesPerMs
	return audioframes.ReadFramesContext(ctx, src, start, end-start)
}

// segmentReader keeps one sequential decoder per analysis rather than opening
// and rescanning a FLAC file for every segment. Other AudioLoader implementations
// still use the original per-segment contract. Open lazily for all-short sessions.
func segmentReader(ctx context.Context, loader AudioLoader, sessionID string) (
	load func(startMs, endMs int64) ([]float32, error), closeReader func(),
) {
	l, ok := loader.(*WAVLoader)
	if !ok {
		return func(startMs, endMs int64) ([]float32, error) {
			return loader.LoadSegment(ctx, sessionID, startMs, endMs)
		}, func() {}
	}
	var src audioframes.FrameSource
	var openErr error
	return func(startMs, endMs int64) ([]float32, error) {
			if src == nil && openErr == nil {
				src, openErr = l.openSession(ctx, sessionID)
			}
			if openErr != nil {
				return nil, openErr
			}
			return loadSegmentFrames(ctx, src, startMs, endMs)
		}, func() {
			if src != nil {
				_ = src.Close()
				src = nil
			}
		}
}
