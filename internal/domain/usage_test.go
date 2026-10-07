package domain

import "testing"

func TestWindowLabel(t *testing.T) {
	for _, c := range []struct {
		minutes int
		want    string
	}{
		{300, "5h"},
		{10080, "7d"},
		{60, "1h"},
		{1440, "1d"},
		{90, "90m"},
		{43200, "30d"},
		{0, "?"},
	} {
		if got := WindowLabel(c.minutes); got != c.want {
			t.Errorf("WindowLabel(%d) = %q, want %q", c.minutes, got, c.want)
		}
	}
}
