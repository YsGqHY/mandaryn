//go:build windows && !server

package app

import (
	"context"
	"runtime"
	"unsafe"

	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/logx"

	"golang.org/x/sys/windows"
)

var playRecordingCueProc = windows.NewLazySystemDLL("winmm.dll").NewProc("PlaySoundW")

func newRecordingCueObserver(ctx context.Context, enabled func(context.Context) bool) func(contract.RecordingState) {
	return runRecordingCueWorker(ctx, enabled, playRecordingCue)
}

func playRecordingCue(wav []byte) {
	if len(wav) == 0 || playRecordingCueProc.Find() != nil {
		return
	}
	const (
		sndSync      = 0x0000 // do not return while WinMM can still read the buffer
		sndNoDefault = 0x0002 // never play a default system sound on failure
		sndMemory    = 0x0004
	)
	var pin runtime.Pinner
	pin.Pin(&wav[0])
	defer pin.Unpin()
	ok, _, _ := playRecordingCueProc.Call(uintptr(unsafe.Pointer(&wav[0])), 0, sndSync|sndNoDefault|sndMemory)
	logx.For("recording-cue").Debug("recording cue playback", "success", ok != 0)
}
