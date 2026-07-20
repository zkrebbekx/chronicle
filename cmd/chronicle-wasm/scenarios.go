//go:build js && wasm

package main

import "time"

// A scenario scripts a short, deterministic story onto a fresh log so a
// visitor can read the bitemporal picture straight away. Each sets the
// transaction clock explicitly before each write, which is what places a
// correction at a later transaction instant than the belief it revises.
type scenario struct {
	Name   string
	Title  string
	Blurb  string
	Kind   string
	Entity string
	Path   string // a field worth watching in the field-history panel
	Note   string // one line shown after loading, explaining what to look for

	FocusValid time.Time // where to drop the as-of crosshair on the valid axis
	FocusTx    time.Time // zero means "now" (the top of the grid)

	run func(*engine) error
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

var scenarios = map[string]*scenario{
	"salary": {
		Name:       "salary",
		Title:      "Retroactive salary correction",
		Blurb:      "A raise recorded on time, then an old figure corrected months later. The flagship: two beliefs about March coexist.",
		Kind:       "employee",
		Entity:     "alice",
		Path:       "/salary",
		Note:       "The left column (Mar–Jun) is split in transaction time: 100 was believed until July, then corrected to 110. The right column (Jun onward) is the real raise to 120. Drag the transaction slider down to watch the July correction disappear.",
		FocusValid: day(2024, time.April, 1),
		run: func(e *engine) error {
			// March: HR records Alice's salary, effective March 1.
			e.clock.Set(day(2024, time.March, 15))
			if _, err := e.write(false, "employee", "alice",
				[]byte(`{"salary":100}`), day(2024, time.March, 1), time.Time{},
				"Dana (HR)", "Initial salary on record"); err != nil {
				return err
			}
			// June: a genuine merit raise, effective June 1. Splits the timeline.
			e.clock.Set(day(2024, time.June, 10))
			if _, err := e.write(false, "employee", "alice",
				[]byte(`{"salary":120}`), day(2024, time.June, 1), time.Time{},
				"Dana (HR)", "Merit raise effective June 1"); err != nil {
				return err
			}
			// July: we discover March–May was mis-entered — it was 110, not 100.
			e.clock.Set(day(2024, time.July, 5))
			if _, err := e.write(true, "employee", "alice",
				[]byte(`{"salary":110}`), day(2024, time.March, 1), day(2024, time.June, 1),
				"Reyes (Payroll)", "Mar-May was 110, not 100 (data-entry error)"); err != nil {
				return err
			}
			return nil
		},
	},

	"address": {
		Name:       "address",
		Title:      "A late-learned move",
		Blurb:      "No correction — just a fact that became true later. Two adjacent valid-time boxes, one belief band.",
		Kind:       "person",
		Entity:     "bob",
		Path:       "/city",
		Note:       "Bob's address splits on the valid axis at September, not the transaction axis: both boxes are current belief. This is an ordinary change over time, distinct from a retroactive correction.",
		FocusValid: day(2024, time.October, 1),
		run: func(e *engine) error {
			e.clock.Set(day(2024, time.February, 1))
			if _, err := e.write(false, "person", "bob",
				[]byte(`{"city":"Denver","state":"CO"}`), day(2020, time.January, 1), time.Time{},
				"Sam (Records)", "Address on file"); err != nil {
				return err
			}
			e.clock.Set(day(2024, time.September, 15))
			if _, err := e.write(false, "person", "bob",
				[]byte(`{"city":"Austin","state":"TX"}`), day(2024, time.September, 1), time.Time{},
				"Sam (Records)", "Moved to Austin, effective September 1"); err != nil {
				return err
			}
			return nil
		},
	},

	"bonus": {
		Name:       "bonus",
		Title:      "A field that was recorded in error",
		Blurb:      "A bonus asserted, then dropped by a correction. The field-history panel shows absent to 10 to absent.",
		Kind:       "employee",
		Entity:     "carol",
		Path:       "/bonus",
		Note:       "Pick /bonus in the field-history panel: it appears (absent to 10) as an assertion, then disappears (10 to absent) as a correction. An absent field is not the same as a field set to null, and chronicle keeps them distinct.",
		FocusValid: day(2024, time.June, 1),
		run: func(e *engine) error {
			e.clock.Set(day(2024, time.April, 1))
			if _, err := e.write(false, "employee", "carol",
				[]byte(`{"salary":90,"bonus":10}`), day(2024, time.January, 1), time.Time{},
				"Dana (HR)", "Compensation on record"); err != nil {
				return err
			}
			e.clock.Set(day(2024, time.May, 1))
			if _, err := e.write(true, "employee", "carol",
				[]byte(`{"salary":90}`), day(2024, time.January, 1), time.Time{},
				"Reyes (Payroll)", "Bonus was recorded in error; removed"); err != nil {
				return err
			}
			return nil
		},
	},
}

// order fixes the presentation order of scenarios in the UI, flagship first.
var scenarioOrder = []string{"salary", "address", "bonus"}

func scenarioList() []map[string]any {
	out := make([]map[string]any, 0, len(scenarioOrder))
	for _, name := range scenarioOrder {
		sc, ok := scenarios[name]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"name":   sc.Name,
			"title":  sc.Title,
			"blurb":  sc.Blurb,
			"kind":   sc.Kind,
			"entity": sc.Entity,
			"path":   sc.Path,
		})
	}
	return out
}
