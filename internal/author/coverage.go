//go:build linux

package author

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

// maxCoveringSeed bounds the search for covering seeds.
const maxCoveringSeed = 10000

// CoveringSeeds returns seeds whose frozen parameters together use every
// allowed value of every parameter of p, so validating with them certifies
// the problem for whichever value a session's seed later freezes (spec 7.1:
// representative seeds; per value, not every combination). Seeds are tried
// from 1 upwards and kept only when they add a value; a problem without
// parameters needs one seed. The result is deterministic.
func CoveringSeeds(p model.Problem) ([]uint64, error) {
	if len(p.Parameters) == 0 {
		return []uint64{1}, nil
	}
	covered := map[string]map[string]bool{}
	var seeds []uint64
	for seed := uint64(1); seed <= maxCoveringSeed; seed++ {
		params, err := frozenValues(p, seed)
		if err != nil {
			return nil, err
		}
		fresh := false
		for name, v := range params {
			if covered[name] == nil {
				covered[name] = map[string]bool{}
			}
			if !covered[name][v] {
				covered[name][v], fresh = true, true
			}
		}
		if fresh {
			seeds = append(seeds, seed)
		}
		if complete(p, covered) {
			return seeds, nil
		}
	}
	return nil, fmt.Errorf("author: no seeds up to %d cover every parameter value of %q", maxCoveringSeed, p.ID)
}

// UncoveredValues lists, as name=value (sorted), the allowed parameter values
// of p that none of seeds freezes.
func UncoveredValues(p model.Problem, seeds []uint64) ([]string, error) {
	covered := map[string]map[string]bool{}
	for _, seed := range seeds {
		params, err := frozenValues(p, seed)
		if err != nil {
			return nil, err
		}
		for name, v := range params {
			if covered[name] == nil {
				covered[name] = map[string]bool{}
			}
			covered[name][v] = true
		}
	}
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(p.Parameters)) {
		for _, v := range p.Parameters[name] {
			if key := valueKey(v); !covered[name][key] {
				missing = append(missing, name+"="+key)
			}
		}
	}
	return missing, nil
}

// UnvalidatedValues lists, as name=value (sorted), the parameter values that
// seed freezes for p but that none of the validated seeds froze: a session
// with that seed would serve a state the certificate never saw.
func UnvalidatedValues(p model.Problem, validated []uint64, seed uint64) ([]string, error) {
	want, err := frozenValues(p, seed)
	if err != nil {
		return nil, err
	}
	covered := map[string]bool{}
	for _, s := range validated {
		params, err := frozenValues(p, s)
		if err != nil {
			return nil, err
		}
		for name, v := range params {
			covered[name+"="+v] = true
		}
	}
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(want)) {
		if key := name + "=" + want[name]; !covered[key] {
			missing = append(missing, key)
		}
	}
	return missing, nil
}

// frozenValues is the parameter values seed freezes for p, each encoded as
// JSON (numbers and strings compare by value).
func frozenValues(p model.Problem, seed uint64) (map[string]string, error) {
	f, err := problem.Freeze(p, seed)
	if err != nil {
		return nil, fmt.Errorf("author: freeze %q with seed %d: %w", p.ID, seed, err)
	}
	out := make(map[string]string, len(f.Parameters))
	for name, v := range f.Parameters {
		out[name] = valueKey(v)
	}
	return out, nil
}

func valueKey(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func complete(p model.Problem, covered map[string]map[string]bool) bool {
	for name, values := range p.Parameters {
		for _, v := range values {
			if !covered[name][valueKey(v)] {
				return false
			}
		}
	}
	return true
}
