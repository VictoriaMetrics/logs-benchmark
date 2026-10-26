// Package synthetic generates log records, seeded so replaying the same seed
// yields the same records, and encodes them into several log formats.
package synthetic

import (
	"fmt"
	"math/rand/v2"
	"net/url"
	"strings"

	"github.com/brianvoe/gofakeit/v7"
)

// newFaker returns an unlocked PCG faker; every faker here is owned by a
// single goroutine.
func newFaker(seed int64) *gofakeit.Faker {
	return gofakeit.NewFaker(rand.NewPCG(uint64(seed), uint64(seed)), false)
}

// Field is one resolved constant field: a key and its fixed value.
type Field struct {
	Name    string
	Value   string
	Numeric bool // the value is an integer, encoded as a number
}

// HostInfo is one host of the fleet: its constant fields share keys across
// hosts, values are fixed per host.
type HostInfo struct {
	Name      string
	Consts    []Field
	ConstJSON []byte // Consts pre-encoded as `,"key":value` pairs
}

// AppInfo is one app of the fleet: a fixed slice of the field pool, split
// into constants (resolved once) and per-line fields (drawn per record).
type AppInfo struct {
	Name      string
	Consts    []Field
	ConstJSON []byte      // Consts pre-encoded as `,"key":value` pairs
	Lines     []PoolField // enum and dynamic fields; Lines[i] pairs with Record.LineVals[i]
}

// Fleet holds every host and app; a stream is one (host, app) pair. The same
// seed and counts always rebuild the same fleet.
type Fleet struct {
	Hosts []HostInfo
	Apps  []AppInfo
}

// AppNames returns the app names in fleet order.
func (fl *Fleet) AppNames() []string {
	names := make([]string, len(fl.Apps))
	for i := range fl.Apps {
		names[i] = fl.Apps[i].Name
	}
	return names
}

// NewFleet builds hostCount hosts and appCount apps, seeded by seed.
func NewFleet(seed int64, hostCount, appCount int) *Fleet {
	if hostCount <= 0 {
		panic(fmt.Sprintf("generator: hostCount must be positive, got %d", hostCount))
	}
	if appCount <= 0 {
		panic(fmt.Sprintf("generator: appCount must be positive, got %d", appCount))
	}
	f := newFaker(seed)
	fl := &Fleet{
		Hosts: make([]HostInfo, hostCount),
		Apps:  make([]AppInfo, appCount),
	}
	// Names combine a faker word with an index to make it unique.
	for i := range fl.Hosts {
		fl.Hosts[i].Name = fmt.Sprintf("%s-%02d", strings.ToLower(f.Word()), i)
	}
	for i := range fl.Apps {
		fl.Apps[i].Name = fmt.Sprintf("%s-%d", strings.ToLower(f.AppName()), i)
	}
	for i := range fl.Hosts {
		h := &fl.Hosts[i]
		for _, hf := range hostFields {
			h.Consts = append(h.Consts, Field{hf.Name, hf.values[f.Number(0, len(hf.values)-1)], hf.Numeric})
		}
		h.ConstJSON = constJSON(h.Consts)
	}
	for i := range fl.Apps {
		a := &fl.Apps[i]
		for _, pf := range appSchema(f) {
			if pf.kind == fieldConst {
				a.Consts = append(a.Consts, Field{pf.Name, pf.values[f.Number(0, len(pf.values)-1)], pf.Numeric})
			} else {
				a.Lines = append(a.Lines, pf)
			}
		}
		a.ConstJSON = constJSON(a.Consts)
	}
	return fl
}

// Generator produces log records deterministically: one seeded gofakeit stream,
// called in the same order, reproduces the same records.
type Generator struct {
	f  *gofakeit.Faker
	fl *Fleet
}

// New returns a generator drawing from fl, with per-record content seeded by
// seed. To parallelize generation, share one fleet across several generators,
// each with its own seed.
func New(fl *Fleet, seed int64) *Generator {
	if len(fl.Hosts) == 0 || len(fl.Apps) == 0 {
		panic("generator: fleet needs at least one host and one app")
	}
	return &Generator{
		f:  newFaker(seed),
		fl: fl,
	}
}

// Next produces the next record; the caller encodes it.
func (g *Generator) Next() Record {
	f := g.f
	rec := Record{
		Host:     &g.fl.Hosts[f.Number(0, len(g.fl.Hosts)-1)],
		App:      &g.fl.Apps[f.Number(0, len(g.fl.Apps)-1)],
		Level:    pickWeighted(f, logLevels, logLevelWeights),
		Priority: f.Number(0, 191),
		Message:  f.HackerPhrase(),
	}
	if lines := rec.App.Lines; len(lines) > 0 {
		rec.LineVals = make([]string, len(lines))
		for i := range lines {
			rec.LineVals[i] = lines[i].draw(f)
		}
	}
	return rec
}

func randResourceURI(f *gofakeit.Faker) string {
	var uri strings.Builder
	num := f.Number(1, 4)
	for range num {
		uri.WriteString("/")
		uri.WriteString(url.QueryEscape(f.BS()))
	}
	return strings.ToLower(uri.String())
}

func randAuthUserID(f *gofakeit.Faker) string {
	if f.Number(0, 1) == 0 {
		return "-"
	}
	return strings.ToLower(f.Username())
}
