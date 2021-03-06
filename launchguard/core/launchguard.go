package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Netflix/metrics-client-go/metrics"
)

type launchGuardStateMachineState int

const (
	tickWindow = 15 * time.Second
)

const (
	// The event queue is empty
	emptyState launchGuardStateMachineState = iota
	// The event at events[0] is a cleanup event, we're waiting for it to finish
	waitingOnCleanupEventState
	// The event at events[0] is a launch event, give it clearance to launch
	doLaunchState
)

// LaunchGuard coordinates the starting and shutting down of containers
type LaunchGuard struct {
	metrics          metrics.Reporter
	cleanUpEventChan chan cleanUpEvent
	launchEventChan  chan launchEvent
	events           []launchGuardEvent
	// The purpose of the Ticker is to bump the state so we can report the depth metric
	ticker *time.Ticker
}

// NewLaunchGuard should be used to instantiate LaunchGuards. LaunchGuards should never be directly instantiated.
func NewLaunchGuard(m metrics.Reporter) *LaunchGuard {
	lg := &LaunchGuard{
		metrics: m,
		events:  []launchGuardEvent{},
		// We should always be able to take cleanup events async
		cleanUpEventChan: make(chan cleanUpEvent),
		// Launch Events are blocking anyway, no point in optimizing here
		launchEventChan: make(chan launchEvent),
		ticker:          time.NewTicker(tickWindow),
	}
	go lg.loop()
	return lg
}

func (lg *LaunchGuard) loop() {
	defer close(lg.cleanUpEventChan)
	defer close(lg.launchEventChan)
	defer lg.ticker.Stop()
	state := emptyState
	for {
		switch state {
		case emptyState:
			state = lg.dispatchEmpty()
		case waitingOnCleanupEventState:
			state = lg.dispatchWaitingOnCleanupEvent()
		case doLaunchState:
			state = lg.doLaunch()
		default:
			panic(fmt.Sprint("Launch Guard loop in unknown state: ", state))
		}
		lg.metrics.Gauge("titus.executor.launchGuard.depth", len(lg.events), nil)
	}
}

func (lg *LaunchGuard) dispatchEmpty() launchGuardStateMachineState {
	select {
	case myCleanUpEvent := <-lg.cleanUpEventChan:
		lg.events = append(lg.events, myCleanUpEvent)
		return waitingOnCleanupEventState
	case myLaunchEvent := <-lg.launchEventChan:
		lg.events = append(lg.events, myLaunchEvent)
		return doLaunchState
	case <-lg.ticker.C:
		return emptyState
	}
}

func (lg *LaunchGuard) dispatchWaitingOnCleanupEvent() launchGuardStateMachineState {
	lastCleanUpEvent := lg.events[0].(*RealCleanUpEvent)
	select {
	case <-lastCleanUpEvent.done():
		if lastCleanUpEvent.ctx.Err() == context.DeadlineExceeded {
			lg.metrics.Counter("titus.executor.launchGuard.deadlineExceededError", 1, nil)
		}
		// Remove event from the wait queue
		lg.events = lg.events[1:]
		return lg.determineStateAfter()
	case myCleanupEvent := <-lg.cleanUpEventChan:
		lg.events = append(lg.events, myCleanupEvent)
		return waitingOnCleanupEventState
	case myLaunchEvent := <-lg.launchEventChan:
		lg.events = append(lg.events, myLaunchEvent)
		return waitingOnCleanupEventState
	case <-lg.ticker.C:
		return waitingOnCleanupEventState
	}
}

func (lg *LaunchGuard) determineStateAfter() launchGuardStateMachineState {
	if len(lg.events) == 0 {
		return emptyState
	}
	switch lg.events[0].(type) {
	case cleanUpEvent:
		return waitingOnCleanupEventState
	case launchEvent:
		return doLaunchState
	}
	panic(fmt.Sprintf("Unknown event type: %T", lg.events[0]))
}

func (lg *LaunchGuard) doLaunch() launchGuardStateMachineState {
	event := lg.events[0].(launchEvent)
	event.notifyLaunch()
	lg.events = lg.events[1:]
	return lg.determineStateAfter()
}

type launchGuardEvent interface{}

var (
	_ cleanUpEvent = (*RealCleanUpEvent)(nil)
	_ cleanUpEvent = (*NoopCleanUpEvent)(nil)
)

/* cleanup event code */
// These are events that

// RealCleanUpEvent should be used when the launchGuard is actually needed (kill)
type RealCleanUpEvent struct {
	// We wait for this to read as closed
	createdAt time.Time
	ctx       context.Context
	metrics   metrics.Reporter
	cancel    context.CancelFunc
	once      sync.Once
}

// NewRealCleanUpEvent must be used to instantiate new real cleanup events
func NewRealCleanUpEvent(parentCtx context.Context, lg *LaunchGuard) cleanUpEvent { // nolint: golint
	ctx, cancel := context.WithCancel(parentCtx)
	event := &RealCleanUpEvent{
		ctx:       ctx,
		metrics:   lg.metrics,
		cancel:    cancel,
		createdAt: time.Now(),
		once:      sync.Once{},
	}
	lg.cleanUpEventChan <- event
	return event
}

// Done is used to indicate that the event has been cleaned up, and the launch guard can move on. It is idempotent.
// It cancels the underlying context that the real cleanup event built from the parent context.
func (ce *RealCleanUpEvent) Done() {
	ce.once.Do(func() {
		ce.metrics.Timer("titus.executor.cleanUpEvent.timeInQueue", time.Since(ce.createdAt), nil)
		ce.cancel()
	})
}

func (ce *RealCleanUpEvent) done() <-chan struct{} {
	return ce.ctx.Done()
}

// NoopCleanUpEvent is an event to stub out the CleanupEvent when one isn't needed (normal shutdown)
type NoopCleanUpEvent struct{}

// Done does nothing
func (ce *NoopCleanUpEvent) Done() {}
func (ce *NoopCleanUpEvent) done() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// Cancel does nothing
func (ce *NoopCleanUpEvent) Cancel() {}

var (
	_ launchEvent = (*realLaunchEvent)(nil)
	_ LaunchEvent = (*NoopLaunchEvent)(nil)
)

type realLaunchEvent struct {
	metrics    metrics.Reporter
	createdAt  time.Time
	internalCh chan struct{}
	once       sync.Once
}

// NewLaunchEvent must be used to instantiate new LaunchEvents
func NewLaunchEvent(lg *LaunchGuard) launchEvent { // nolint: golint
	event := &realLaunchEvent{
		metrics:    lg.metrics,
		createdAt:  time.Now(),
		internalCh: make(chan struct{}),
		once:       sync.Once{},
	}
	lg.launchEventChan <- event
	return event
}

// One must read off this channel, and once it is closed (returns the nil value) we know it's done
func (ce *realLaunchEvent) Launch() <-chan struct{} {
	return ce.internalCh
}

func (ce *realLaunchEvent) notifyLaunch() {
	ce.once.Do(
		func() {
			ce.metrics.Timer("titus.executor.launchEvent.timeInQueue", time.Since(ce.createdAt), nil)
			close(ce.internalCh)
		})
}

// NoopLaunchEvent is an event to stub out the CleanupEvent when one isn't needed (normal shutdown)
type NoopLaunchEvent struct{}

// Launch Always immediately returns a closed channel
func (ce *NoopLaunchEvent) Launch() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}
