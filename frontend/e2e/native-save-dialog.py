"""Drive only the named native dialog of the isolated test application PID."""
import ctypes as c
from ctypes import wintypes as w
import os
import sys
import time

pid, title, action, destination, initial_filename = sys.argv[1:]
pid = int(pid)
user = c.WinDLL('user32', use_last_error=True)
callback = c.WINFUNCTYPE(w.BOOL, w.HWND, w.LPARAM)
user.EnumWindows.argtypes = [callback, w.LPARAM]
user.EnumChildWindows.argtypes = [w.HWND, callback, w.LPARAM]
user.GetWindowThreadProcessId.argtypes = [w.HWND, c.POINTER(w.DWORD)]
user.IsWindowVisible.argtypes = [w.HWND]
user.GetWindowTextW.argtypes = [w.HWND, w.LPWSTR, c.c_int]
user.GetClassNameW.argtypes = [w.HWND, w.LPWSTR, c.c_int]
user.GetDlgItem.argtypes = [w.HWND, c.c_int]
user.GetDlgItem.restype = w.HWND
user.SendMessageTimeoutW.argtypes = [w.HWND, w.UINT, w.WPARAM, w.LPARAM, w.UINT, w.UINT, c.POINTER(c.c_size_t)]
user.SendMessageTimeoutW.restype = w.LPARAM
user.PostMessageW.argtypes = [w.HWND, w.UINT, w.WPARAM, w.LPARAM]


def name(hwnd, getter=user.GetWindowTextW):
    buf = c.create_unicode_buffer(2048)
    getter(hwnd, buf, len(buf))
    return buf.value


def message(hwnd, msg, wparam=0, lparam=0):
    result = c.c_size_t()
    if not user.SendMessageTimeoutW(hwnd, msg, wparam, lparam, 2, 1500, c.byref(result)):
        raise OSError(c.get_last_error(), 'native dialog message timed out')
    return result.value


def control_text(hwnd):
    buf = c.create_unicode_buffer(2048)
    message(hwnd, 0x000D, len(buf), c.addressof(buf))  # WM_GETTEXT
    return buf.value


def locate():
    matches = []

    @callback
    def visit(hwnd, _):
        owner = w.DWORD()
        user.GetWindowThreadProcessId(hwnd, c.byref(owner))
        if owner.value == pid and user.IsWindowVisible(hwnd) and name(hwnd, user.GetClassNameW) == '#32770' and name(hwnd) == title:
            matches.append(hwnd)
        return True

    user.EnumWindows(visit, 0)
    if len(matches) > 1:
        raise RuntimeError('multiple matching native dialogs')
    return matches[0] if matches else None


end = time.monotonic() + 12
while not (dialog := locate()):
    if time.monotonic() >= end:
        raise RuntimeError(f'Native dialog {title!r} not found for PID {pid}')
    time.sleep(0.05)

if initial_filename:
    candidates = []

    @callback
    def filename_control(hwnd, _):
        if user.IsWindowVisible(hwnd) and name(hwnd, user.GetClassNameW) == 'Edit':
            candidates.append(hwnd)
        return True

    user.EnumChildWindows(dialog, filename_control, 0)
    edits = [hwnd for hwnd in candidates if control_text(hwnd) == initial_filename]
    if len(edits) != 1:
        raise RuntimeError(f'Expected one native filename input initialized to {initial_filename!r}; found {len(edits)} among {len(candidates)} Edit controls')
    if action == 'save':
        if not os.path.isabs(destination) or os.path.lexists(destination):
            raise RuntimeError('test save target must be an absent absolute path')
        buf = c.create_unicode_buffer(destination)
        if not message(edits[0], 0x000C, 0, c.addressof(buf)) or control_text(edits[0]) != destination:  # WM_SETTEXT
            raise RuntimeError('native filename input did not accept destination')

if action not in {'ok', 'save', 'cancel'}:
    raise RuntimeError('unsupported dialog action')
if action == 'ok':
    # Wails' informational dialog need not expose a Win32 IDOK child.
    if not user.PostMessageW(dialog, 0x0010, 0, 0):  # WM_CLOSE
        raise RuntimeError('could not dismiss native confirmation')
else:
    button = user.GetDlgItem(dialog, 2 if action == 'cancel' else 1)  # IDCANCEL / IDOK
    if not button or not user.PostMessageW(button, 0x00F5, 0, 0):  # BM_CLICK
        raise RuntimeError('could not click native dialog button')
end = time.monotonic() + 5
while locate():
    if time.monotonic() >= end:
        raise RuntimeError(f'Native dialog {title!r} did not dismiss')
    time.sleep(0.05)
