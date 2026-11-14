//go:build linux

package perf

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// —— Linux 采样源：/proc/stat（整机）+ /proc/<pid>/stat（进程树）——
//
// 两边都以 USER_HZ（通常 100）为单位累计，同单位相除即可；这里**不做 HZ 换算**，
// 因为占用率是两个差值之比，换算因子会被约掉（少一次换算就少一个量纲坑）。

// linuxReader 持有子进程表缓存。
type linuxReader struct {
	selfPid     int
	treeRefresh time.Duration

	mu     sync.Mutex
	tree   []int
	treeAt time.Time
}

// newPlatformReader 构造 Linux 读数器。
func newPlatformReader(pid int, treeRefresh time.Duration) (readFunc, error) {
	if pid <= 0 {
		pid = os.Getpid()
	}
	// 先探一次，确保 /proc 可读（容器里偶尔被裁剪），失败就明确报错，
	// 让上层降级为「不采样、不降档」而不是每拍都失败。
	if _, err := readProcStatSelf(pid); err != nil {
		return nil, fmt.Errorf("perf: 无法读取 /proc/<pid>/stat: %w", err)
	}
	r := &linuxReader{selfPid: pid, treeRefresh: treeRefresh}
	return r.read, nil
}

// read 取一拍原始计数。
func (r *linuxReader) read() (rawCounters, error) {
	total, idle, err := readProcStatTotal()
	if err != nil {
		return rawCounters{}, err
	}

	sum := uint64(0)
	pids := append([]int{r.selfPid}, r.childPids()...)
	for _, pid := range pids {
		v, err := readProcStatSelf(pid)
		if err != nil {
			// 进程刚退出（/proc/<pid> 消失）不是错误，跳过即可。
			continue
		}
		sum += v
	}

	return rawCounters{SystemTotal: total, SystemIdle: idle, Self: sum, At: time.Now()}, nil
}

// childPids 返回后代 pid 列表，按 treeRefresh 刷新。
func (r *linuxReader) childPids() []int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if r.tree != nil && r.treeRefresh > 0 && now.Sub(r.treeAt) < r.treeRefresh {
		return r.tree
	}
	r.tree = descendants(r.selfPid)
	r.treeAt = now
	return r.tree
}

// readProcStatTotal 读 /proc/stat 首行，返回 (total, idle) 累计 tick。
//
// idle 取 idle + iowait：iowait 期间 CPU 实际不干活，算作「忙」会让整机占用率虚高，
// 在磁盘慢的机器上足以便出一个降档档位。
// total 只加到 steal 为止：guest / guest_nice 已经计在 user / nice 里，
// 再加一遍会把虚拟化负载算两遍。
func readProcStatTotal() (total, idle uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, fmt.Errorf("perf: 打开 /proc/stat 失败: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, fmt.Errorf("perf: /proc/stat 为空")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("perf: /proc/stat 首行格式异常")
	}
	// 顺序：user nice system idle iowait irq softirq steal [guest guest_nice]
	var vals [8]uint64
	for i := 0; i < 8 && i+1 < len(fields); i++ {
		v, e := strconv.ParseUint(fields[i+1], 10, 64)
		if e != nil {
			return 0, 0, fmt.Errorf("perf: /proc/stat 解析失败: %w", e)
		}
		vals[i] = v
	}
	for _, v := range vals {
		total += v
	}
	idle = vals[3] + vals[4] // idle + iowait
	return total, idle, nil
}

// readProcStatSelf 读 /proc/<pid>/stat，返回该进程 kernel+user 累计 tick。
//
// comm 字段可能含空格与右括号（进程名可以叫 "a) b"），所以不能按空格切分整体，
// 只能先定位**最后一个** ')'，再按空格切后续字段。字段号因此是相对的：
// 完整格式里 utime 是第 14 个字段，切掉前两段后落在下标 11（第 3 字段 ppid 起算）。
func readProcStatSelf(pid int) (uint64, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 > len(s) {
		return 0, fmt.Errorf("perf: /proc/%d/stat 格式异常", pid)
	}
	fields := strings.Fields(s[end+1:])
	// fields[0] = state（原第 3 字段）⇒ utime 原第 14 ⇒ fields[11]，
	// stime 第 15 ⇒ fields[12]，cutime 第 16 ⇒ fields[13]，cstime 第 17 ⇒ fields[14]。
	if len(fields) < 15 {
		return 0, fmt.Errorf("perf: /proc/%d/stat 字段不足", pid)
	}
	var sum uint64
	for _, idx := range []int{11, 12, 13, 14} {
		v, e := strconv.ParseUint(fields[idx], 10, 64)
		if e != nil {
			return 0, fmt.Errorf("perf: /proc/%d/stat 字段 %d 解析失败: %w", pid, idx, e)
		}
		sum += v
	}
	// cutime/cstime 是「已回收的子进程」累计时间，那些子进程已不在 /proc 里，
	// 因此与枚举存活子进程**不重复计算**，加上它们反而更准。
	return sum, nil
}

// descendants 广度优先枚举 pid 的全部后代（用 ppid 建索引后 BFS）。
func descendants(pid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	parents := make(map[int]int, len(entries))
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		p, err := readPPID(child)
		if err != nil {
			continue
		}
		parents[child] = p
	}

	children := make(map[int][]int, len(parents))
	for child, parent := range parents {
		children[parent] = append(children[parent], child)
	}

	var out []int
	queue := append([]int(nil), children[pid]...)
	seen := map[int]bool{pid: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		out = append(out, cur)
		queue = append(queue, children[cur]...)
	}
	return out
}

// readPPID 读 /proc/<pid>/stat 的 ppid（原第 4 字段 ⇒ 切分后 fields[1]）。
func readPPID(pid int) (int, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 || end+2 > len(s) {
		return 0, fmt.Errorf("perf: /proc/%d/stat 格式异常", pid)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("perf: /proc/%d/stat 字段不足", pid)
	}
	return strconv.Atoi(fields[1])
}
