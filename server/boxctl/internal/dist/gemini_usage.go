package dist

import (
	"context"
	"strings"
	"time"
)

// ---- 管理服务保存额度采样（BXC-F0-022）----

const (
	geminiQuotaKeep   = 8 * 24 * time.Hour
	maxQuotaSamples   = 50
	maxQuotaBuckets   = 64
	geminiPruneEvery  = time.Hour
	maxQuotaKeyLength = 128
)

// RecordGeminiQuota 保存客户端上报的额度。只接受分配给这台客户端的账号；定期删掉 8 天前的采样。
func (s *Server) RecordGeminiQuota(ctx context.Context, deviceID int64, samples []GeminiQuotaSample) error {
	if len(samples) > maxQuotaSamples {
		return &InputError{"一次上报的账号太多"}
	}
	now := time.Now().UTC()
	// 先查分配：数据库只有一个连接，事务开着时不能再用 s.DB 查询。
	var keep []GeminiQuotaSample
	for _, smp := range samples {
		ok, err := s.assigned(ctx, smp.AccountID, deviceID)
		if err != nil {
			return err
		}
		if ok && len(smp.Buckets) <= maxQuotaBuckets {
			keep = append(keep, smp)
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, smp := range keep {
		for _, b := range smp.Buckets {
			key := strings.TrimSpace(b.Key)
			if key == "" || len(key) > maxQuotaKeyLength || b.Remaining < 0 || b.Remaining > 1 {
				continue
			}
			reset := ""
			if t, err := time.Parse(time.RFC3339Nano, b.ResetAt); err == nil {
				reset = t.UTC().Format(time.RFC3339)
			}
			label := b.Label
			if len([]rune(label)) > 100 {
				label = string([]rune(label)[:100])
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO gemini_quota (account_id, bucket, label, remaining, reset_at, device_id, sampled_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, smp.AccountID, key, label, b.Remaining, reset, deviceID, now.Format(time.RFC3339)); err != nil {
				return err
			}
		}
	}
	if s.geminiPruned.Load() < now.Add(-geminiPruneEvery).Unix() {
		s.geminiPruned.Store(now.Unix())
		if _, err := tx.ExecContext(ctx, `DELETE FROM gemini_quota WHERE sampled_at < ?`, now.Add(-geminiQuotaKeep).Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.geminiChanged() // 建议顺序可能变了
	return nil
}
