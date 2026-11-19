package recording

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/utils/filex"
	"vip.oberon.mandaryn/internal/utils/logx"
)

// ExportFormatWAV 导出支持的格式（本版本仅 WAV）。
const ExportFormatWAV = "wav"

// ExportAudio 把会话音频另存为一个独立音频文件（API-2.7 / FR-03-11）。
//
// 三条硬性约束：
//  1. **不改数据库记录**：导出是「把已有的东西复制出去」，会话仍在库里、
//     仍可回放。因此这里一个 Store 写操作都不做（连 size_bytes 都不更新）。
//  2. **不改源文件**：即便源文件的 WAV 头因崩溃没回填，也只在**副本**上修头
//     （源文件的修补是 Recover 的职责，属于另一个决策）。
//  3. targetPath 为空时**不弹窗**：原生「另存为」对话框由前端 NativeDialogs 打开
//     （CLAUDE.md 铁律禁止后端直接弹 UI），后端只负责把文件复制到给定路径。
//     传空路径返回 ERR_INVALID_ARGUMENT 并说明，避免静默什么都不做。
//
// 返回实际写入的绝对路径。
func (r *Recorder) ExportAudio(ctx context.Context, sessionID, targetPath string) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("sessionId 不能为空")
	}
	target := strings.TrimSpace(targetPath)
	if target == "" {
		return "", apierr.New(apierr.CodeInvalidArgument).
			WithDetail("targetPath 不能为空：请先用原生「另存为」对话框选择目标文件")
	}
	if err := ensureExportableFormat(target); err != nil {
		return "", err
	}

	row, err := r.opts.Store.GetSession(ctx, sessionID)
	if err != nil {
		return "", apierr.From(err, apierr.CodeNotFound)
	}
	src, err := r.audioAbsPath(row.AudioPath)
	if err != nil {
		return "", apierr.From(err, apierr.CodeNotFound)
	}
	source, statErr := os.Stat(src)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return "", apierr.New(apierr.CodeNotFound).
				WithDetail("会话音频文件已不存在：%s", row.AudioPath)
		}
		return "", wrapFSError(statErr, "读取会话音频失败")
	}

	format := strings.ToLower(strings.TrimPrefix(filepath.Ext(src), "."))
	if strings.ToLower(filepath.Ext(target)) != "."+format {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("导出须使用与录音相同的 .%s 格式", format)
	}
	if dest, err := os.Stat(target); err == nil && os.SameFile(source, dest) {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("导出路径不能是录音源文件")
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		absTarget = target
	}
	if err := filex.EnsureDir(filepath.Dir(absTarget)); err != nil {
		return "", wrapFSError(err, "创建导出目录失败")
	}
	if err := copyFile(src, absTarget); err != nil {
		return "", err
	}

	// 副本的头若与实际长度不符（源文件在崩溃后未修补），在副本上补一次。
	if info, ierr := InspectWAV(absTarget); ierr == nil {
		if int64(info.DataBytes) != info.DataFileBytes {
			if _, rerr := RepairWAV(absTarget); rerr != nil {
				logx.For("recording").Warn("导出副本补头失败（文件仍可播放到实际长度）",
					"session", row.ID, "err", rerr.Error())
			}
		}
	}

	logx.For("recording").Info("导出会话音频",
		"session", row.ID, "bytes", row.SizeBytes, "format", format)
	return absTarget, nil
}

// ensureExportableFormat rejects implicit conversion and unknown extensions.
func ensureExportableFormat(target string) error {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(target), "."))
	switch ext {
	case AudioFormatWAV, AudioFormatFLAC:
		return nil
	case "":
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("导出路径缺少扩展名：请使用原录音格式")
	default:
		return apierr.New(apierr.CodeInvalidArgument).
			WithDetail("不支持的导出格式 %q：仅支持原始的 WAV 或 FLAC 格式", ext)
	}
}

// copyFile 复制文件并刷盘；磁盘满映射成 ERR_DISK_FULL。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return wrapFSError(err, "打开源音频失败")
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return wrapFSError(err, "创建导出文件失败")
	}
	// 目标已存在且为空也不能留半截：出错时删除本次创建的残缺文件。
	cleanup := true
	defer func() {
		_ = out.Close()
		if cleanup {
			_ = os.Remove(dst)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return wrapFSError(err, "写入导出文件失败")
	}
	if err := out.Sync(); err != nil {
		return wrapFSError(err, "导出文件刷盘失败")
	}
	cleanup = false
	return nil
}

// ExportFileName 为某会话生成默认导出文件名（不含目录）。
//
// 由前端在和「另存为」对话框一起使用；放在这里是为了让 L3 与前端都能拿到
// 同一套命名规则（标题里可能有 : / \ 等非法字符，必须清洗）。
func ExportFileName(sess contract.Session) string {
	base := strings.TrimSpace(sess.Title)
	if base == "" {
		base = sess.ID
	}
	replacer := strings.NewReplacer(
		"\\", "_", "/", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
		"\r", "", "\n", "",
	)
	base = replacer.Replace(base)
	if base == "" {
		base = sess.ID
	}
	return base + "." + ExportFormatWAV
}
