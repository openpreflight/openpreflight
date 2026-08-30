package layouts

import "strconv"

func itoa(n int) string {
	return strconv.Itoa(n)
}

func dockerLabel(ok bool) string {
	if ok {
		return "reachable"
	}
	return "not reachable"
}
