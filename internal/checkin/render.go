package checkin

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/oner8/newapi-checkin/internal/model"
	"github.com/oner8/newapi-checkin/internal/notify"
)

// 通知里失败项的升级级别：失败需要能穿透 iOS 的专注模式。
const failureLevel = "timeSensitive"

// RenderMessage 把批次汇总渲染成一条 Bark 通知。
//
// fresh 是本次"值得提醒"的失败项（已通过去重）；未出现在 fresh 里的失败会在正文里
// 被合并成一行提示，避免同一天反复推送同样的失败。
func RenderMessage(sum *Summary, fresh []SiteResult, defaultLevel string) notify.Message {
	level := strings.TrimSpace(defaultLevel)
	if level == "" {
		level = "active"
	}

	freshSet := make(map[string]bool, len(fresh))
	for _, f := range fresh {
		freshSet[f.SiteName] = true
	}

	ok, total := sum.Succeeded(), sum.Total()
	allFailures := sum.Failures()
	title := fmt.Sprintf("签到完成 %d/%d", ok, total)
	if len(allFailures) > 0 {
		// 语气按"当天全部失败"判断，而不是本次新增的失败：否则 always 模式下
		// 第二次运行会因为失败项已去重，把标题显示成"签到完成"，与事实相反。
		title = fmt.Sprintf("签到异常 %d/%d", ok, total)
		level = failureLevel
	}
	if total == 0 {
		title = "签到未执行"
	}

	var lines []string
	if quota := sum.QuotaAwarded(); quota > 0 {
		lines = append(lines, "本次共获得 "+formatMoney(quota, firstQuotaPerUnit(sum)))
	}

	suppressed := 0
	for _, r := range sum.Results {
		switch r.Status {
		case model.StatusSuccess:
			line := "✅ " + r.SiteName
			if r.QuotaAwarded > 0 {
				line += " +" + formatMoney(r.QuotaAwarded, r.QuotaPerUnit)
			} else {
				line += " 签到成功"
			}
			if r.QuotaAfter != nil {
				line += fmt.Sprintf("（余额 %s）", formatMoney(*r.QuotaAfter, r.QuotaPerUnit))
			}
			lines = append(lines, line)
		case model.StatusAlready:
			lines = append(lines, "☑️ "+r.SiteName+" 今日已签到")
		case model.StatusSkippedDisabled:
			lines = append(lines, "⏭️ "+r.SiteName+" 站点未开启签到")
		case model.StatusDryRun:
			lines = append(lines, "🧪 "+r.SiteName+" 试运行未提交")
		default:
			if !freshSet[r.SiteName] {
				suppressed++
				continue
			}
			line := fmt.Sprintf("❌ %s %s", r.SiteName, r.Status.Label())
			if msg := strings.TrimSpace(r.Message); msg != "" {
				line += "：" + truncateRunes(msg, 160)
			}
			lines = append(lines, line)
		}
	}

	if suppressed > 0 {
		lines = append(lines, fmt.Sprintf("（另有 %d 项失败今天已提醒过，不重复推送）", suppressed))
	}
	if len(fresh) > 0 {
		lines = append(lines, hintFor(sum, fresh))
	}

	return notify.Message{
		Title: title,
		Body:  strings.Join(lines, "\n"),
		Level: level,
	}
}

// hintFor 针对失败原因给出可操作的下一步建议。
func hintFor(sum *Summary, fresh []SiteResult) string {
	seen := map[model.Status]bool{}
	var hints []string
	add := func(s model.Status, text string) {
		if seen[s] {
			return
		}
		seen[s] = true
		hints = append(hints, text)
	}
	for _, f := range fresh {
		switch f.Status {
		case model.StatusAuthFailed:
			add(f.Status, "凭据已失效：重新登录面板复制 Cookie 到环境变量后重启容器")
		case model.StatusNeedTurnstile:
			add(f.Status, "站点在签到接口上开了 Cloudflare Turnstile：token 由浏览器生成、一次性且与校验方 IP 相关，脚本无法自动通过；换令牌/换 Cookie 都没用（校验挂在接口上，与凭据类型无关）。只能在浏览器手动签到，或请站点管理员关闭该开关")
		case model.StatusNotNewAPI:
			add(f.Status, "站点未开放签到接口：确认 base_url 是否正确、站点是否为 new-api")
		case model.StatusBlocked:
			add(f.Status, "被站点前置的 WAF/防护拦截：在浏览器通过挑战后，把整段 Cookie（含 acw_sc__v2 / cf_clearance）填进 credential.cookie，并把 headers.User-Agent 设成与浏览器一致")
		case model.StatusError:
			add(f.Status, "其他错误：查看容器日志中的完整报错")
		}
	}
	if len(hints) == 0 {
		return ""
	}
	return "提示：" + strings.Join(hints, "；")
}

// firstQuotaPerUnit 返回第一个已知的额度换算单位，兜底用 new-api 默认值。
func firstQuotaPerUnit(sum *Summary) float64 {
	for _, r := range sum.Results {
		if r.QuotaPerUnit > 0 {
			return r.QuotaPerUnit
		}
	}
	return defaultQuotaPerUnit
}

// formatMoney 把内部额度换算成金额字符串。
//
// new-api 默认 500000 额度 = $1，签到奖励通常只有几分钱，所以金额越小保留越多小数位。
func formatMoney(quota int64, perUnit float64) string {
	if perUnit <= 0 {
		perUnit = defaultQuotaPerUnit
	}
	value := float64(quota) / perUnit
	switch {
	case value >= 1:
		return "$" + strconv.FormatFloat(value, 'f', 2, 64)
	case value >= 0.01:
		return "$" + strconv.FormatFloat(value, 'f', 4, 64)
	default:
		return "$" + strconv.FormatFloat(value, 'f', 6, 64)
	}
}
