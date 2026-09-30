// Package humanize formats sizes, speeds and durations for the UI.
package humanize

import (
	"fmt"
	"time"
)

// Bytes formats a size with binary units: 512 B, 1.5 KB, 64.0 MB, 1.21 GB.
func Bytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n) / unit
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if v >= 100 || i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	if i >= 2 && v < 10 {
		return fmt.Sprintf("%.2f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// Speed formats bytes per second.
func Speed(bps int64) string {
	if bps <= 0 {
		return "—"
	}
	return Bytes(bps) + "/s"
}

// Duration formats an ETA as m:ss or h:mm:ss.
func Duration(d time.Duration) string {
	if d < 0 {
		return "--:--"
	}
	s := int64(d.Round(time.Second) / time.Second)
	h, m, sec := s/3600, (s/60)%60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}
