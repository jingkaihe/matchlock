//go:build linux

package lifecycle

import (
	"fmt"
	"strings"
)

func tapNameCandidates(vmID, fromRecord string) []string {
	candidates := make([]string, 0, 3)
	add := func(v string) {
		if v == "" {
			return
		}
		for _, existing := range candidates {
			if existing == v {
				return
			}
		}
		candidates = append(candidates, v)
	}

	add(fromRecord)
	idSuffix := strings.TrimPrefix(vmID, "vm-")
	if len(idSuffix) >= 8 {
		add("fc-" + idSuffix[:8])
	} else if idSuffix != "" {
		add("fc-" + idSuffix)
	}
	if len(vmID) >= 8 {
		add(fmt.Sprintf("fc-%s", vmID[:8]))
	}
	return candidates
}
