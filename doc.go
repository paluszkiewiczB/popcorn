// Package popcorn is a microframework for building modular applications: a
// dependency-ordered [Kernel] driving [Module]s, and an event [Bus] as the
// single communication mechanism — including health, which is just an event.
//
// Design philosophy:
//
//   - One owner per channel. The Bus creates, writes, and closes subscription
//     channels; modules only read them. No module ever writes to a channel it does
//     not own, and no module closes a bus-owned channel.
//   - One communication mechanism. The event Bus covers module->kernel,
//     module->module, and kernel->module traffic. Health is just an event.
//   - Explicit subscriptions. A module subscribes itself, when it is ready, via the
//     injected Bus. There are no receiver interfaces and no kernel runtime checks.
//   - Identity is attached. Publishers are bound to an id, so Event.Source is set by
//     the Bus and Send skips the sender's own subscription. The kernel only trusts
//     health from ids it has registered.
//   - Bounded everything. Each subscription has a bounded ring and the Bus keeps a
//     bounded replay history. A slow subscriber can only overflow its own ring.
//   - Health is best-effort. It travels on the shared Bus like any other event.
//   - Events are lightweight values. No pointers, no per-event wrappers.
package popcorn
