package homeweb

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// plural picks the Russian form for n: one (1, 21), few (2-4, 22-24), many.
func plural(n int, one, few, many string) string {
	if n < 0 {
		n = -n
	}
	a, b := n%10, n%100
	switch {
	case a == 1 && b != 11:
		return one
	case a >= 2 && a <= 4 && (b < 10 || b >= 20):
		return few
	}
	return many
}

// pl is "n <form>".
func pl(n int, one, few, many string) string {
	return strconv.Itoa(n) + " " + plural(n, one, few, many)
}

// usd formats a cost: $0.00 for nothing, <$0.01 for a trace, else two decimals.
func usd(v float64) string {
	if v > 0 && v < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

// short formats a token count: 950, 12 тыс., 3,4 млн.
func short(n int64) string {
	switch {
	case n >= 10_000_000:
		return strconv.FormatInt((n+500_000)/1_000_000, 10) + " млн"
	case n >= 1_000_000:
		s := strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64)
		return strings.Replace(strings.TrimSuffix(s, ".0"), ".", ",", 1) + " млн"
	case n >= 1000:
		return strconv.FormatInt((n+500)/1000, 10) + " тыс."
	}
	return strconv.FormatInt(n, 10)
}

// ago says how long ago t was, relative to now.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " мин назад"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " ч назад"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " дн. назад"
}

// pct is n/of as a rounded percentage ("87%").
func pct(n, of int) string { return strconv.Itoa(int(float64(n)/float64(of)*100+0.5)) + "%" }
