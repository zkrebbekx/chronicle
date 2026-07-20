package chroniclefest_test

import (
	"fmt"
	"strings"
	"sync"

	"github.com/zkrebbekx/chronicle/chroniclefest"
)

// recorder is a [chroniclefest.T] that writes failures down instead of failing
// a real test. It is what lets this package assert that the conformance suite
// *fails* against a store that violates the contract, which is the only
// evidence that the suite's failure branches say what they claim to.
//
// It mirrors testing's control flow closely enough for the suite to behave the
// same: Fatal and Fatalf do not return, and a failure inside a subtest ends
// that subtest without touching its siblings or its parent.
type recorder struct {
	shared *recorded
	name   string

	cleanups []func()
}

// recorded is the state shared by every recorder in one run.
type recorded struct {
	mu       sync.Mutex
	failures []failure
}

// failure is one reported failure: the full subtest path it happened under,
// and the message.
type failure struct {
	Test string
	Msg  string
}

func (f failure) String() string { return f.Test + ": " + f.Msg }

// abort is what Fatal panics with, so that the suite's "this does not return"
// assumption holds. Run recovers it at the subtest boundary.
type abort struct{}

func newRecorder(name string) *recorder {
	return &recorder{shared: &recorded{}, name: name}
}

func (r *recorder) Helper()      {}
func (r *recorder) Name() string { return r.name }

func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recorder) Errorf(format string, args ...any) {
	r.report(fmt.Sprintf(format, args...))
}

func (r *recorder) Fatal(args ...any) {
	r.report(fmt.Sprint(args...))
	panic(abort{})
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.report(fmt.Sprintf(format, args...))
	panic(abort{})
}

func (r *recorder) report(msg string) {
	r.shared.mu.Lock()
	defer r.shared.mu.Unlock()
	r.shared.failures = append(r.shared.failures, failure{Test: r.name, Msg: msg})
}

// Run runs f as a subtest. A Fatal inside f unwinds to here and no further,
// which is what *testing.T does with runtime.Goexit.
func (r *recorder) Run(name string, f func(chroniclefest.T)) bool {
	sub := &recorder{shared: r.shared, name: r.name + "/" + name}
	before := r.count()
	sub.protect(func() { f(sub) })
	sub.runCleanups()
	return r.count() == before
}

// protect runs f, swallowing an abort panic and letting any other panic
// through — a store that panics is a failure the harness must not hide.
func (r *recorder) protect(f func()) {
	defer func() {
		if p := recover(); p != nil {
			if _, ok := p.(abort); ok {
				return
			}
			panic(p)
		}
	}()
	f()
}

// runCleanups runs registered cleanups last-in-first-out, as testing does,
// protecting each so that a Fatal in one still lets the rest run.
func (r *recorder) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.protect(r.cleanups[i])
	}
	r.cleanups = nil
}

func (r *recorder) count() int {
	r.shared.mu.Lock()
	defer r.shared.mu.Unlock()
	return len(r.shared.failures)
}

func (r *recorder) failures() []failure {
	r.shared.mu.Lock()
	defer r.shared.mu.Unlock()
	return append([]failure(nil), r.shared.failures...)
}

// matched reports whether any failure's subtest path or message contains want.
// Matching on the check rather than merely on "something failed" is the whole
// point: a suite that fails for an unrelated reason would otherwise look like
// it caught the injected fault.
func (r *recorder) matched(want string) bool {
	for _, f := range r.failures() {
		if strings.Contains(f.Test, want) || strings.Contains(f.Msg, want) {
			return true
		}
	}
	return false
}

// summary renders the failures for a diagnostic, capped so that a store that
// fails hundreds of checks does not bury the one line that matters.
func (r *recorder) summary() string {
	fs := r.failures()
	const maxShown = 12
	var b strings.Builder
	fmt.Fprintf(&b, "%d failure(s)", len(fs))
	for i, f := range fs {
		if i == maxShown {
			fmt.Fprintf(&b, "\n\t... and %d more", len(fs)-maxShown)
			break
		}
		b.WriteString("\n\t" + f.String())
	}
	return b.String()
}

// runWith drives fn against a fresh recorder and returns it.
func runWith(fn func(chroniclefest.T)) *recorder {
	r := newRecorder("fest")
	r.protect(func() { fn(r) })
	r.runCleanups()
	return r
}

// run drives the whole conformance suite against newStore and returns the
// recorder holding whatever it reported. It is the entry point the fault tests
// use.
func run(newStore chroniclefest.Factory) *recorder {
	return runWith(func(t chroniclefest.T) { chroniclefest.RunT(t, newStore) })
}
