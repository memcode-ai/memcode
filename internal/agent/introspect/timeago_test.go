package introspect

import (
	"testing"
	"time"
)

func TestTimeAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		d    time.Duration
		want string
	}{
		{20 * time.Second, "just now"},
		{20 * time.Minute, "20m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, c := range cases {
		if got := timeAgo(now.Add(-c.d)); got != c.want {
			t.Errorf("timeAgo(-%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
