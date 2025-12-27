// errorcheck

package main

import . "gitlab.com/haproxy-haptic/scriggo/test/compare/testpkg"

func main() {
	t := T(0)
	T.Method(&t) // ERROR `invalid method expression T.Method (needs pointer receiver: (*T).Method)`
	_ = t
}
