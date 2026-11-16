package rke2

import "testing"

func TestParseVersion(t *testing.T) {
	v, err := ParseVersion("v1.37.2+rke2r1")
	if err != nil {
		t.Fatalf("ParseVersion() error = %v", err)
	}
	if want := (Version{1, 37, 2, 1}); v != want {
		t.Errorf("ParseVersion() = %+v, want %+v", v, want)
	}
	if got := v.String(); got != "v1.37.2+rke2r1" {
		t.Errorf("String() = %q, want round trip", got)
	}

	for _, in := range []string{"", "1.37.2", "v1.37.2", "v1.37.2+k3s1", "latest", "v1.37.2-rc1+rke2r1"} {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseVersion(in); err == nil {
				t.Errorf("ParseVersion(%q) = nil error, want error", in)
			}
		})
	}
}

func TestVersionCompare(t *testing.T) {
	ordered := []string{"v1.37.2+rke2r1", "v1.37.2+rke2r2", "v1.37.10+rke2r1", "v1.38.0+rke2r1"}
	for i, a := range ordered {
		va, err := ParseVersion(a)
		if err != nil {
			t.Fatal(err)
		}
		for j, b := range ordered {
			vb, err := ParseVersion(b)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := va.Compare(vb); got != want {
				t.Errorf("%s.Compare(%s) = %d, want %d", a, b, got, want)
			}
		}
	}
}
