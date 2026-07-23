// Package popcorn is a tiny Go microframework for building modular applications.
//
// The core type is [Kernel], which manages the lifecycle of [Module]s. Modules
// are self-contained units that expose an ID, a list of dependencies, and a
// Start function that returns a [StopFunc]. The kernel starts modules in
// dependency order, routes events through a [Bus], watches health, and shuts
// everything down cleanly.
//
// For finite work, a module can implement [TaskModule]. When all task modules
// are done, the kernel initiates graceful shutdown, allowing the framework to
// support both long-running services and CLI-style applications.
//
// See the README for a quick-start example.
package popcorn
