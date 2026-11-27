package extension

import (
	"fmt"
	"time"
)

// clock is what the pages write times against: now (the club's clock) and the viewer's time zone.
type clock struct {
	now time.Time
	loc *time.Location
}

// at is a moment as the pages write it, the one way for every time a page shows: within 7 days relative (「刚刚」
// 「5 分钟前」「3 小时前」「3 天前」, ahead 「5 分钟后」…), older or further the date and time (「11月14日 12:04」), with the
// year only when it is not this year's.
func (c clock) at(l lang, ms int64) string {
	t := time.UnixMilli(ms)
	d := c.now.Sub(t)
	ahead := d < 0
	if ahead {
		d = -d
	}
	rel := func(n int, zhUnit, enUnit string) string {
		if n != 1 {
			enUnit += "s"
		}
		if ahead {
			return l.t(fmt.Sprintf("%d %s后", n, zhUnit), fmt.Sprintf("in %d %s", n, enUnit))
		}
		return l.t(fmt.Sprintf("%d %s前", n, zhUnit), fmt.Sprintf("%d %s ago", n, enUnit))
	}
	switch {
	case d < time.Minute:
		if ahead {
			return l.t("马上", "in a moment")
		}
		return l.t("刚刚", "just now")
	case d < time.Hour:
		return rel(int(d/time.Minute), "分钟", "minute")
	case d < 24*time.Hour:
		return rel(int(d/time.Hour), "小时", "hour")
	case d < 7*24*time.Hour:
		return rel(int(d/(24*time.Hour)), "天", "day")
	}
	loc := c.loc
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	if t.Year() != c.now.In(loc).Year() {
		return l.t(t.Format("2006年1月2日 15:04"), t.Format("Jan 2, 2006 15:04"))
	}
	return l.t(t.Format("1月2日 15:04"), t.Format("Jan 2 15:04"))
}

// relative says whether at wrote ms relative to now (「3 小时前」) rather than as a date, for the words around it.
func (c clock) relative(ms int64) bool {
	return c.now.Sub(time.UnixMilli(ms)).Abs() < 7*24*time.Hour
}

// lastSynced is when an account last synced, or that it never did.
func (c clock) lastSynced(l lang, ms int64) string {
	if ms <= 0 {
		return l.t("还没同步过", "never")
	}
	return c.at(l, ms)
}
