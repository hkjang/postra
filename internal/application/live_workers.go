package application

import (
	"context"
	"time"
)

// Polling keeps startup-disabled workers alive without busy-looping. Settings
// changes are observed within this interval; an in-flight operation is allowed
// to finish, and the next operation uses the current configuration.
const workerSettingsPoll = 5 * time.Second

type workerSchedule struct {
	initialized bool
	active      bool
	last        time.Time
}

// due is separate from timers so enable/disable and cadence changes can be
// verified deterministically. Enabling a dormant worker triggers a first pass.
func (s *workerSchedule) due(now time.Time, interval time.Duration, initial bool) bool {
	first := !s.initialized
	s.initialized = true
	if interval <= 0 {
		s.active = false
		return false
	}
	if !s.active {
		s.active, s.last = true, now
		return !first || initial
	}
	if now.Before(s.last.Add(interval)) {
		return false
	}
	s.last = now
	return true
}

func (a *App) runLiveWorker(ctx context.Context, name string, interval func() time.Duration, initial bool, run func()) {
	ticker := time.NewTicker(workerSettingsPoll)
	defer ticker.Stop()
	state := workerSchedule{}
	tick := func(now time.Time) {
		period := interval()
		if !a.IsLeader() {
			period = 0
		}
		if state.due(now, period, initial) && ctx.Err() == nil {
			a.guard(name, run)
		}
	}
	tick(time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			tick(now)
		}
	}
}

func minutesDuration(minutes int) time.Duration {
	if minutes <= 0 {
		return 0
	}
	// Bound duration arithmetic even if an old/imported setting exceeds the
	// contemporary UI limits. One year already exceeds useful worker cadence.
	if minutes > 365*24*60 {
		minutes = 365 * 24 * 60
	}
	return time.Duration(minutes) * time.Minute
}

func (a *App) tryAcquireSyncSlot() (bool, <-chan struct{}) {
	limit := a.EffectiveConfig().Sync.MaxConcurrentSyncs
	if limit < 1 {
		limit = 2
	}
	a.syncSlotsMu.Lock()
	defer a.syncSlotsMu.Unlock()
	if a.syncSlotsChanged == nil {
		a.syncSlotsChanged = make(chan struct{})
	}
	if a.syncSlotsInUse < limit {
		a.syncSlotsInUse++
		return true, nil
	}
	return false, a.syncSlotsChanged
}

func (a *App) acquireSyncSlot(ctx context.Context) error {
	ticker := time.NewTicker(workerSettingsPoll)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if acquired, changed := a.tryAcquireSyncSlot(); acquired {
			return nil
		} else {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-changed:
			case <-ticker.C: // a live increase can open slots without a release
			}
		}
	}
}

func (a *App) releaseSyncSlot() {
	a.syncSlotsMu.Lock()
	defer a.syncSlotsMu.Unlock()
	a.syncSlotsInUse--
	close(a.syncSlotsChanged)
	a.syncSlotsChanged = make(chan struct{})
}
