package merge

import "slices"

// mergeOrder merges lists of distinct names, such as the parts' member orders, into one order
// that keeps the relative order of every list.
//
// A part's member order is a subsequence of the order the service writes for the whole document:
// a part leaves out what it does not have, an empty array for instance. So the merged order must
// have every list as a subsequence. Where the lists leave the order open, the lists are taken in
// turn, and a name first seen in list k goes immediately after the name before it in list k, or
// first if it is list k's first. That order is kept as it is unless a later list orders two names
// the other way round; then the names are sorted topologically, by every list's order, taking
// the earliest name in that order whenever there is a choice.
//
// ok is false when the lists contradict each other and no order keeps them all.
func mergeOrder(lists [][]string) (order []string, ok bool) {
	var pref []string
	for _, list := range lists {
		for i, name := range list {
			if slices.Contains(pref, name) {
				continue
			}
			at := 0
			if i > 0 {
				// Already placed: it was seen before, or inserted just now.
				at = slices.Index(pref, list[i-1]) + 1
			}
			pref = slices.Insert(pref, at, name)
		}
	}

	// before[j] lists the names, by index into pref, that some list puts right before pref[j].
	// When pref keeps every list's order, the earliest unplaced name is always free to go next,
	// and the sort returns pref unchanged.
	before := make([][]int, len(pref))
	for _, list := range lists {
		for i := 1; i < len(list); i++ {
			j := slices.Index(pref, list[i])
			before[j] = append(before[j], slices.Index(pref, list[i-1]))
		}
	}
	placed := make([]bool, len(pref))
	unplaced := func(i int) bool { return !placed[i] }
	order = make([]string, 0, len(pref))
	for len(order) < len(pref) {
		next := -1
		for j := range pref {
			if !placed[j] && !slices.ContainsFunc(before[j], unplaced) {
				next = j
				break
			}
		}
		if next < 0 {
			return nil, false // every name left waits for another: the lists disagree
		}
		placed[next] = true
		order = append(order, pref[next])
	}
	return order, true
}
