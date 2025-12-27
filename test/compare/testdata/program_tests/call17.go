// run

package main

import "gitlab.com/haproxy-haptic/scriggo/test/compare/testpkg"

func main() {
	f := testpkg.ReturnFunction()
	println(f(5))
}
