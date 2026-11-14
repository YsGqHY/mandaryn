package app

import (
	"context"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/events"
	"vip.oberon.mandaryn/internal/hotkeys"
	"vip.oberon.mandaryn/internal/services/appsettings"
	"vip.oberon.mandaryn/internal/services/recordingsvc"
	"vip.oberon.mandaryn/internal/utils/logx"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// wireHotkeyHandlers 把内置动作接到真实实现上。
//
// 分工原则：**后端能直接做的才在后端做**。录制开关与窗口显隐在后端有权威实现；
// 切页、播放器跳转、聚焦搜索框这些「界面动作」后端没有对应的真实状态，
// 硬造一份只会产生两套导航真相 —— 因此统一经 EV-17 广播给前端，由前端各接自己那部分。
//
// 返回错误一律给 ERR_STATE_FORBIDDEN（热键触发失败是「现在不能做」，
// 而不是参数错误），由 hotkeys.Service 统一映射。
func wireHotkeyHandlers(
	reg *hotkeys.Registry,
	recSvc *recordingsvc.Service,
	settingsSvc *appsettings.Service,
	win application.Window,
	app *application.App,
) {

	// record.toggle：录制中/暂停中 → 停止；否则用「默认音频源」开始。
	if err := reg.SetHandler(hotkeys.ActionRecordToggle, func(ctx context.Context) error {
		st, err := recSvc.GetState(ctx)
		if err != nil {
			return err
		}
		if st.State == contract.StateRecording || st.State == contract.StatePaused {
			_, err := recSvc.Stop(ctx)
			return err
		}
		snap, err := settingsSvc.Get(ctx)
		if err != nil {
			return err
		}
		if snap.Audio.DefaultSourceID == "" {
			// 没有默认源时不猜：热键隔空开始录制却录错源，比不开始更糟。
			return apierr.New(apierr.CodeStateForbidden).
				WithDetail("未设置默认音频源，请先在录制页选择")
		}
		_, err = recSvc.Start(ctx, contract.StartRequest{SourceID: snap.Audio.DefaultSourceID})
		return err
	}); err != nil {
		logx.For("app").Warn("接线热键失败", "action", hotkeys.ActionRecordToggle, "err", err.Error())
	}

	// record.pause：录制中 → 暂停；暂停中 → 继续；其它状态不做（不报错，静默忽略）。
	if err := reg.SetHandler(hotkeys.ActionRecordPause, func(ctx context.Context) error {
		st, err := recSvc.GetState(ctx)
		if err != nil {
			return err
		}
		switch st.State {
		case contract.StateRecording:
			_, err = recSvc.Pause(ctx)
		case contract.StatePaused:
			_, err = recSvc.Resume(ctx)
		default:
			logx.For("app").Debug("非录制态忽略暂停热键", "state", st.State)
		}
		return err
	}); err != nil {
		logx.For("app").Warn("接线热键失败", "action", hotkeys.ActionRecordPause, "err", err.Error())
	}

	// window.show：显示并聚焦主窗口（已是前台时也把焦点拉回来）。
	if err := reg.SetHandler(hotkeys.ActionWindowShow, func(_ context.Context) error {
		if win == nil {
			return nil
		}
		win.Show()
		win.Focus()
		return nil
	}); err != nil {
		logx.For("app").Warn("接线热键失败", "action", hotkeys.ActionWindowShow, "err", err.Error())
	}

	// 其余动作：广播给前端。
	for _, id := range []string{
		hotkeys.ActionPlaybackPlay, hotkeys.ActionPlaybackBack, hotkeys.ActionPlaybackForward,
		hotkeys.ActionPlaybackPrevSeg, hotkeys.ActionPlaybackNextSeg, hotkeys.ActionPlaybackRecenter,
		hotkeys.ActionNavRecord, hotkeys.ActionNavLibrary, hotkeys.ActionNavModels,
		hotkeys.ActionNavSettings, hotkeys.ActionSearchFocus, hotkeys.ActionRoleRename,
		hotkeys.ActionSessionDelete, hotkeys.ActionModelSwitchSrc, hotkeys.ActionLeanMode,
	} {
		action := id
		if err := reg.SetHandler(action, func(_ context.Context) error {
			if app != nil {
				app.Event.Emit(events.HotkeyTrigger, contract.HotkeyTrigger{Action: action})
			}
			return nil
		}); err != nil {
			logx.For("app").Warn("接线热键失败", "action", action, "err", err.Error())
		}
	}
}
