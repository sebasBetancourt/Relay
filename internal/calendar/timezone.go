package calendar

import (
	"strings"
	"time"
)

//go:generate go run gen_windowszones.go

// Zone turns a Microsoft Graph time zone name into an IANA name.
// Unknown names give "" (never a guess). Delta sync uses the same map
// as the live events read.
func Zone(name string) string { return ianaZone(name) }

// ianaZone turns a Microsoft Graph time zone name into an IANA name.
// Graph returns Windows names ("SA Pacific Standard Time"), sometimes IANA
// names, or its own markers. Unknown names give "" (never a guess).
func ianaZone(name string) string {
	switch name {
	case "", "tzone://Microsoft/Custom":
		return ""
	case "UTC", "tzone://Microsoft/Utc":
		return "UTC"
	}
	if z, ok := windowsZones[name]; ok {
		if z == "Etc/UTC" {
			return "UTC"
		}
		return z
	}
	if strings.Contains(name, "/") {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	return ""
}

// location resolves a Graph time zone for parsing wall-clock times.
// The binary embeds time/tzdata (cmd/api), so this works in distroless.
func location(name string) (*time.Location, bool) {
	z := ianaZone(name)
	if z == "" {
		return nil, false
	}
	loc, err := time.LoadLocation(z)
	return loc, err == nil
}
