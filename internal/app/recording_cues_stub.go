//go:build !windows || server

package app

import (
	"context"

	"vip.oberon.mandaryn/internal/contract"
)

func newRecordingCueObserver(context.Context, func(context.Context) bool) func(contract.RecordingState) {
	return func(contract.RecordingState) {}
}
