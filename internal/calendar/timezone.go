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

// cldrAliases maps current IANA names to the older ones CLDR still lists.
// A client sending Asia/Kolkata must not be refused over a rename.
var cldrAliases = map[string]string{
	"Africa/Asmara":                  "Africa/Asmera",
	"America/Argentina/Buenos_Aires": "America/Buenos_Aires",
	"America/Argentina/Catamarca":    "America/Catamarca",
	"America/Argentina/Cordoba":      "America/Cordoba",
	"America/Argentina/Jujuy":        "America/Jujuy",
	"America/Argentina/Mendoza":      "America/Mendoza",
	"America/Indiana/Indianapolis":   "America/Indianapolis",
	"America/Kentucky/Louisville":    "America/Louisville",
	"America/Nuuk":                   "America/Godthab",
	"Asia/Ho_Chi_Minh":               "Asia/Saigon",
	"Asia/Kathmandu":                 "Asia/Katmandu",
	"Asia/Kolkata":                   "Asia/Calcutta",
	"Asia/Yangon":                    "Asia/Rangoon",
	"Atlantic/Faroe":                 "Atlantic/Faeroe",
	"Europe/Kyiv":                    "Europe/Kiev",
	"Pacific/Chuuk":                  "Pacific/Truk",
	"Pacific/Kanton":                 "Pacific/Enderbury",
	"Pacific/Pohnpei":                "Pacific/Ponape",
}

// windowsZone turns an IANA zone into the Windows name Graph takes on writes.
// A zone CLDR does not list gives false: never a guess at a similar offset,
// because two zones can share an offset today and differ at the next DST.
func windowsZone(iana string) (string, bool) {
	if iana == "UTC" {
		return "UTC", true
	}
	if old, ok := cldrAliases[iana]; ok {
		iana = old
	}
	w, ok := ianaWindowsZones[iana]
	return w, ok
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
