package recording

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/storage"
)

// Store 是录制域对持久层的**端口**（只有本包需要的那几个动作）。
//
// 为什么用接口而不是直接抓 *storage.Holder：
//   - 编排逻辑（Recorder / Recover）因此可以在没有数据库的情况下被单测驱动
//     （注入内存实现），崩溃恢复这种「构造脏库行」的测试也不必真的杀进程；
//   - L2 领域包不把「GORM 用法」扩散出去，未来换存储只需换适配器。
//
// 生产实现是下面的 GORMStore（由 recordingsvc 用 *storage.Holder 构造）。
type Store interface {
	// CreateSession 插入一条会话行（状态为 recording）。
	CreateSession(ctx context.Context, s *storage.Session) error
	// UpdateSession 按**字段名 map** 更新会话。
	//
	// 必须收 map 而不是结构体：GORM 的 `Updates(struct)` 会**跳过零值字段**，
	// 导致「把 size_bytes 更新成 0」「把 ended_at 清空」这类操作静默失效
	// （docs/PERSISTENCE.md 的零值陷阱，见 DEV-ISSUES-S2 S2-01）。
	UpdateSession(ctx context.Context, id string, fields map[string]any) error
	// GetSession 按 ID 取会话；不存在返回 ERR_NOT_FOUND。
	GetSession(ctx context.Context, id string) (*storage.Session, error)
	// ListUnfinished 列出「进程死掉时会卡住」的会话：
	// status ∈ {recording, paused, finalizing}。
	ListUnfinished(ctx context.Context) ([]storage.Session, error)
	// AppendEvent 追加一条时间轴标记（TBL-10）。
	AppendEvent(ctx context.Context, e *storage.SessionEvent) error
}

// GORMStore 是 Store 的 SQLite 实现。
//
// 持有 *storage.Holder 而不是 *storage.DB：切换数据目录会替换 DB 句柄，
// 只有 holder 能始终拿到当前活跃实例（docs/PERSISTENCE.md 铁律）。
type GORMStore struct {
	holder *storage.Holder
}

// NewGORMStore 构造存储适配器。
func NewGORMStore(holder *storage.Holder) *GORMStore { return &GORMStore{holder: holder} }

func (s *GORMStore) db(ctx context.Context) (*gorm.DB, error) {
	if s == nil || s.holder == nil || s.holder.Current() == nil {
		return nil, apierr.New(apierr.CodeDBWrite).WithDetail("数据库尚未就绪")
	}
	return s.holder.Current().GORM.WithContext(ctx), nil
}

// CreateSession 插入会话行。
func (s *GORMStore) CreateSession(ctx context.Context, row *storage.Session) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	if err := db.Create(row).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("写入会话失败：%v", err)
	}
	return nil
}

// UpdateSession 按字段 map 更新会话（零值也会被写入）。
func (s *GORMStore) UpdateSession(ctx context.Context, id string, fields map[string]any) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	if len(fields) == 0 {
		return nil
	}
	res := db.Model(&storage.Session{}).Where("id = ?", id).Updates(fields)
	if res.Error != nil {
		return apierr.Wrap(res.Error, apierr.CodeDBWrite).WithDetail("更新会话失败：%v", res.Error)
	}
	if res.RowsAffected == 0 {
		return apierr.New(apierr.CodeNotFound).WithDetail("会话不存在：%s", id)
	}
	return nil
}

// GetSession 取会话。
func (s *GORMStore) GetSession(ctx context.Context, id string) (*storage.Session, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	var row storage.Session
	if err := db.Where("id = ?", id).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, apierr.New(apierr.CodeNotFound).WithDetail("会话不存在：%s", id)
		}
		return nil, apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("读取会话失败：%v", err)
	}
	return &row, nil
}

// ListUnfinished 列出未正常结束的会话（按 started_at 升序，恢复顺序稳定）。
func (s *GORMStore) ListUnfinished(ctx context.Context) ([]storage.Session, error) {
	db, err := s.db(ctx)
	if err != nil {
		return nil, err
	}
	var rows []storage.Session
	if err := db.Where("status IN ?", []string{
		"recording", "paused", "finalizing",
	}).Order("started_at ASC").Find(&rows).Error; err != nil {
		return nil, apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("扫描未完成会话失败：%v", err)
	}
	return rows, nil
}

// AppendEvent 追加时间轴标记。
//
// 标记写失败不该中断录制（它是「回放可见性」的附加信息），
// 因此由调用方决定是记日志还是忽略。
func (s *GORMStore) AppendEvent(ctx context.Context, e *storage.SessionEvent) error {
	db, err := s.db(ctx)
	if err != nil {
		return err
	}
	if err := db.Create(e).Error; err != nil {
		return apierr.Wrap(err, apierr.CodeDBWrite).WithDetail("写入时间轴标记失败：%v", err)
	}
	return nil
}
