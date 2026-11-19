package audioframes

import (
	"bufio"
	"context"
	"io"
	"io/fs"

	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// FrameSource reads normalized mono samples, indexed by sample frame.
// It owns the underlying handle after a successful open and is not concurrent-safe.
type FrameSource interface {
	Close() error
	SampleRate() int
	TotalFrames() int64
	TotalMs() int64
	ReadFrames(start, n int64) ([]float32, error)
}

// OpenFrameSource detects WAV or native FLAC by its signature. On error, the
// caller still owns the handle. File-backed sources must be regular files.
func OpenFrameSource(r io.ReadSeekCloser) (FrameSource, error) {
	if r == nil {
		return nil, errAudioInvalid("音频句柄为空")
	}
	if f, ok := r.(interface{ Stat() (fs.FileInfo, error) }); ok {
		info, err := f.Stat()
		if err != nil {
			return nil, errAudioInvalid("音频文件不可读").WithCause(err)
		}
		if !info.Mode().IsRegular() {
			return nil, errAudioInvalid("音频不是普通文件")
		}
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, errAudioInvalid("音频定位失败").WithCause(err)
	}
	var signature [4]byte
	if _, err := io.ReadFull(r, signature[:]); err != nil {
		return nil, errAudioInvalid("音频头不完整").WithCause(err)
	}
	switch string(signature[:]) {
	case "RIFF":
		return openWAV(r)
	case "fLaC":
		return openFLAC(r)
	default:
		return nil, errAudioInvalid("不支持的音频容器")
	}
}

const (
	flacMaxMetadataBlocks = 64
	flacMaxMetadataBytes  = 16 << 20
	flacMaxFrameBytes     = 1 << 20
)

type flacSource struct {
	r          io.ReadSeekCloser
	br         *bufio.Reader
	info       *meta.StreamInfo
	dataOffset int64
	block      *frame.Frame
	blockStart int64
	next       int64
}

// openFLAC follows the signature with exactly one STREAMINFO block. Skip
// optional metadata by size rather than allocating/parsing it, and position
// the buffered decoder at the first frame.
func openFLAC(r io.ReadSeekCloser) (*flacSource, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr[0]&0x7f != 0 || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 34 {
		return nil, errAudioInvalid("FLAC STREAMINFO 头无效").WithCause(err)
	}
	if _, err := r.Seek(4, io.SeekStart); err != nil {
		return nil, errAudioInvalid("FLAC 音频定位失败").WithCause(err)
	}
	block, err := meta.Parse(r)
	if err != nil {
		return nil, errAudioInvalid("FLAC STREAMINFO 无效").WithCause(err)
	}
	info := block.Body.(*meta.StreamInfo)
	if info.SampleRate == 0 || info.SampleRate > 192000 || info.NChannels < 1 || info.NChannels > 2 ||
		info.BitsPerSample != 16 || info.BlockSizeMin < 16 || info.BlockSizeMax < info.BlockSizeMin || info.NSamples == 0 {
		return nil, errAudioInvalid("不支持的 FLAC 采样格式或缺少总帧数")
	}
	pos := int64(42)
	fileSize, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, errAudioInvalid("FLAC 音频定位失败").WithCause(err)
	}
	for last, count := block.IsLast, 1; !last; count++ {
		if count >= flacMaxMetadataBlocks || pos+4 > fileSize {
			return nil, errAudioInvalid("FLAC 元数据块超限或不完整")
		}
		if _, err := r.Seek(pos, io.SeekStart); err != nil {
			return nil, errAudioInvalid("FLAC 元数据定位失败").WithCause(err)
		}
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, errAudioInvalid("FLAC 元数据头不完整").WithCause(err)
		}
		size := int64(hdr[1])<<16 | int64(hdr[2])<<8 | int64(hdr[3])
		pos += 4 + size
		if pos > flacMaxMetadataBytes || hdr[0]&0x7f == 0 || hdr[0]&0x7f == 127 {
			return nil, errAudioInvalid("FLAC 元数据块无效或超限")
		}
		last = hdr[0]&0x80 != 0
	}
	if pos >= fileSize {
		return nil, errAudioInvalid("FLAC 缺少音频帧")
	}
	if _, err := r.Seek(pos, io.SeekStart); err != nil {
		return nil, errAudioInvalid("FLAC 音频定位失败").WithCause(err)
	}
	return &flacSource{r: r, br: bufio.NewReaderSize(r, 32<<10), info: info, dataOffset: pos}, nil
}

func (s *flacSource) Close() error       { return s.r.Close() }
func (s *flacSource) SampleRate() int    { return int(s.info.SampleRate) }
func (s *flacSource) TotalFrames() int64 { return int64(s.info.NSamples) }
func (s *flacSource) TotalMs() int64     { return s.TotalFrames() * 1000 / int64(s.SampleRate()) }

func (s *flacSource) ReadFrames(start, n int64) ([]float32, error) {
	return s.ReadFramesContext(context.Background(), start, n)
}

// ReadFramesContext also checks cancellation while skipping FLAC blocks.
func ReadFramesContext(ctx context.Context, src FrameSource, start, n int64) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f, ok := src.(*flacSource); ok {
		return f.ReadFramesContext(ctx, start, n)
	}
	return src.ReadFrames(start, n)
}

func (s *flacSource) ReadFramesContext(ctx context.Context, start, n int64) ([]float32, error) {
	if n <= 0 || start >= s.TotalFrames() {
		return nil, nil
	}
	if start < 0 {
		return nil, errAudioInvalid("起始帧为负")
	}
	end := start + min(n, s.TotalFrames()-start)
	if start < s.blockStart {
		if _, err := s.r.Seek(s.dataOffset, io.SeekStart); err != nil {
			return nil, errAudioInvalid("FLAC 音频定位失败").WithCause(err)
		}
		s.br.Reset(s.r)
		s.block, s.blockStart, s.next = nil, 0, 0
	}
	out := make([]float32, 0, min(end-start, 65536))
	for start < end {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.block == nil || start >= s.next {
			if err := s.nextBlock(); err != nil {
				return nil, errAudioInvalid("FLAC 音频帧损坏或不完整").WithCause(err)
			}
			if start >= s.next {
				continue
			}
		}
		limit := min(end, s.next)
		channels := s.block.Subframes
		scale := float32(len(channels) * 32768)
		for i := start - s.blockStart; i < limit-s.blockStart; i++ {
			value := channels[0].Samples[i]
			if len(channels) == 2 {
				value += channels[1].Samples[i]
			}
			out = append(out, float32(value)/scale)
		}
		start = limit
	}
	return out, nil
}

func (s *flacSource) nextBlock() error {
	// Bound even a damaged frame's encoded size. Check its parameters before
	// the decoder allocates PCM. A fixed-size stream may end in a short block.
	budget := int64(s.info.FrameSizeMax)
	if budget == 0 || budget > flacMaxFrameBytes {
		budget = flacMaxFrameBytes
	}
	f, err := frame.New(&io.LimitedReader{R: s.br, N: budget})
	if err != nil {
		return err
	}
	if f.BitsPerSample == 0 {
		f.BitsPerSample = s.info.BitsPerSample
	}
	start := f.Num
	if f.HasFixedBlockSize {
		start *= uint64(s.info.BlockSizeMax)
	}
	if start != uint64(s.next) || f.BitsPerSample != s.info.BitsPerSample ||
		(f.SampleRate != 0 && f.SampleRate != s.info.SampleRate) || f.Channels.Count() != int(s.info.NChannels) ||
		f.BlockSize == 0 || f.BlockSize > s.info.BlockSizeMax || int64(f.BlockSize) > s.TotalFrames()-s.next {
		return errAudioInvalid("FLAC 帧头与 STREAMINFO 不一致")
	}
	if err := parseFLACSamples(f); err != nil {
		return err
	}
	for _, ch := range f.Subframes {
		if len(ch.Samples) != int(f.BlockSize) {
			return errAudioInvalid("FLAC 子帧采样数无效")
		}
	}
	s.block, s.blockStart = f, s.next
	s.next += int64(f.BlockSize)
	return nil
}

// Malformed LPC/Rice coefficients can violate assumptions inside the fixed
// decoder dependency. Convert an internal panic into an invalid-audio error.
func parseFLACSamples(f *frame.Frame) (err error) {
	defer func() {
		if recover() != nil {
			err = errAudioInvalid("FLAC 子帧解码失败")
		}
	}()
	return f.Parse()
}
