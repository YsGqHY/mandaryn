package audioflac

import (
	"crypto/md5"
	"encoding/binary"
	"hash"
	"hash/crc32"
	"io"
)

const (
	streamInfoOffset = 8 // signature (4) + metadata header (4)
	streamInfoBytes  = 34
	applicationID    = 0x4d4e4459                             // "MNDY"
	intentOffset     = streamInfoOffset + streamInfoBytes + 8 // metadata header and APPLICATION ID
	intentBytes      = 16
	headerBytes      = intentOffset + intentBytes
)

// frameIntent is written immediately before appending a frame. FLAC frames
// don't carry their byte length. A corrupt Rice stream can read past its actual
// end, so a decoder's EOF alone does not prove the last frame was truncated.
// The APPLICATION block carries this independent length without a sidecar or
// changing standard FLAC decoding. A torn intent never authorizes truncation.
type frameIntent struct {
	offset int64
	size   int
}

func (i frameIntent) marshal() [intentBytes]byte {
	var b [intentBytes]byte
	binary.BigEndian.PutUint64(b[:8], uint64(i.offset))
	binary.BigEndian.PutUint32(b[8:12], uint32(i.size))
	binary.BigEndian.PutUint32(b[12:], crc32.ChecksumIEEE(b[:12]))
	return b
}

func parseIntent(b []byte) frameIntent {
	if binary.BigEndian.Uint32(b[12:]) != crc32.ChecksumIEEE(b[:12]) {
		return frameIntent{}
	}
	off := int64(binary.BigEndian.Uint64(b[:8]))
	size := int(binary.BigEndian.Uint32(b[8:12]))
	if off < headerBytes || size < 2 || size > maxFrameBytes {
		return frameIntent{}
	}
	return frameIntent{offset: off, size: size}
}

type streamStats struct {
	samples  int64
	end      int64
	minFrame int
	maxFrame int
	md5      hash.Hash
}

func newStreamStats() streamStats { return streamStats{end: headerBytes, md5: md5.New()} }

func (s *streamStats) add(samples []int32, frameBytes int) {
	var pcm [blockSamples * 2]byte
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[2*i:], uint16(sample))
	}
	_, _ = s.md5.Write(pcm[:2*len(samples)])
	s.samples += int64(len(samples))
	s.end += int64(frameBytes)
	if s.minFrame == 0 || frameBytes < s.minFrame {
		s.minFrame = frameBytes
	}
	s.maxFrame = max(s.maxFrame, frameBytes)
}

// For fixed blocking both fields are the nominal block size, ignoring any
// shorter final block; the actual final frame still contributes to frame sizes.
func (s *streamStats) streamInfo() [streamInfoBytes]byte {
	var b [streamInfoBytes]byte
	binary.BigEndian.PutUint16(b[:2], blockSamples)
	binary.BigEndian.PutUint16(b[2:4], blockSamples)
	putUint24(b[4:7], s.minFrame)
	putUint24(b[7:10], s.maxFrame)
	const format = uint64(sampleRate)<<44 | uint64(bitsPerSample-1)<<36 // mono stores 0 in the channel field.
	binary.BigEndian.PutUint64(b[10:18], format|uint64(s.samples))
	copy(b[18:], s.md5.Sum(nil))
	return b
}

func putUint24(b []byte, v int) { b[0], b[1], b[2] = byte(v>>16), byte(v>>8), byte(v) }

func writeAt(f io.WriterAt, p []byte, off int64) error {
	n, err := f.WriteAt(p, off)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}
