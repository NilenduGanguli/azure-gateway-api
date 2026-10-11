package realcut

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
)

// Diff is one difference between two JSON trees.
type Diff struct {
	Path string `json:"path"`
	Want string `json:"want"`
	Got  string `json:"got"`
}

func short(b []byte) string {
	if len(b) > 160 {
		return string(b[:160]) + "…"
	}
	return string(b)
}

// DiffNodes lists differences between want and got, at most limit of them.
func DiffNodes(want, got *Node, path string, out *[]Diff, limit int) {
	if len(*out) >= limit {
		return
	}
	if want.Kind != got.Kind {
		*out = append(*out, Diff{path, short(Encode(want)), short(Encode(got))})
		return
	}
	switch want.Kind {
	case Scalar:
		if !bytes.Equal(want.Raw, got.Raw) {
			*out = append(*out, Diff{path, short(want.Raw), short(got.Raw)})
		}
	case Obj:
		if !slices.Equal(want.Keys, got.Keys) {
			*out = append(*out, Diff{path + " (member order)", fmt.Sprint(want.Keys), fmt.Sprint(got.Keys)})
		}
		for i, k := range want.Keys {
			if g := got.Get(k); g != nil {
				DiffNodes(want.Vals[i], g, path+"."+k, out, limit)
			}
		}
	case Arr:
		if len(want.Vals) != len(got.Vals) {
			*out = append(*out, Diff{path + " (length)", strconv.Itoa(len(want.Vals)), strconv.Itoa(len(got.Vals))})
		}
		for i := 0; i < min(len(want.Vals), len(got.Vals)); i++ {
			DiffNodes(want.Vals[i], got.Vals[i], fmt.Sprintf("%s[%d]", path, i), out, limit)
		}
	}
}

// MemberReport compares two analyzeResults member by member.
type MemberReport struct {
	Member    string `json:"member"`
	Identical bool   `json:"identical"`
	Diffs     []Diff `json:"diffs,omitempty"`
	NDiffs    int    `json:"nDiffs,omitempty"`
}

// CompareMembers compares want and got (bare analyzeResults) member by member.
func CompareMembers(want, got *Node) (bool, []MemberReport, []string, []string) {
	same := bytes.Equal(Encode(want), Encode(got))
	var reps []MemberReport
	for i, k := range want.Keys {
		g := got.Get(k)
		r := MemberReport{Member: k}
		if g == nil {
			r.Diffs = []Diff{{Path: k, Want: "present", Got: "absent"}}
			r.NDiffs = 1
			reps = append(reps, r)
			continue
		}
		if bytes.Equal(Encode(want.Vals[i]), Encode(g)) {
			r.Identical = true
		} else {
			var all []Diff
			DiffNodes(want.Vals[i], g, k, &all, 100000)
			r.NDiffs = len(all)
			r.Diffs = all[:min(len(all), 12)]
		}
		reps = append(reps, r)
	}
	for _, k := range got.Keys {
		if want.Get(k) == nil {
			reps = append(reps, MemberReport{Member: k, Diffs: []Diff{{Path: k, Want: "absent", Got: "present"}}, NDiffs: 1})
		}
	}
	return same, reps, want.Keys, got.Keys
}
