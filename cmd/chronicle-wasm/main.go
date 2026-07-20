//go:build js && wasm

// Command chronicle-wasm is the browser playground's engine: the chronicle
// library compiled to WebAssembly over an in-memory store, exposing a small
// JSON API on the global "chronicle" object. It has no backend — one
// chronicle.Log lives in the visitor's tab and resets on reload.
//
// The transaction clock is a chronicle.FixedClock the page controls through
// setClock, so the demo can place a write at a chosen transaction instant and
// show a retroactive correction as two coexisting beliefs. That is a teaching
// affordance only: in a real deployment transaction time is system-assigned
// and no caller can write it. The library forbids writing TxFrom/TxTo whatever
// clock is injected — an injected clock can slow the transaction axis but never
// rewind it, because every write is ratcheted strictly forward.
//
// This file carries the js && wasm build constraint so the native toolchain —
// go build ./..., go vet ./..., go test ./..., golangci-lint — never tries to
// compile it (syscall/js exists only under GOOS=js). The root module stays
// zero-dependency: syscall/js is stdlib and adds nothing to go.sum.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"syscall/js"
	"time"

	"github.com/zkrebbekx/chronicle"
)

// baseClock is the transaction instant a freshly reset log starts at. Manual
// writes that do not set the clock land here (ratcheted forward per write);
// scenarios set their own instants.
var baseClock = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

var ctx = context.Background()

// engine holds the single in-memory log the whole page shares. WebAssembly is
// single-threaded and JS callbacks are serialized on the event loop, so the
// mutex only guards against a reset landing between a write's own steps.
type engine struct {
	mu    sync.Mutex
	log   *chronicle.Log
	store *chronicle.MemStore
	clock *chronicle.FixedClock
}

var st engine

// rebuild replaces the log with a fresh, empty one. The caller holds st.mu.
func (e *engine) rebuild() {
	e.clock = chronicle.NewFixedClock(baseClock)
	e.store = chronicle.NewMemStore()
	e.log = chronicle.NewLog(e.store, chronicle.WithClock(e.clock))
}

func main() {
	st.mu.Lock()
	st.rebuild()
	st.mu.Unlock()

	obj := map[string]any{}
	register(obj, "setClock", handleSetClock)
	register(obj, "put", handlePut(false))
	register(obj, "correct", handlePut(true))
	register(obj, "get", handleGet)
	register(obj, "timeline", handleTimeline)
	register(obj, "history", handleHistory)
	register(obj, "fieldHistory", handleFieldHistory)
	register(obj, "query", handleQuery)
	register(obj, "reset", handleReset)
	register(obj, "loadScenario", handleLoadScenario)
	obj["scenarios"] = toJS(scenarioList())

	js.Global().Set("chronicle", js.ValueOf(obj))
	js.Global().Set("__chronicleReady", js.ValueOf(true))
	if cb := js.Global().Get("__chronicleOnReady"); cb.Type() == js.TypeFunction {
		cb.Invoke()
	}

	// Keep the Go runtime alive for the lifetime of the page.
	select {}
}

// register wraps a handler so every call recovers from a panic into a returned
// {error} rather than tearing down the wasm instance, and marshals the result
// to a real JS object.
func register(obj map[string]any, name string, fn func([]byte) (any, error)) {
	obj[name] = js.FuncOf(func(_ js.Value, args []js.Value) (result any) {
		defer func() {
			if r := recover(); r != nil {
				result = toJS(map[string]any{"error": fmt.Sprintf("panic: %v", r), "code": "panic"})
			}
		}()
		in := input(args)
		out, err := fn(in)
		if err != nil {
			return toJS(errObj(err))
		}
		return toJS(out)
	})
}

// input normalises the first argument to JSON bytes. A JS object is
// stringified; a bare string is passed through verbatim (setClock and
// loadScenario accept one); anything absent becomes an empty object.
func input(args []js.Value) []byte {
	if len(args) == 0 {
		return []byte("{}")
	}
	a := args[0]
	switch a.Type() {
	case js.TypeString:
		return []byte(a.String())
	case js.TypeUndefined, js.TypeNull:
		return []byte("{}")
	default:
		return []byte(js.Global().Get("JSON").Call("stringify", a).String())
	}
}

// toJS marshals a Go value and parses it back into a native JS object so the
// page receives structured data rather than a string to parse itself.
func toJS(v any) js.Value {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]any{"error": err.Error(), "code": "marshal"})
	}
	return js.Global().Get("JSON").Call("parse", string(b))
}

// errObj renders an error as the page's error shape, mapping chronicle's
// sentinels loosely onto stable string codes.
func errObj(err error) map[string]any {
	return map[string]any{"error": err.Error(), "code": errCode(err)}
}

func errCode(err error) string {
	switch {
	case errors.Is(err, chronicle.ErrNotFound):
		return "not_found"
	case errors.Is(err, chronicle.ErrInvalidInterval):
		return "invalid_interval"
	case errors.Is(err, chronicle.ErrMissingActor):
		return "missing_actor"
	case errors.Is(err, chronicle.ErrUnknownKind):
		return "unknown_kind"
	case errors.Is(err, chronicle.ErrMissingEntityID):
		return "missing_entity"
	case errors.Is(err, chronicle.ErrInvalidPath):
		return "invalid_path"
	case errors.Is(err, chronicle.ErrCodec):
		return "codec"
	case errors.Is(err, chronicle.ErrInvalidMeta), errors.Is(err, chronicle.ErrInvalidField):
		return "invalid_field"
	default:
		return "error"
	}
}

// --- JSON shapes returned to the page ---------------------------------------

type actorJSON struct {
	ID   string `json:"id"`
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
}

type recordJSON struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Entity    string          `json:"entity"`
	Data      json.RawMessage `json:"data"`
	ValidFrom string          `json:"validFrom"`
	ValidTo   string          `json:"validTo"`
	TxFrom    string          `json:"txFrom"`
	TxTo      string          `json:"txTo"`
	Current   bool            `json:"current"`
	Actor     actorJSON       `json:"actor"`
	Reason    string          `json:"reason,omitempty"`
	Intent    string          `json:"intent"`
}

func recJSON(r chronicle.Record) recordJSON {
	data := r.Data
	if len(data) == 0 {
		data = json.RawMessage("null")
	}
	return recordJSON{
		ID:        string(r.ID),
		Kind:      r.Kind,
		Entity:    r.EntityID,
		Data:      json.RawMessage(data),
		ValidFrom: fmtTime(r.ValidFrom),
		ValidTo:   fmtTime(r.ValidTo),
		TxFrom:    fmtTime(r.TxFrom),
		TxTo:      fmtTime(r.TxTo),
		Current:   r.IsCurrent(),
		Actor:     actorJSON{ID: r.Actor.ID, Type: r.Actor.Type, Name: r.Actor.Name},
		Reason:    r.Reason,
		Intent:    r.Intent.String(),
	}
}

func recsJSON(rs []chronicle.Record) []recordJSON {
	out := make([]recordJSON, len(rs))
	for i, r := range rs {
		out[i] = recJSON(r)
	}
	return out
}

// fmtTime renders an instant as RFC 3339, or "" for the zero time, which on
// either axis is an unbounded end.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// parseTime accepts RFC 3339, a bare date, or "" (the unbounded / zero time).
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("cannot parse time %q (want RFC 3339 or YYYY-MM-DD)", s)
}

// --- write handlers ---------------------------------------------------------

type putIn struct {
	Kind      string          `json:"kind"`
	Entity    string          `json:"entity"`
	Data      json.RawMessage `json:"data"`
	ValidFrom string          `json:"validFrom"`
	ValidTo   string          `json:"validTo"`
	Reason    string          `json:"reason"`
	Actor     string          `json:"actor"`
}

func handlePut(correct bool) func([]byte) (any, error) {
	return func(in []byte) (any, error) {
		var p putIn
		if err := json.Unmarshal(in, &p); err != nil {
			return nil, err
		}
		vf, err := parseTime(p.ValidFrom)
		if err != nil {
			return nil, fmt.Errorf("validFrom: %w", err)
		}
		vt, err := parseTime(p.ValidTo)
		if err != nil {
			return nil, fmt.Errorf("validTo: %w", err)
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		res, err := st.write(correct, p.Kind, p.Entity, dataBytes(p.Data), vf, vt, p.Actor, p.Reason)
		if err != nil {
			return nil, err
		}
		return resultJSON(res), nil
	}
}

// write is the unlocked write path shared by the exported handlers and the
// scenario scripts. The caller holds st.mu.
func (e *engine) write(correct bool, kind, entity string, data []byte, vf, vt time.Time, actor, reason string) (chronicle.Result, error) {
	who := chronicle.Actor{ID: actor, Name: actor, Type: "user"}
	var opts []chronicle.WriteOption
	if reason != "" {
		opts = append(opts, chronicle.WithReason(reason))
	}
	if correct {
		return e.log.Correct(ctx, kind, entity, data, vf, vt, who, opts...)
	}
	return e.log.Put(ctx, kind, entity, data, vf, vt, who, opts...)
}

func dataBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return []byte(raw)
}

func resultJSON(res chronicle.Result) map[string]any {
	superseded := make([]string, len(res.Superseded))
	for i, id := range res.Superseded {
		superseded[i] = string(id)
	}
	return map[string]any{
		"txAt":       fmtTime(res.TxAt),
		"record":     recJSON(res.Record),
		"written":    recsJSON(res.Written),
		"superseded": superseded,
	}
}

// --- read handlers ----------------------------------------------------------

type getIn struct {
	Kind    string `json:"kind"`
	Entity  string `json:"entity"`
	ValidAt string `json:"validAt"`
	TxAt    string `json:"txAt"`
}

func handleGet(in []byte) (any, error) {
	var g getIn
	if err := json.Unmarshal(in, &g); err != nil {
		return nil, err
	}
	va, err := parseTime(g.ValidAt)
	if err != nil {
		return nil, fmt.Errorf("validAt: %w", err)
	}
	ta, err := parseTime(g.TxAt)
	if err != nil {
		return nil, fmt.Errorf("txAt: %w", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	rec, err := st.log.Get(ctx, g.Kind, g.Entity, chronicle.As{ValidAt: va, TxAt: ta})
	if errors.Is(err, chronicle.ErrNotFound) {
		// Not an error to the page: an empty cell is a fact, not a failure.
		return map[string]any{"found": false}, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"found": true, "record": recJSON(*rec)}, nil
}

func handleTimeline(in []byte) (any, error) {
	var g getIn
	if err := json.Unmarshal(in, &g); err != nil {
		return nil, err
	}
	ta, err := parseTime(g.TxAt)
	if err != nil {
		return nil, fmt.Errorf("txAt: %w", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	recs, err := st.log.Timeline(ctx, g.Kind, g.Entity, chronicle.As{TxAt: ta})
	if err != nil {
		return nil, err
	}
	return map[string]any{"records": recsJSON(recs)}, nil
}

func handleHistory(in []byte) (any, error) {
	var g getIn
	if err := json.Unmarshal(in, &g); err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	recs, err := st.log.History(ctx, g.Kind, g.Entity)
	if err != nil {
		return nil, err
	}
	return map[string]any{"records": recsJSON(recs)}, nil
}

type fieldHistoryIn struct {
	Kind    string `json:"kind"`
	Entity  string `json:"entity"`
	Path    string `json:"path"`
	ValidAt string `json:"validAt"`
}

type fieldValueJSON struct {
	Present bool `json:"present"`
	Value   any  `json:"value,omitempty"`
}

func fvJSON(v chronicle.FieldValue) fieldValueJSON {
	return fieldValueJSON{Present: v.Present, Value: v.Value}
}

func handleFieldHistory(in []byte) (any, error) {
	var f fieldHistoryIn
	if err := json.Unmarshal(in, &f); err != nil {
		return nil, err
	}
	va, err := parseTime(f.ValidAt)
	if err != nil {
		return nil, fmt.Errorf("validAt: %w", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	revs, err := st.log.FieldHistory(ctx, f.Kind, f.Entity, f.Path, chronicle.As{ValidAt: va})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(revs))
	for i, r := range revs {
		out[i] = map[string]any{
			"path":      r.Path,
			"from":      fvJSON(r.From),
			"to":        fvJSON(r.To),
			"txAt":      fmtTime(r.TxAt),
			"validFrom": fmtTime(r.ValidFrom),
			"validTo":   fmtTime(r.ValidTo),
			"actor":     actorJSON{ID: r.Actor.ID, Type: r.Actor.Type, Name: r.Actor.Name},
			"reason":    r.Reason,
			"intent":    r.Intent.String(),
		}
	}
	return map[string]any{"revisions": out}, nil
}

type queryIn struct {
	Kind        string `json:"kind"`
	Entity      string `json:"entity"`
	Actor       string `json:"actor"`
	Intent      string `json:"intent"`
	ValidAt     string `json:"validAt"`
	TxAt        string `json:"txAt"`
	CurrentOnly bool   `json:"currentOnly"`
	Descending  bool   `json:"descending"`
	Limit       int    `json:"limit"`
}

func handleQuery(in []byte) (any, error) {
	var q queryIn
	if err := json.Unmarshal(in, &q); err != nil {
		return nil, err
	}
	va, err := parseTime(q.ValidAt)
	if err != nil {
		return nil, fmt.Errorf("validAt: %w", err)
	}
	ta, err := parseTime(q.TxAt)
	if err != nil {
		return nil, fmt.Errorf("txAt: %w", err)
	}
	cq := chronicle.Query{
		Kind:        q.Kind,
		EntityID:    q.Entity,
		ActorID:     q.Actor,
		ValidAt:     va,
		TxAt:        ta,
		CurrentOnly: q.CurrentOnly,
		Descending:  q.Descending,
		Limit:       q.Limit,
	}
	if q.Limit <= 0 {
		cq.Limit = 500
	}
	switch q.Intent {
	case "assert":
		cq.HasIntent, cq.Intent = true, chronicle.IntentAssert
	case "correction":
		cq.HasIntent, cq.Intent = true, chronicle.IntentCorrection
	case "remainder":
		cq.HasIntent, cq.Intent = true, chronicle.IntentRemainder
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	recs, cursor, err := st.log.Query(ctx, cq)
	if err != nil {
		return nil, err
	}
	return map[string]any{"records": recsJSON(recs), "cursor": string(cursor)}, nil
}

// --- clock, reset, scenarios ------------------------------------------------

type clockIn struct {
	T     string `json:"t"`
	Clock string `json:"clock"`
}

func handleSetClock(in []byte) (any, error) {
	s := string(in)
	// A bare RFC 3339 / date string was passed straight through by input().
	if t, err := parseTime(s); err == nil && s != "" && s != "{}" {
		st.mu.Lock()
		st.clock.Set(t)
		st.mu.Unlock()
		return map[string]any{"clock": fmtTime(t)}, nil
	}
	var c clockIn
	if err := json.Unmarshal(in, &c); err != nil {
		return nil, err
	}
	raw := c.T
	if raw == "" {
		raw = c.Clock
	}
	t, err := parseTime(raw)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	st.clock.Set(t)
	st.mu.Unlock()
	return map[string]any{"clock": fmtTime(t)}, nil
}

func handleReset(_ []byte) (any, error) {
	st.mu.Lock()
	st.rebuild()
	st.mu.Unlock()
	return map[string]any{"ok": true, "clock": fmtTime(baseClock)}, nil
}

type scenarioIn struct {
	Name string `json:"name"`
}

func handleLoadScenario(in []byte) (any, error) {
	name := string(in)
	if name == "" || name == "{}" || name[0] == '{' {
		var s scenarioIn
		if err := json.Unmarshal(in, &s); err == nil && s.Name != "" {
			name = s.Name
		}
	}
	sc, ok := scenarios[name]
	if !ok {
		return nil, fmt.Errorf("unknown scenario %q", name)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.rebuild()
	if err := sc.run(&st); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":     true,
		"name":   name,
		"title":  sc.Title,
		"kind":   sc.Kind,
		"entity": sc.Entity,
		"path":   sc.Path,
		"focus":  map[string]any{"validAt": fmtTime(sc.FocusValid), "txAt": fmtTime(sc.FocusTx)},
		"note":   sc.Note,
	}, nil
}
