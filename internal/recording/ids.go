package recording

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// crockford ULID 用的 Crockford Base32 字母表（去掉 I/L/O/U，避免人工抄写歧义）。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newSessionID 生成会话 ID：`s_` + 26 位 Crockford Base32（ULID 布局）。
//
// 为什么自己写而不是引 github.com/oklog/ulid：
//   - 依赖清单属于「不新增依赖」约束（go.mod 由其它域负责，私自加会冲突）；
//   - 只需要「时间有序 + 全局唯一」这一个性质，20 行足够，且可被单测断言格式。
//
// 布局：前 10 位 = 48bit 毫秒时间戳，后 16 位 = 80bit 随机数。
// 前缀 `s_` 与 DATA-MODEL TBL-03 一致（`size:32`，实际长度 28）。
func newSessionID() string { return "s_" + newULID(time.Now()) }

// newULID 生成 26 位 ULID 字符串。
func newULID(t time.Time) string {
	var entropy [10]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		// crypto/rand 失败在桌面端几乎不可能；退化成时间纳秒作为随机源，
		// 保证 ID 仍唯一（宁可 ID 质量下降，也不要录制起不来）。
		binary.BigEndian.PutUint64(entropy[2:], uint64(t.UnixNano()))
	}
	var out [26]byte
	ms := uint64(t.UnixMilli())

	// 48bit 时间戳 → 前 10 个 base32 字符（每字符 5bit，10*5=50bit，高位补 0）。
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&0x1f]
		ms >>= 5
	}
	// 80bit 随机数 → 后 16 个 base32 字符（16*5=80bit，正好）。
	var acc uint64
	var bits uint
	idx := 10
	for _, b := range entropy {
		acc = acc<<8 | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(acc>>bits)&0x1f]
			idx++
		}
	}
	return string(out[:])
}
