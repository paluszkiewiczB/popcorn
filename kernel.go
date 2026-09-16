package popcorn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	kernelID        = "kernel"
	healthBacklog   = 8
	defaultStopTime = 5 * time.Second
)

var (
	errHealthTick  = errors.New("health tick must not be negative")
	errStopTimeout = errors.New("stop timeout must not be negative")
	errParallelism = errors.New("parallelism must not be negative")
	errHealthGone  = errors.New("kernel health stream closed")
)

// Kernel manages the lifecycle of a set of Modules. It starts modules in
// dependency order, publishes lifecycle events on the Bus, watches module
// health, and shuts everything down when the context is canceled, a module
// reports NOK, or all tasks finish.
//
// A Kernel runs once: after Start returns, it cannot be started again.
type Kernel struct {
	cfg kernelConfig

	startMu sync.Mutex
	started bool
}

// NewKernel creates a Kernel from the given options. It rejects an invalid
// module set before Start is called: empty, duplicate, or reserved ids, nil
// modules, and unknown, self, duplicate, or circular dependencies.
func NewKernel(opts ...KernelOption) (*Kernel, error) {
	k := &Kernel{cfg: kernelConfig{
		stopTimeout: defaultStopTime,
		log:         discardLogger(),
	}}
	for _, opt := range opts {
		if err := opt(&k.cfg); err != nil {
			return nil, err
		}
	}
	if err := validateModules(k.cfg.modules); err != nil {
		return nil, err
	}
	return k, nil
}

// Start starts modules in dependency order and blocks until ctx is canceled, a
// module fails to start, a module reports NOK, or the kernel becomes idle. A
// module starts once every dependency's Start has returned.
//
// It returns a wrapped [ErrKernelStopped] after a graceful stop, a
// [KernelUnhealthyError] when a registered module reported NOK, the error from a
// module's Start when it fails, or the joined errors from shutdown. Start may be
// called only once: a later call returns [ErrKernelStarted], even if the first
// call failed during setup.
func (k *Kernel) Start(ctx context.Context) error {
	if !k.begin() {
		return ErrKernelStarted
	}
	// Derive a cancelable run context so modules whose Start ignores its own
	// cancellation are released once the kernel has finished shutting down.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	run, err := newKernelRun(ctx, k.cfg)
	if err != nil {
		return err
	}
	defer run.close()

	run.startModules(ctx)
	return run.loop(ctx)
}

func (k *Kernel) begin() bool {
	k.startMu.Lock()
	defer k.startMu.Unlock()
	if k.started {
		return false
	}
	k.started = true
	return true
}

type moduleRun struct {
	id     string
	module Module
	deps   []string
	task   TaskModule
	ready  chan struct{}
	depth  int

	// Guarded by kernelRun.kmu. stopOnce guarantees a module is stopped exactly
	// once even if it finishes starting mid-shutdown.
	started  bool
	stop     StopFunc
	order    int
	stopOnce sync.Once
}

type kernelRun struct {
	cfg     kernelConfig
	bus     *Bus
	ownsBus bool
	pub     *Publisher
	health  <-chan Event

	stateMu sync.Mutex
	state   KernelState

	runs       []*moduleRun
	rts        map[string]*moduleRun
	stopLevels [][]*moduleRun
	totalTasks int
	workers    int
	slots      chan struct{}
	stopSlots  chan struct{}

	halt     chan struct{}
	haltOnce sync.Once

	kmu          sync.Mutex
	startedCount int
	doneTasks    int
	failErr      error

	wake chan struct{}

	ticker *time.Ticker
}

func newKernelRun(ctx context.Context, cfg kernelConfig) (*kernelRun, error) {
	b := cfg.bus
	ownsBus := b == nil
	if ownsBus {
		nb, err := NewBus(WithBusLogger(cfg.log))
		if err != nil {
			return nil, err
		}
		b = nb
	}
	run := &kernelRun{
		cfg:     cfg,
		bus:     b,
		ownsBus: ownsBus,
		pub:     b.Publisher(kernelID),
		halt:    make(chan struct{}),
		wake:    make(chan struct{}, 1),
	}
	run.indexModules(cfg.modules)
	run.assignDepths()
	run.buildStopLevels()
	run.slots = make(chan struct{}, max(1, run.workers))
	run.stopSlots = make(chan struct{}, max(1, run.workers))

	run.transition(ctx, KernelStateStarting, nil)
	health, err := b.Subscribe(kernelID,
		WithBacklog(healthBacklog),
		WithFilter(func(e Event) bool {
			msc, ok := e.Payload.(ModuleStateChanged)
			return ok && msc.To == ModuleStateNOK
		}))
	if err != nil {
		if ownsBus {
			b.Close()
		}
		run.transition(ctx, KernelStateStopped, err)
		return nil, err
	}
	run.health = health
	if cfg.healthTick > 0 {
		run.ticker = time.NewTicker(cfg.healthTick)
	}
	// Discard anything the subscribe-time replay seeded: no module has started
	// yet, so every queued health event is either stale history or out-of-band.
	run.drainHealth()
	return run, nil
}

func (run *kernelRun) drainHealth() {
	for {
		select {
		case _, ok := <-run.health:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func (run *kernelRun) indexModules(modules []Module) {
	n := len(modules)
	run.runs = make([]*moduleRun, 0, n)
	run.rts = make(map[string]*moduleRun, n)
	for _, m := range modules {
		r := &moduleRun{
			id:     m.ID(),
			module: m,
			deps:   slices.Clone(m.Dependencies()),
			ready:  make(chan struct{}),
		}
		if task, ok := m.(TaskModule); ok {
			r.task = task
			run.totalTasks++
		}
		run.runs = append(run.runs, r)
		run.rts[r.id] = r
	}
	run.workers = run.cfg.parallelism
	if run.workers <= 0 {
		run.workers = len(run.runs) // 0 means no cap
	}
}

// assignDepths computes depth(m) = 0 for a module with no dependencies, else
// 1 + max(depth(dep)). The graph is acyclic (validateModules rejects cycles), so
// the memoized recursion terminates. Dependencies always have a strictly smaller
// depth, so stopping in descending depth order is dependency-safe.
func (run *kernelRun) assignDepths() {
	depths := make(map[string]int, len(run.runs))
	var visit func(id string) int
	visit = func(id string) int {
		if d, ok := depths[id]; ok {
			return d
		}
		r := run.rts[id]
		depth := 0
		for _, dep := range r.deps {
			if d := visit(dep) + 1; d > depth {
				depth = d
			}
		}
		r.depth = depth
		depths[id] = depth
		return depth
	}
	for _, r := range run.runs {
		visit(r.id)
	}
}

// buildStopLevels groups every module by depth. stopStarted walks the levels in
// descending order, stopping a whole wave concurrently. Not-yet-started modules
// are included; stopModule no-ops them.
func (run *kernelRun) buildStopLevels() {
	maxDepth := 0
	for _, r := range run.runs {
		if r.depth > maxDepth {
			maxDepth = r.depth
		}
	}
	run.stopLevels = make([][]*moduleRun, maxDepth+1)
	for _, r := range run.runs {
		run.stopLevels[r.depth] = append(run.stopLevels[r.depth], r)
	}
}

func (run *kernelRun) close() {
	if run.ticker != nil {
		run.ticker.Stop()
	}
	if run.ownsBus {
		// The kernel created this bus, so it owns its teardown and must reap the
		// delivery goroutines of any subscription a module forgot to remove.
		run.bus.Close()
		return
	}
	run.bus.Unsubscribe(kernelID)
}

func (run *kernelRun) startModules(ctx context.Context) {
	for _, r := range run.runs {
		go run.startModule(ctx, r)
	}
}

func (run *kernelRun) startModule(ctx context.Context, r *moduleRun) {
	if !run.waitDependencies(ctx, r) || !run.acquireSlot(ctx) {
		return
	}
	defer run.releaseSlot()

	start := time.Now()
	stop, err := r.module.Start(ctx)
	if err != nil {
		run.reportFailure(err)
		return
	}
	run.recordStarted(ctx, r, stop, time.Since(start))
}

func (run *kernelRun) waitDependencies(ctx context.Context, r *moduleRun) bool {
	for _, dep := range r.deps {
		select {
		case <-run.rts[dep].ready:
		case <-run.halt:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func (run *kernelRun) acquireSlot(ctx context.Context) bool {
	select {
	case run.slots <- struct{}{}:
		return true
	case <-run.halt:
		return false
	case <-ctx.Done():
		return false
	}
}

func (run *kernelRun) releaseSlot() { <-run.slots }

func (run *kernelRun) reportFailure(err error) {
	run.kmu.Lock()
	if run.failErr == nil {
		run.failErr = err
	}
	run.kmu.Unlock()
	run.signalWake()
}

func (run *kernelRun) failure() error {
	run.kmu.Lock()
	defer run.kmu.Unlock()
	return run.failErr
}

// The last module to start moves the kernel to Running; a module that finishes
// starting during shutdown is stopped right away instead.
func (run *kernelRun) recordStarted(ctx context.Context, r *moduleRun, stop StopFunc, took time.Duration) {
	doneAlready := run.markStarted(r, stop)
	allStarted := run.allStarted()

	if !run.publishStart(ctx, r, took) {
		_ = run.stopModule(ctx, r, time.Now().Add(run.cfg.stopTimeout))
		close(r.ready)
		return
	}
	close(r.ready)

	if allStarted {
		run.transition(ctx, KernelStateRunning, nil)
	}
	if r.task != nil && !doneAlready {
		go run.watchTask(r)
	}
	if run.isIdle() {
		run.signalWake()
	}
}

func (run *kernelRun) markStarted(r *moduleRun, stop StopFunc) bool {
	run.kmu.Lock()
	defer run.kmu.Unlock()
	r.started = true
	r.stop = stop
	r.order = run.startedCount
	run.startedCount++
	doneAlready := r.task != nil && isClosed(r.task.Done())
	if doneAlready {
		run.doneTasks++
	}
	return doneAlready
}

func (run *kernelRun) allStarted() bool {
	run.kmu.Lock()
	defer run.kmu.Unlock()
	return run.startedCount == len(run.runs)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (run *kernelRun) watchTask(r *moduleRun) {
	select {
	case <-r.task.Done():
	case <-run.halt:
		return
	}
	run.kmu.Lock()
	run.doneTasks++
	run.kmu.Unlock()
	if run.isIdle() {
		run.signalWake()
	}
}

func (run *kernelRun) isIdle() bool {
	run.kmu.Lock()
	defer run.kmu.Unlock()
	if run.doneTasks != run.totalTasks || run.totalTasks == 0 {
		return false
	}
	return run.cfg.exitWhenIdle || run.startedCount == len(run.runs)
}

func (run *kernelRun) signalWake() {
	select {
	case run.wake <- struct{}{}:
	default:
	}
}

type loopEvent int

const (
	eventCanceled loopEvent = iota
	eventFailed
	eventHealth
	eventTick
	eventIdle
)

func (run *kernelRun) wait(ctx context.Context) (loopEvent, Event, error) {
	select {
	case <-ctx.Done():
		return eventCanceled, Event{}, fmt.Errorf("kernel: %w", ctx.Err())
	case <-run.wake:
		if err := run.failure(); err != nil {
			return eventFailed, Event{}, err
		}
		return eventIdle, Event{}, nil
	case e, ok := <-run.health:
		if !ok {
			return eventFailed, Event{}, errHealthGone
		}
		return eventHealth, e, nil
	case <-run.tick():
		return eventTick, Event{}, nil
	}
}

func (run *kernelRun) loop(ctx context.Context) error {
	for {
		if err := run.checkHealth(); err != nil {
			return run.finish(ctx, err)
		}
		event, payload, err := run.wait(ctx)
		keepGoing, herr := run.handleEvent(ctx, event, payload, err)
		if !keepGoing {
			return herr
		}
	}
}

// Reports whether the loop should continue; when it should not, the returned error
// is Start's result.
func (run *kernelRun) handleEvent(ctx context.Context, event loopEvent, payload Event, err error) (bool, error) {
	switch event {
	case eventCanceled:
		return false, errors.Join(append(run.shutdown(ctx, err), err)...)
	case eventFailed:
		return false, run.finish(ctx, err)
	case eventHealth:
		if herr := run.handleHealth(payload); herr != nil {
			return false, run.finish(ctx, herr)
		}
	case eventTick:
		// health is re-checked at the top of the loop
	case eventIdle:
		// A NOK may be queued from before the idle wake but have lost the
		// select to it (both cases were ready). Drain health once more so an
		// idle completion can never mask a reported failure.
		if herr := run.checkHealth(); herr != nil {
			return false, run.finish(ctx, herr)
		}
		if run.isIdle() {
			return false, errors.Join(append(run.shutdown(ctx, nil),
				fmt.Errorf("kernel finished its work: %w", ErrKernelStopped))...)
		}
	}
	return true, nil
}

func (run *kernelRun) finish(ctx context.Context, err error) error {
	return errors.Join(append([]error{err}, run.shutdown(ctx, err)...)...)
}

// tick is nil when no health tick is configured, which disables the select case.
func (run *kernelRun) tick() <-chan time.Time {
	if run.ticker == nil {
		return nil
	}
	return run.ticker.C
}

// A closed subscription is surfaced so the loop can stop instead of spinning.
func (run *kernelRun) checkHealth() error {
	for {
		select {
		case e, ok := <-run.health:
			if !ok {
				return errHealthGone
			}
			if err := run.handleHealth(e); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (run *kernelRun) handleHealth(e Event) error {
	msc, ok := e.Payload.(ModuleStateChanged)
	if !ok {
		return nil
	}
	r, known := run.rts[e.Source()]
	if !known || msc.To != ModuleStateNOK {
		return nil
	}
	return KernelUnhealthyError{ModuleID: r.id, Cause: msc.Cause}
}

// transition publishes KernelStateChanged atomically with the state change, so the
// delivered event order always matches the lifecycle order.
func (run *kernelRun) transition(ctx context.Context, to KernelState, cause error) {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	if !canTransition(run.state, to) {
		return
	}
	from := run.state
	run.state = to
	_ = run.pub.Send(context.WithoutCancel(ctx), NewEvent(KernelStateChanged{From: from, To: to, Cause: cause}))
}

// canTransition encodes the lifecycle order: once shutdown has begun the only
// remaining move is to Stopped, so a late module can never revive the kernel.
func canTransition(from, to KernelState) bool {
	if from == to || from == KernelStateStopped {
		return false
	}
	if from == KernelStateStopping {
		return to == KernelStateStopped
	}
	return true
}

func (run *kernelRun) shutdown(ctx context.Context, cause error) []error {
	run.failHalt()
	run.transition(ctx, KernelStateStopping, cause)

	deadline := time.Now().Add(run.cfg.stopTimeout)
	errs := run.stopStarted(ctx, deadline)

	run.transition(ctx, KernelStateStopped, cause)
	return errs
}

func (run *kernelRun) stopStarted(ctx context.Context, deadline time.Time) []error {
	var (
		mu   sync.Mutex
		errs []error
	)
	collect := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	for _, level := range slices.Backward(run.stopLevels) {
		if run.workers <= 1 {
			for _, r := range level {
				collect(run.stopModule(ctx, r, deadline))
			}
			continue
		}
		// A module stuck in Start holds its start slot, so stops need their own
		// capacity. Every stopModule is deadline-bounded, so waiting for a slot
		// cannot outlive the shared deadline. A wave is joined before descending
		// so a dependency is never stopped before its dependents.
		var wg sync.WaitGroup
		for _, r := range level {
			wg.Go(func() {
				run.stopSlots <- struct{}{}
				defer func() { <-run.stopSlots }()
				collect(run.stopModule(ctx, r, deadline))
			})
		}
		wg.Wait()
	}
	return errs
}

// stopModule runs a module's StopFunc at most once, bounded by deadline. The
// StopFunc runs in its own goroutine so a StopFunc that ignores its context cannot
// hang shutdown; in that case the StopFunc goroutine is abandoned. The started/stop
// read happens before stopOnce so a module that has not started yet does not consume
// the once; a later call after it finishes starting can still stop it.
func (run *kernelRun) stopModule(ctx context.Context, r *moduleRun, deadline time.Time) error {
	run.kmu.Lock()
	stop, started := r.stop, r.started
	run.kmu.Unlock()
	if !started || stop == nil {
		return nil
	}
	var err error
	r.stopOnce.Do(func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Until(deadline))
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- stop(stopCtx) }()
		select {
		case err = <-done:
		case <-stopCtx.Done():
			err = fmt.Errorf("stop %q: %w", r.id, stopCtx.Err())
		}
	})
	return err
}

// publishStart publishes ModuleStarted while holding stateMu, so it can never be
// ordered after a Stopping/Stopped transition. It reports false when shutdown has
// already begun.
func (run *kernelRun) publishStart(ctx context.Context, r *moduleRun, took time.Duration) bool {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	if run.state == KernelStateStopping || run.state == KernelStateStopped {
		return false
	}
	_ = run.pub.Send(context.WithoutCancel(ctx), NewEvent(ModuleStarted{ID: r.id, Order: r.order, StartTook: took}))
	return true
}

func (run *kernelRun) failHalt() {
	run.haltOnce.Do(func() { close(run.halt) })
}

// KernelOption configures a Kernel.
type KernelOption func(*kernelConfig) error

// WithBus sets the Bus the kernel publishes on and subscribes to. When omitted,
// the kernel creates a Bus and closes it on shutdown; a Bus supplied here is not
// closed by the kernel, so the caller owns its teardown.
func WithBus(b *Bus) KernelOption {
	return func(cfg *kernelConfig) error {
		cfg.bus = b
		return nil
	}
}

// WithLogger sets the logger for the Bus the kernel creates when no Bus is
// supplied with [WithBus]. A supplied Bus keeps its own logger.
func WithLogger(l *slog.Logger) KernelOption {
	return func(cfg *kernelConfig) error {
		cfg.log = l
		return nil
	}
}

// WithModules registers the modules the kernel runs. Repeated calls accumulate.
func WithModules(modules ...Module) KernelOption {
	return func(cfg *kernelConfig) error {
		cfg.modules = append(cfg.modules, modules...)
		return nil
	}
}

// WithHealthTick sets how often the kernel re-drains queued health events, in
// addition to handling them as they arrive. The default is 0, which disables
// the periodic re-check.
func WithHealthTick(d time.Duration) KernelOption {
	return func(cfg *kernelConfig) error {
		if d < 0 {
			return errHealthTick
		}
		cfg.healthTick = d
		return nil
	}
}

// WithStopTimeout sets the total shutdown budget shared by all module StopFuncs.
// Each StopFunc receives a context carved out of this budget. The default is 5
// seconds.
func WithStopTimeout(d time.Duration) KernelOption {
	return func(cfg *kernelConfig) error {
		if d < 0 {
			return errStopTimeout
		}
		cfg.stopTimeout = d
		return nil
	}
}

// WithParallelism caps how many modules start concurrently, and how many stop
// concurrently within a wave. 1 is sequential; 0 (the default) means no cap.
// Stops always run in descending dependency-depth waves: every dependent is
// stopped before anything it depends on, while modules at the same depth stop
// concurrently.
func WithParallelism(p int) KernelOption {
	return func(cfg *kernelConfig) error {
		if p < 0 {
			return errParallelism
		}
		cfg.parallelism = p
		return nil
	}
}

// WithExitWhenIdle controls auto-shutdown. The kernel stops once there is at
// least one TaskModule and every TaskModule is done. When true, this happens even
// while long-running peers are still up. When false (the default), the kernel
// additionally waits until every module has started, so a graph still coming up
// is not cut short.
func WithExitWhenIdle(v bool) KernelOption {
	return func(cfg *kernelConfig) error {
		cfg.exitWhenIdle = v
		return nil
	}
}

type kernelConfig struct {
	bus          *Bus
	log          *slog.Logger
	modules      []Module
	healthTick   time.Duration
	stopTimeout  time.Duration
	parallelism  int
	exitWhenIdle bool
}

func validateModules(modules []Module) error {
	known, err := indexModules(modules)
	if err != nil {
		return err
	}
	if err := validateDependencies(modules, known); err != nil {
		return err
	}
	return detectCycle(known)
}

func indexModules(modules []Module) (map[string]Module, error) {
	known := make(map[string]Module, len(modules))
	for _, m := range modules {
		if m == nil {
			return nil, ErrNilModule
		}
		id := m.ID()
		if id == "" {
			return nil, ErrModuleIDNotSet
		}
		if id == kernelID {
			return nil, fmt.Errorf("%w: %q", ErrModuleIDReserved, id)
		}
		if _, exists := known[id]; exists {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateModuleID, id)
		}
		known[id] = m
	}
	return known, nil
}

func validateDependencies(modules []Module, known map[string]Module) error {
	for _, m := range modules {
		seen := map[string]bool{}
		for _, d := range m.Dependencies() {
			if d == m.ID() {
				return fmt.Errorf("%w: %q", ErrSelfDependency, m.ID())
			}
			if _, ok := known[d]; !ok {
				return fmt.Errorf("%w: %q depends on unknown %q", ErrUnknownDependency, m.ID(), d)
			}
			if seen[d] {
				return fmt.Errorf("%w: %q lists %q twice", ErrDuplicateDependency, m.ID(), d)
			}
			seen[d] = true
		}
	}
	return nil
}

const (
	colorUnvisited = iota
	colorVisiting
	colorDone
)

func detectCycle(known map[string]Module) error {
	color := map[string]int{}
	var path []string
	var visit func(id string) error
	visit = func(id string) error {
		switch color[id] {
		case colorVisiting:
			cycle := path
			for i, p := range path {
				if p == id {
					cycle = path[i:]
					break
				}
			}
			return fmt.Errorf("%w: %s", ErrCircularDependency, strings.Join(append(slices.Clone(cycle), id), " -> "))
		case colorDone:
			return nil
		}
		color[id] = colorVisiting
		path = append(path, id)
		for _, d := range known[id].Dependencies() {
			if err := visit(d); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		color[id] = colorDone
		return nil
	}
	for id := range known {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
