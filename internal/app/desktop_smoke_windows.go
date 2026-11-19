//go:build windows && !server && desktop_smoke

package app

import (
	"os"
	"strconv"
)

func init() {
	port, err := strconv.ParseUint(os.Getenv("E2E_NATIVE_DESKTOP_PORT"), 10, 16)
	if err == nil && port > 0 {
		desktopSmokeBrowserArgs = []string{
			"--remote-debugging-port=" + strconv.FormatUint(port, 10),
			"--remote-debugging-address=127.0.0.1",
		}
	}
}
