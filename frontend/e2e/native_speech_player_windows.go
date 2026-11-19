// The standalone test player gives application loopback a unique executable
// name. Build this file explicitly; it is not part of the Mandaryn desktop app.
package main

import (
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	filename, err := windows.UTF16PtrFromString(os.Args[1])
	if err != nil {
		os.Exit(2)
	}
	play := windows.NewLazySystemDLL("winmm.dll").NewProc("PlaySoundW")
	const fileLoop = 0x20000 | 0x0002 | 0x0001 | 0x0008 // FILENAME | NODEFAULT | ASYNC | LOOP
	if ok, _, _ := play.Call(uintptr(unsafe.Pointer(filename)), 0, fileLoop); ok == 0 {
		os.Exit(3)
	}
	time.Sleep(45 * time.Second) // Do not outlive a failed test indefinitely.
	play.Call(0, 0, 0)
}
