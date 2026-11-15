package auth

import "sync/atomic"

// A scheduler decorator stands between the Manager and whatever scheduler it is given: every
// scheduler passed to SetPluginScheduler, nil included, goes through it first. The pool
// (internal/pool) uses it to choose the account for each request.

var schedulerDecorator atomic.Pointer[func(PluginScheduler) PluginScheduler]

// SetSchedulerDecorator installs decorate, or removes the decorator when it is nil. It takes
// effect at the next SetPluginScheduler.
func SetSchedulerDecorator(decorate func(PluginScheduler) PluginScheduler) {
	if decorate == nil {
		schedulerDecorator.Store(nil)
		return
	}
	schedulerDecorator.Store(&decorate)
}

func decorateScheduler(scheduler PluginScheduler) PluginScheduler {
	if decorate := schedulerDecorator.Load(); decorate != nil {
		return (*decorate)(scheduler)
	}
	return scheduler
}
