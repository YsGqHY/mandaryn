package audioflac

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/mewkiz/flac/frame"
)

// Repair validates a recording made by Create and returns its on-disk sample
// count. It only discards a physically incomplete, journaled final frame in
// an unsealed file. CRC errors and inconsistent sealed files are left intact.
// The caller must ensure no writer is still using path.
func Repair(path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !st.Mode().IsRegular() {
		return 0, errors.New("audioflac: repair requires a regular file")
	}
	var h [headerBytes]byte
	if _, err := io.ReadFull(f, h[:]); err != nil {
		return 0, fmt.Errorf("audioflac: read recording header: %w", err)
	}
	if string(h[:streamInfoOffset]) != "fLaC\x00\x00\x00\x22" ||
		string(h[streamInfoOffset+streamInfoBytes:intentOffset]) != "\x82\x00\x00\x14MNDY" {
		return 0, errors.New("audioflac: unsupported recording header")
	}
	info := h[streamInfoOffset : streamInfoOffset+streamInfoBytes]
	stats := newStreamStats()
	initial := stats.streamInfo()
	clear(initial[18:]) // The provisional STREAMINFO has no digest.
	if !bytes.Equal(info[:4], initial[:4]) || !bytes.Equal(info[10:13], initial[10:13]) {
		return 0, errors.New("audioflac: unexpected recording format")
	}
	unsealed := bytes.Equal(info, initial[:])
	intent := parseIntent(h[intentOffset:])
	if err := scanFrames(f, st.Size(), intent, unsealed, &stats); err != nil {
		return 0, err
	}
	final := stats.streamInfo()
	if !unsealed {
		if !bytes.Equal(info, final[:]) {
			return 0, errors.New("audioflac: sealed STREAMINFO does not match audio")
		}
		return stats.samples, nil
	}

	// The scan above made no changes. Truncation cannot touch a confirmed
	// frame, and STREAMINFO can be regenerated without recopying the audio.
	if stats.end < st.Size() {
		if err := f.Truncate(stats.end); err != nil {
			return 0, fmt.Errorf("audioflac: remove incomplete frame: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("audioflac: sync recovered frames: %w", err)
	}
	if err := writeAt(f, final[:], streamInfoOffset); err != nil {
		return 0, fmt.Errorf("audioflac: update STREAMINFO: %w", err)
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("audioflac: sync recovered STREAMINFO: %w", err)
	}
	return stats.samples, nil
}

func scanFrames(source io.Reader, size int64, intent frameIntent, unsealed bool, stats *streamStats) error {
	r := bufio.NewReader(source)
	var num uint64
	var shortBlock bool
	for stats.end < size {
		if shortBlock {
			return errors.New("audioflac: data follows a short final block")
		}
		remaining := size - stats.end
		// Check even a one-byte fragment against the fixed-block sync prefix.
		prefix, err := r.Peek(int(min(remaining, 2)))
		if err != nil {
			return fmt.Errorf("audioflac: read frame %d: %w", num, err)
		}
		if !bytes.Equal(prefix, []byte("\xff\xf8")[:len(prefix)]) {
			return fmt.Errorf("audioflac: invalid sync for frame %d at byte %d", num, stats.end)
		}

		limit := min(remaining, maxFrameBytes)
		if stats.end < intent.offset && intent.offset < size {
			limit = min(limit, intent.offset-stats.end)
		}
		journaled := stats.end == intent.offset
		if journaled {
			limit = min(limit, int64(intent.size))
		}
		bounded := &io.LimitedReader{R: r, N: limit}
		f, err := frame.New(bounded)
		if err == nil {
			samples := int64(f.BlockSize)
			if !f.HasFixedBlockSize || samples == 0 || samples > blockSamples ||
				f.BitsPerSample != bitsPerSample || f.SampleRate != sampleRate ||
				f.Channels != frame.ChannelsMono || f.Num != num || samples > maxSamples-stats.samples {
				return fmt.Errorf("audioflac: invalid header for frame %d at byte %d", num, stats.end)
			}
			err = f.Parse()
		}
		if err != nil {
			// A corrupt code can also cause decoder EOF. Only a physical EOF
			// strictly before the independently recorded frame end is repairable.
			if unsealed && journaled && remaining < int64(intent.size) && bounded.N == 0 &&
				(errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				return nil
			}
			return fmt.Errorf("audioflac: invalid frame %d at byte %d: %w", num, stats.end, err)
		}
		count := int(limit - bounded.N)
		if journaled && count != intent.size {
			return fmt.Errorf("audioflac: length mismatch for frame %d", num)
		}
		samples := f.Subframes[0].Samples
		if len(samples) != int(f.BlockSize) {
			return fmt.Errorf("audioflac: sample count mismatch for frame %d", num)
		}
		for _, v := range samples {
			if v < math.MinInt16 || v > math.MaxInt16 {
				return fmt.Errorf("audioflac: sample out of range in frame %d", num)
			}
		}
		stats.add(samples, count)
		shortBlock = len(samples) < blockSamples
		num++
	}
	return nil
}
