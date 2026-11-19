package models

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"vip.oberon.mandaryn/internal/apierr"
	"vip.oberon.mandaryn/internal/contract"
	"vip.oberon.mandaryn/internal/storage"
)

const mirrorPreference = "models.catalogMirror"

func normalizeUserMirror(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || len(value) > 2048 || strings.Contains(value, "\\") ||
		u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("镜像必须是没有凭据、查询或片段的 HTTP(S) 地址")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (m *Manager) savedModelMirror(ctx context.Context) (string, error) {
	db := m.db()
	if db == nil {
		return "", nil
	}
	var pref storage.Preference
	if err := db.WithContext(ctx).Where("key = ?", mirrorPreference).Take(&pref).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", apierr.Wrap(err, apierr.CodeDBWrite)
	}
	var saved string
	if err := json.Unmarshal([]byte(pref.Value), &saved); err != nil {
		return "", apierr.New(apierr.CodeInvalidArgument).WithDetail("已保存的镜像配置无效，请重新保存")
	}
	return normalizeUserMirror(saved)
}

func (m *Manager) ModelMirror(ctx context.Context) (contract.ModelMirror, error) {
	saved, err := m.savedModelMirror(ctx)
	return contract.ModelMirror{SavedURL: saved, ManagedBy: m.opts.MirrorManagedBy}, err
}

// SetModelMirror persists a user catalog source without making a network request.
func (m *Manager) SetModelMirror(ctx context.Context, raw string) (contract.ModelMirror, error) {
	if m.opts.MirrorManagedBy != "" {
		return contract.ModelMirror{}, apierr.New(apierr.CodeStateForbidden).WithDetail("镜像设置由部署环境管理")
	}
	value, err := normalizeUserMirror(raw)
	if err != nil {
		return contract.ModelMirror{}, err
	}
	db := m.db()
	if db == nil {
		return contract.ModelMirror{}, apierr.New(apierr.CodeDBWrite)
	}
	encoded, _ := json.Marshal(value)
	pref := storage.Preference{Key: mirrorPreference, Value: string(encoded)}
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&pref).Error; err != nil {
		return contract.ModelMirror{}, apierr.Wrap(err, apierr.CodeDBWrite)
	}
	return contract.ModelMirror{SavedURL: value}, nil
}

func (m *Manager) effectiveUserMirror(ctx context.Context) (string, error) {
	if m.opts.MirrorManagedBy != "" || !m.opts.UseStarterCatalog {
		return "", nil
	}
	return m.savedModelMirror(ctx)
}
