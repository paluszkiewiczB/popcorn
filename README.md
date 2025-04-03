# popcorn

Popcorn is a microframework following the micro kernel architecture.

It's core takes care of a proper bootstrap and shutdown of your application, and does not care about functionality of your app.

It does not matter whether you're going to have the HTTP server or CLI tool.

Popcorn Modules are ment to be self-containing plugins, which could depend on each other (without circular dependencies!).

Popcorn Core exposes its current state (liveness, readiness) through the probes and events, which could be consumed by your Module.

Popcorn Events can also be used as an asynchronous communication mechanism between Modules.
