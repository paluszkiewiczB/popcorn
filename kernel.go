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
	errStopTimeout = errors.New("stop timeout must not be negative")
	errParallelism = errors.New("parallelism must not be negative")
	errHealthGone  = errors.New("kernel health stream closed")
)

// Kernel manages the lifecycle of a set of Modules. It starts modules in
// dependency order, publishes lifecycle events on the Bus, watches module
// health, and shuts everything down when the context is canceled, a module
// reports NOK, or every module is a TaskModule that has finished.
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
// module fails to start, a module reports NOK, or the kernel finishes its work
// (every module is a TaskModule and all are done). A module starts once every
// dependency has completed: a plain Module when its Start returns, a TaskModule
// when its Done closes.
//
// It returns a wrapped [ErrKernelStopped] after a graceful stop, whether ctx
// was canceled or the kernel finished its work, a [KernelUnhealthyError] when a
// registered module reported NOK, the error from a module's Start when it fails,
// or the joined errors from shutdown. A canceled stop also matches
// [context.Canceled] (or [context.DeadlineExceeded]) through [errors.Is], so the
// cause stays inspectable. Start may be called only once: a later call returns
// [ErrKernelStarted], even if the first call failed during setup.
func (k *Kernel) Start(ctx context.Context) error {
	if !k.begin() {
		return ErrKernelStarted
	}
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
}

func newKernelRun(ctx context.Context, cfg kernelConfig) (*kernelRun, error) {
	b := cfg.bus
	ownsBus := b == nil
	if ownsBus {
		b = newBus(busConfig{log: cfg.log, replayBuffer: defaultHistoryCap})
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
		run.transition(ctx, KernelStateStopped, err)
		return nil, err
	}
	run.health = health
	run.drainHealth()
	return run, nil
}

func (run *kernelRun) drainHealth() {
	for len(run.health) > 0 {
		<-run.health
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
	if run.ownsBus {
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
	if !run.waitDependencies(r) || !run.acquireSlot() {
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

func (run *kernelRun) waitDependencies(r *moduleRun) bool {
	for _, dep := range r.deps {
		d := run.rts[dep]
		select {
		case <-d.ready:
		case <-run.halt:
			return false
		}
		if d.task == nil {
			continue
		}
		select {
		case <-d.task.Done():
		case <-run.halt:
			return false
		}
	}
	return true
}

func (run *kernelRun) acquireSlot() bool {
	select {
	case run.slots <- struct{}{}:
		return true
	case <-run.halt:
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

func (run *kernelRun) recordStarted(ctx context.Context, r *moduleRun, stop StopFunc, took time.Duration) {
	run.markStarted(r, stop)
	last := r.order == len(run.runs)-1

	if !run.publishStart(ctx, r, took, last) {
		_ = run.stopModule(ctx, r, time.Now().Add(run.cfg.stopTimeout))
		close(r.ready)
		return
	}
	close(r.ready)

	if r.task != nil {
		go run.watchTask(ctx, r)
	}
}

func (run *kernelRun) markStarted(r *moduleRun, stop StopFunc) {
	run.kmu.Lock()
	defer run.kmu.Unlock()
	r.started = true
	r.stop = stop
	r.order = run.startedCount
	run.startedCount++
}

func (run *kernelRun) watchTask(ctx context.Context, r *moduleRun) {
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
	if err := run.stopModule(ctx, r, time.Now().Add(run.cfg.stopTimeout)); err != nil {
		run.cfg.log.Warn("task stop failed", "module", r.id, "error", err)
	}
}

func (run *kernelRun) isIdle() bool {
	run.kmu.Lock()
	defer run.kmu.Unlock()
	return run.totalTasks > 0 && run.totalTasks == len(run.runs) && run.doneTasks == run.totalTasks
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
	case e := <-run.health:
		return eventHealth, e, nil
	case <-run.cfg.healthTick:
		return eventTick, Event{}, nil
	}
}

func (run *kernelRun) loop(ctx context.Context) error {
	for {
		if err := run.checkHealth(); err != nil {
			return run.finish(ctx, err)
		}
		if run.isIdle() {
			return errors.Join(append(run.shutdown(ctx, nil),
				fmt.Errorf("kernel finished its work: %w", ErrKernelStopped))...)
		}
		event, payload, err := run.wait(ctx)
		keepGoing, herr := run.handleEvent(ctx, event, payload, err)
		if !keepGoing {
			return herr
		}
	}
}

func (run *kernelRun) handleEvent(ctx context.Context, event loopEvent, payload Event, err error) (bool, error) {
	switch event {
	case eventCanceled:
		return false, errors.Join(append(run.shutdown(ctx, err), err,
			fmt.Errorf("kernel stopped: %w", ErrKernelStopped))...)
	case eventFailed:
		return false, run.finish(ctx, err)
	case eventHealth:
		if herr := run.handleHealth(payload); herr != nil {
			return false, run.finish(ctx, herr)
		}
	case eventTick, eventIdle:
	}
	return true, nil
}

func (run *kernelRun) finish(ctx context.Context, err error) error {
	return errors.Join(append([]error{err}, run.shutdown(ctx, err)...)...)
}

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
	msc, _ := e.Payload.(ModuleStateChanged)
	r, known := run.rts[e.Source()]
	if !known {
		return nil
	}
	return KernelUnhealthyError{ModuleID: r.id, Cause: msc.Cause}
}

func (run *kernelRun) transition(ctx context.Context, to KernelState, cause error) {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	from := run.state
	run.state = to
	_ = run.pub.Send(context.WithoutCancel(ctx), NewEvent(KernelStateChanged{From: from, To: to, Cause: cause}))
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

func (run *kernelRun) publishStart(ctx context.Context, r *moduleRun, took time.Duration, last bool) bool {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	if run.state == KernelStateStopping || run.state == KernelStateStopped {
		return false
	}
	_ = run.pub.Send(context.WithoutCancel(ctx), NewEvent(ModuleStarted{ID: r.id, Order: r.order, StartTook: took}))
	if last {
		from := run.state
		run.state = KernelStateRunning
		running := NewEvent(KernelStateChanged{From: from, To: KernelStateRunning, Cause: nil})
		_ = run.pub.Send(context.WithoutCancel(ctx), running)
	}
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

// WithHealthTick injects a channel the kernel selects on to re-check health
// periodically, in addition to handling events as they arrive. A nil channel
// disables the check, which is the default. The caller owns the channel.
func WithHealthTick(ch <-chan time.Time) KernelOption {
	return func(cfg *kernelConfig) error {
		cfg.healthTick = ch
		return nil
	}
}

// WithStopTimeout sets the total shutdown budget shared by the StopFuncs of the
// shutdown waves. Each receives a context carved out of this budget; a
// TaskModule stopped on completion gets a fresh budget. The default is 5 seconds.
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
// Stops run in descending dependency-depth waves: every dependent is stopped
// before anything it depends on, while modules at the same depth stop
// concurrently. A TaskModule whose Done closes is stopped immediately instead,
// before its dependents.
func WithParallelism(p int) KernelOption {
	return func(cfg *kernelConfig) error {
		if p < 0 {
			return errParallelism
		}
		cfg.parallelism = p
		return nil
	}
}

type kernelConfig struct {
	bus         *Bus
	log         *slog.Logger
	modules     []Module
	healthTick  <-chan time.Time
	stopTimeout time.Duration
	parallelism int
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
