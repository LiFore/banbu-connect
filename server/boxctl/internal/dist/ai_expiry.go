package dist

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"boxctl/internal/notify"
)

// ---- AI 账号的订阅到期日 ----
//
// 管理员在账号上手动填写订阅（Claude Pro / Max、ChatGPT Plus / Pro、Google AI Pro 等）的到期日，
// 只用于管理页面显示和推送提醒；不向 Anthropic / OpenAI / Google 查询，也不下发给客户端。

const expiryLayout = "2006-01-02"

// normalizeExpiry 检查到期日：空串表示不填，否则是 YYYY-MM-DD（也接受带时间的 RFC 3339，只取日期）。
func normalizeExpiry(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if len(v) > len(expiryLayout) {
		v = v[:len(expiryLayout)]
	}
	if _, err := time.Parse(expiryLayout, v); err != nil {
		return "", &InputError{"订阅到期日的格式应为 YYYY-MM-DD"}
	}
	return v, nil
}

// expiryDaysLeft 是从 now 到到期日当天结束（loc 时区）还剩几天：当天到期为 0，已过期为负数。
func expiryDaysLeft(date string, now time.Time, loc *time.Location) (int, bool) {
	d, err := time.ParseInLocation(expiryLayout, date, loc)
	if err != nil {
		return 0, false
	}
	lt := now.In(loc)
	today := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	return int(math.Round(d.Sub(today).Hours() / 24)), true // 四舍五入抵消夏令时的 23 / 25 小时
}

// checkAIExpiry 提醒离订阅到期不到 days 天（含已过期）的账号。key 带上到期日：改了到期日（续费后）会按新日期重新提醒。
func (s *Server) checkAIExpiry(ctx context.Context, days int, now time.Time, loc *time.Location, send func(key, kind string, m notify.Message)) error {
	type acc struct {
		kind, label, email, date string
		id                       int64
	}
	var list []acc
	claude, err := s.ClaudeAccounts(ctx)
	if err != nil {
		return err
	}
	labels := map[string]string{KindClaude: "Claude", KindCodex: "Codex"}
	for _, a := range claude {
		list = append(list, acc{a.Kind, labels[a.Kind], a.Email, a.SubExpires, a.ID})
	}
	gemini, err := s.GeminiAccounts(ctx)
	if err != nil {
		return err
	}
	for _, a := range gemini {
		list = append(list, acc{"gemini", "Gemini", a.Email, a.SubExpires, a.ID})
	}
	for _, a := range list {
		left, ok := expiryDaysLeft(a.date, now, loc)
		if !ok || left > days {
			continue
		}
		title := fmt.Sprintf("%s 订阅 %d 天后到期：%s", a.label, left, a.email)
		switch {
		case left < 0:
			title = fmt.Sprintf("%s 订阅已到期：%s", a.label, a.email)
		case left == 0:
			title = fmt.Sprintf("%s 订阅今天到期：%s", a.label, a.email)
		}
		// 进入最后一天时再提醒一次。
		stage := "early"
		if left <= 0 {
			stage = "due"
		}
		send(fmt.Sprintf("ai-expire:%s:%d:%s:%s", a.kind, a.id, a.date, stage), "ai_expire", notify.Message{
			Title:  title,
			Body:   fmt.Sprintf("到期日 %s（管理员在「AI 账号」里填写）。续费后记得改到期日。", a.date),
			Urgent: left <= 1,
		})
	}
	return nil
}
