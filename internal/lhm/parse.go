package lhm

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseValue splits an LHM value string such as "41,0 °C" or "4.593,0 MHz" into its
// number and unit. The last separator ('.' or ',') is the decimal one; any earlier
// separator is a thousands separator and is dropped. "-", empty and non-numeric
// strings return an error.
func ParseValue(s string) (float64, string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, " ", " "))

	end := 0
	for end < len(s) && strings.IndexByte("0123456789.,+-", s[end]) >= 0 {
		end++
	}
	num, unit := s[:end], strings.TrimSpace(s[end:])
	if num == "" {
		return 0, "", errors.New("no numeric part")
	}

	if i := strings.LastIndexAny(num, ".,"); i >= 0 {
		num = strings.NewReplacer(".", "", ",", "").Replace(num[:i]) + "." + num[i+1:]
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parsing %q: %w", s, err)
	}
	return v, unit, nil
}
