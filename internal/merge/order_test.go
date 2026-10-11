package merge

import (
	"strings"
	"testing"
)

func TestMergeOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lists []string // each list's names, space-separated
		want  string   // "" when the lists contradict each other
	}{
		{"one list", []string{"a b c"}, "a b c"},
		{"identical lists", []string{"a b c", "a b c"}, "a b c"},
		{"a later list adds a name in the middle",
			[]string{"api tables sections cf", "api tables figures sections cf"},
			"api tables figures sections cf"},
		{"a later list adds a name first", []string{"a b", "s a b"}, "s a b"},
		{"a later list adds a name last", []string{"a b", "a b w"}, "a b w"},
		{"an earlier list has a name a later one lacks", []string{"a b c", "a c"}, "a b c"},
		{"two lists add a name after the same name: the later addition goes first",
			[]string{"a z", "a x z", "a y z"}, "a y x z"},
		{"a later list settles what an addition left open",
			[]string{"x a d", "x c d", "x a c"}, "x a c d"},
		{"lists with no name in common", []string{"a b", "c d"}, "c d a b"},
		{"contradiction", []string{"a b", "b a"}, ""},
		{"contradiction through a third name", []string{"a b", "b c", "c a"}, ""},
		{"no lists", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lists [][]string
			for _, l := range tc.lists {
				lists = append(lists, strings.Fields(l))
			}
			order, ok := mergeOrder(lists)
			got := strings.Join(order, " ")
			if tc.want == "" && len(tc.lists) > 0 {
				if ok {
					t.Errorf("merged contradictory lists into %q", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("got %q (ok=%v), want %q", got, ok, tc.want)
			}
		})
	}
}
