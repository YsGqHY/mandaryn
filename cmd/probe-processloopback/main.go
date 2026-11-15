// 独立探测工具：确认本机是否支持「进程级回环（按应用分路捕获）」。
//
// 用途：在改动生产代码之前，先用它拿到当前机器的**实测**结论。
//
//	go run ./cmd/probe-processloopback
//
// 退出码：0=可用，1=不可用，2=探测本身出错。
package main

import (
	"fmt"
	"os"

	"vip.oberon.mandaryn/internal/capture"
)

func main() {
	ok, note := capture.ProbeProcessLoopback()
	if ok {
		fmt.Println("进程级回环：可用（ActivateAudioInterfaceAsync + PROCESS_LOOPBACK 返回 S_OK）")
		os.Exit(0)
	}
	fmt.Println("进程级回环：不可用")
	if note != "" {
		fmt.Println("证据：", note)
	}
	os.Exit(1)
}
