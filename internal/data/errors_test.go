package data

import (
	"errors"
	"testing"
)

func TestParseDrainStrict(t *testing.T) {
	cases := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"off", false, false},
		{"OFF", false, false},
		{"false", false, false},
		{"FALSE", false, false},
		{"on", true, false},
		{"ON", true, false},
		{"true", true, false},
		{"TRUE", true, false},
		{"yes", false, true},
		{"1", false, true},
		{"enabled", false, true},
	}
	for _, c := range cases {
		got, err := ParseDrainStrict(c.raw)
		if c.wantErr {
			if err == nil {
				t.Fatalf("ParseDrainStrict(%q): want err, got nil", c.raw)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseDrainStrict(%q): unexpected err %v", c.raw, err)
		}
		if got != c.want {
			t.Fatalf("ParseDrainStrict(%q): want %v, got %v", c.raw, c.want, got)
		}
	}
}

func TestDrainRefusedErrorIs(t *testing.T) {
	err := NewDrainRefusedError("default")
	if !errors.Is(err, ErrDrainRefused) {
		t.Fatalf("errors.Is(NewDrainRefusedError, ErrDrainRefused): false")
	}
	var dre *DrainRefusedError
	if !errors.As(err, &dre) {
		t.Fatalf("errors.As(NewDrainRefusedError, *DrainRefusedError): false")
	}
	if dre.Cluster != "default" {
		t.Fatalf("DrainRefusedError.Cluster: want %q, got %q", "default", dre.Cluster)
	}
}
