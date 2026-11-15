package rke2

import (
	"fmt"
	"regexp"
	"strconv"
)

var versionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)\+rke2r(\d+)$`)

// Version is an RKE2 release tag such as v1.37.2+rke2r1.
type Version struct{ Major, Minor, Patch, Release int }

// ParseVersion accepts only release tags of the form vMAJOR.MINOR.PATCH+rke2rN.
func ParseVersion(s string) (Version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("invalid RKE2 version %q: want the form v1.2.3+rke2r1", s)
	}
	var n [4]int
	for i := range n {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("invalid RKE2 version %q: %w", s, err)
		}
		n[i] = v
	}
	return Version{n[0], n[1], n[2], n[3]}, nil
}

// Compare returns -1, 0 or 1 as v is older than, equal to or newer than o.
func (v Version) Compare(o Version) int {
	a := [4]int{v.Major, v.Minor, v.Patch, v.Release}
	b := [4]int{o.Major, o.Minor, o.Patch, o.Release}
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// String returns the release tag.
func (v Version) String() string {
	return fmt.Sprintf("v%d.%d.%d+rke2r%d", v.Major, v.Minor, v.Patch, v.Release)
}
