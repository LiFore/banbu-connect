package subscriptions

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// 定时更新（M2-11）：按每个订阅的 update_hours 自动拉取并应用。
// 失败时不动已有节点（预览失败不改数据），原因记在 last_error，首页显示；
// 更新会删掉仍被代理组引用的节点时不自动应用，等用户手动确认。

const (
	scheduleTick = time.Minute
	// retryAfter 是自动更新失败后的重试间隔，不超过订阅本身的间隔。
	retryAfter = 30 * time.Minute
	// ErrNeedsConfirm 写在 last_error 里，提示用户手动更新。
	needsConfirmMsg = "自动更新会删除仍被代理组引用的节点，已跳过；请在订阅页手动更新并确认"
)

// ErrNeedsConfirm 表示自动更新会删除仍被引用的节点，已跳过，需要手动更新并确认。
var ErrNeedsConfirm = errors.New(needsConfirmMsg)

// RunScheduler 定时检查到期的订阅并更新；有订阅的节点变了时调用 onUpdated。ctx 结束时返回。
func (s *Service) RunScheduler(ctx context.Context, log *slog.Logger, onUpdated func()) {
	attempts := map[string]time.Time{} // 订阅 ID → 上一次自动更新的时间（含失败）
	t := time.NewTicker(scheduleTick)
	defer t.Stop()
	for {
		if s.runDue(ctx, log, attempts) && onUpdated != nil {
			onUpdated()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runDue 更新所有到期的订阅，返回是否有订阅应用了新节点。
func (s *Service) runDue(ctx context.Context, log *slog.Logger, attempts map[string]time.Time) bool {
	list, err := s.List(ctx)
	if err != nil {
		log.Warn("读取订阅失败", "err", err)
		return false
	}
	now := s.now()
	updated := false
	for _, sb := range list {
		if !sb.Enabled || sb.Options.UpdateHours <= 0 || !sb.HasURL {
			continue
		}
		if !s.due(sb, attempts[sb.ID], now) {
			continue
		}
		attempts[sb.ID] = now
		diff, err := s.autoUpdate(ctx, sb.ID)
		if s.OnAutoUpdate != nil {
			s.OnAutoUpdate(sb, diff, err)
		}
		if err != nil {
			log.Warn("订阅自动更新失败", "subscription", sb.Name, "err", err)
			continue
		}
		updated = updated || len(diff.Added)+len(diff.Removed)+len(diff.Changed) > 0
	}
	return updated
}

// due 判断是否该更新：距上次成功拉取超过间隔；上次自动更新失败的，过 retryAfter 再试。
func (s *Service) due(sb Subscription, lastAttempt, now time.Time) bool {
	interval := time.Duration(sb.Options.UpdateHours) * time.Hour
	if last, err := time.Parse(timeLayout, sb.LastFetchAt); err == nil && now.Sub(last) < interval {
		return false
	}
	return lastAttempt.IsZero() || now.Sub(lastAttempt) >= min(interval, retryAfter)
}

// autoUpdate 拉取并应用一个订阅，返回节点的变化。
func (s *Service) autoUpdate(ctx context.Context, id string) (Diff, error) {
	p, err := s.Preview(ctx, id) // 失败时 Preview 已记 last_error
	if err != nil {
		return Diff{}, err
	}
	diff, err := s.Apply(ctx, id, p.ID, false)
	var refErr *ReferencesError
	if errors.As(err, &refErr) {
		_, _ = s.db.ExecContext(ctx, `UPDATE subscriptions SET last_error = ? WHERE id = ?`, needsConfirmMsg, id)
		s.mu.Lock()
		delete(s.previews, p.ID)
		s.mu.Unlock()
		return Diff{}, ErrNeedsConfirm
	}
	if err != nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE subscriptions SET last_error = ? WHERE id = ?`, err.Error(), id)
		return Diff{}, err
	}
	return diff, nil
}
