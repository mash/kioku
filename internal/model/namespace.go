package model

// ValidSlug restricts namespace components to portable ASCII path segments.
func ValidSlug(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	last := rune(0)
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') || r == '-' && last == '-' {
			return false
		}
		last = r
	}
	return true
}
