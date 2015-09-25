package main

import (
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/log"
	"golang.org/x/net/context"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/types"
)

const ResolveTimeout = 30 * time.Second

// Dispatcher sorts incoming alerts into aggregation groups and
// assigns the correct notifiers to each.
type Dispatcher struct {
	routes Routes
	alerts provider.Alerts

	aggrGroups map[model.Fingerprint]*aggrGroup
	notifiers  map[string]Notifier

	mtx    sync.RWMutex
	done   chan struct{}
	ctx    context.Context
	cancel func()
}

// NewDispatcher returns a new Dispatcher.
func NewDispatcher(ap provider.Alerts) *Dispatcher {
	return &Dispatcher{alerts: ap}
}

// ApplyConfig updates the dispatcher to match the new configuration.
func (d *Dispatcher) ApplyConfig(conf *config.Config) {
	d.mtx.Lock()
	defer d.mtx.Unlock()

	// If a cancelation function is set, the dispatcher is running.
	if d.cancel != nil {
		d.Stop()
		defer func() { go d.Run() }()
	}

	d.routes = NewRoutes(conf.Routes)
	d.notifiers = map[string]Notifier{}

	// TODO(fabxc): build correct notifiers from new conf.NotificationConfigs.
	for _, ncfg := range conf.NotificationConfigs {
		d.notifiers[ncfg.Name] = &LogNotifier{ncfg.Name}
	}
}

// Run starts dispatching alerts incoming via the updates channel.
func (d *Dispatcher) Run() {
	d.done = make(chan struct{})
	d.aggrGroups = map[model.Fingerprint]*aggrGroup{}

	d.ctx, d.cancel = context.WithCancel(context.Background())

	updates := d.alerts.IterActive()

	defer close(d.done)
	// TODO(fabxc): updates channel is never closed!!!

	d.run(updates)
}

func (d *Dispatcher) run(updates <-chan *types.Alert) {
	cleanup := time.Tick(30 * time.Second)

	for {
		select {
		case alert := <-updates:
			d.mtx.RLock()
			routes := d.routes.Match(alert.Labels)
			d.mtx.RUnlock()

			for _, r := range routes {
				d.processAlert(alert, r)
			}

		case <-cleanup:
			for _, ag := range d.aggrGroups {
				if ag.empty() {
					ag.stop()
					delete(d.aggrGroups, ag.fingerprint())
				}
			}

		case <-d.ctx.Done():
			return
		}
	}
}

// Stop the dispatcher.
func (d *Dispatcher) Stop() {
	d.cancel()
	d.cancel = nil
	<-d.done
}

// notifyFunc is a function that performs notifcation for the alert
// with the given fingerprint. It aborts on context cancelation.
// It returns whether the alert has successfully been communiated as
// resolved.
type notifyFunc func(context.Context, *types.Alert) bool

// notifyFunc returns a function which performs a notification
// as required by the routing options.
func (d *Dispatcher) notifyFunc(dest string) notifyFunc {
	d.mtx.Lock()
	defer d.mtx.Unlock()

	notifier := d.notifiers[dest]

	return func(ctx context.Context, alert *types.Alert) bool {
		if err := notifier.Notify(ctx, alert); err != nil {
			log.Errorf("Notify for %v failed: %s", alert, err)
			return false
		}
		return alert.Resolved()
	}
}

// processAlert determins in which aggregation group the alert falls
// and insert it.
func (d *Dispatcher) processAlert(alert *types.Alert, opts *RouteOpts) {
	group := model.LabelSet{}

	for ln, lv := range alert.Labels {
		if _, ok := opts.GroupBy[ln]; ok {
			group[ln] = lv
		}
	}

	fp := group.Fingerprint()

	// If the group does not exist, create it.
	ag, ok := d.aggrGroups[fp]
	if !ok {
		ag = newAggrGroup(d.ctx, group, opts)
		ag.run(d.notifyFunc(opts.SendTo))

		d.aggrGroups[fp] = ag
	}

	ag.insert(alert)
}

// aggrGroup aggregates alert fingerprints into groups to which a
// common set of routing options applies.
// It emits notifications in the specified intervals.
type aggrGroup struct {
	labels model.LabelSet
	opts   *RouteOpts

	ctx    context.Context
	cancel func()
	done   chan struct{}
	next   *time.Timer

	mtx     sync.RWMutex
	alerts  map[model.Fingerprint]*types.Alert
	hasSent bool
}

// newAggrGroup returns a new aggregation group.
func newAggrGroup(ctx context.Context, labels model.LabelSet, opts *RouteOpts) *aggrGroup {
	ag := &aggrGroup{
		labels: labels,
		opts:   opts,
		alerts: map[model.Fingerprint]*types.Alert{},
	}
	ag.ctx, ag.cancel = context.WithCancel(ctx)

	return ag
}

func (ag *aggrGroup) run(notify notifyFunc) {
	ag.done = make(chan struct{})

	// Set an initial one-time wait before flushing
	// the first batch of notifications.
	ag.next = time.NewTimer(ag.opts.GroupWait)

	defer close(ag.done)
	defer ag.next.Stop()

	for {
		select {
		case <-ag.next.C:
			// Give the notifcations 2/3 the time of the repeat interval
			// to finish before terminating them.
			ctx, _ := context.WithTimeout(ag.ctx, ag.opts.RepeatInterval*2/3)

			// Wait the configured interval before calling flush again.
			ag.next.Reset(ag.opts.RepeatInterval)

			ag.flush(func(a *types.Alert) bool {
				return notify(ctx, a)
			})

		case <-ag.ctx.Done():
			return
		}
	}
}

func (ag *aggrGroup) stop() {
	// Calling cancel will terminate all in-process notifications
	// and the run() loop.
	ag.cancel()
	<-ag.done
}

func (ag *aggrGroup) fingerprint() model.Fingerprint {
	return ag.labels.Fingerprint()
}

// insert the alert into the aggregation group. If the aggregation group
// is empty afterwards, true is returned.
func (ag *aggrGroup) insert(alert *types.Alert) {
	ag.mtx.Lock()
	defer ag.mtx.Unlock()

	ag.alerts[alert.Fingerprint()] = alert

	// Immediately trigger a flush if the wait duration for this
	// alert is already over.
	if !ag.hasSent && alert.Timestamp.Add(ag.opts.GroupWait).Before(time.Now()) {
		ag.next.Reset(0)
	}
}

func (ag *aggrGroup) empty() bool {
	ag.mtx.RLock()
	defer ag.mtx.RUnlock()

	return len(ag.alerts) == 0
}

// flush sends notifications for all new alerts.
func (ag *aggrGroup) flush(notify func(*types.Alert) bool) {
	ag.mtx.Lock()

	alerts := make(map[model.Fingerprint]*types.Alert, len(ag.alerts))
	for fp, alert := range ag.alerts {
		alerts[fp] = alert
	}

	ag.mtx.Unlock()

	var wg sync.WaitGroup
	wg.Add(len(alerts))

	for fp, a := range alerts {
		go func(fp model.Fingerprint, a *types.Alert) {
			// notify returns whether the alert can be deleted
			// afterwards.
			if notify(a) {
				ag.mtx.Lock()
				// Only delete if the fingerprint has not been inserted
				// again since we notified about it.
				if ag.alerts[fp] == a {
					delete(alerts, fp)
				}
				ag.mtx.Unlock()
			}
			wg.Done()
		}(fp, a)
	}

	wg.Wait()

	ag.hasSent = true
}
