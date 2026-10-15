// run

package main

import "github.com/haproxy-haptic/scriggo/test/compare/testpkg"

func main() {
	a := testpkg.A
	testpkg.PrintInt(a)
}
