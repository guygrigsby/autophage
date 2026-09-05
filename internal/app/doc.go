// Package app holds the application services: they load aggregates, call
// their methods and the ports, and commit. No domain rule lives here except
// the Scheduler, which is the domain service spanning cases (concurrency,
// order, removed repositories) and is kept beside the code that runs it.
package app
