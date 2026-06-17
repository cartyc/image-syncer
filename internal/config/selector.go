package config

import (
	"fmt"
	"regexp"
	"sort"
)

// Select resolves the tags to mirror given the full set of tags that exist in
// the source repository. An explicit List is intersected with what exists
// (missing tags are reported); otherwise All tags pass through Include/Exclude.
func (t TagSelector) Select(available []string) (selected []string, missing []string, err error) {
	have := make(map[string]bool, len(available))
	for _, a := range available {
		have[a] = true
	}

	if len(t.List) > 0 {
		for _, want := range t.List {
			if have[want] {
				selected = append(selected, want)
			} else {
				missing = append(missing, want)
			}
		}
		return selected, missing, nil
	}

	var include, exclude *regexp.Regexp
	if t.Include != "" {
		if include, err = regexp.Compile(t.Include); err != nil {
			return nil, nil, fmt.Errorf("invalid include regex %q: %w", t.Include, err)
		}
	}
	if t.Exclude != "" {
		if exclude, err = regexp.Compile(t.Exclude); err != nil {
			return nil, nil, fmt.Errorf("invalid exclude regex %q: %w", t.Exclude, err)
		}
	}
	for _, tag := range available {
		if include != nil && !include.MatchString(tag) {
			continue
		}
		if exclude != nil && exclude.MatchString(tag) {
			continue
		}
		selected = append(selected, tag)
	}
	sort.Strings(selected)
	return selected, nil, nil
}
