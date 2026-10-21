package report

import (
	"fmt"
	"strings"
	"time"
)

// Dur formats a duration for logs and notifications: 1.8s, 44s, 4m30s, 1h34m.
func Dur(d time.Duration) string {
	if d < time.Minute {
		if r := d.Round(time.Second); r < 10*time.Second {
			return fmt.Sprintf("%.1fs", d.Round(100*time.Millisecond).Seconds())
		} else if r < time.Minute {
			return r.String()
		}
	}
	if d < time.Hour {
		return d.Round(time.Second).String()
	}
	s := d.Round(time.Minute).String() // 1h34m0s
	return strings.TrimSuffix(s, "0s")
}

// Bytes formats a size in binary units, one decimal below 10 units: 4.4 GiB, 32 GiB.
func Bytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i > 0 && v < 10 {
		return fmt.Sprintf("%.1f %s", v, units[i])
	}
	return fmt.Sprintf("%.0f %s", v, units[i])
}

// Age formats how old a backup is: 45m, 20h, 3d 4h.
func Age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd %dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}
