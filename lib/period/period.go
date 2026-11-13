// Package period names budget periods: a calendar month in the
// installation's period time zone, the month a call started in
// (contracts.md §3). The gateway's counters, its ledger rows and
// golem-control's watermarks all name a call's period through here, so they
// cannot disagree about which month a call near midnight belongs to.
package period

import "time"

// Of is the period t falls in, as "2026-11".
func Of(t time.Time, zone *time.Location) string { return t.In(zone).Format("2006-01") }

// FirstDay is a period's first day, as the ledger's Date column takes it
// ("2026-11-01").
func FirstDay(p string) string { return p + "-01" }
