// run

package main

import (
	"fmt"

	"gitlab.com/haproxy-haptic/scriggo/test/compare/testpkg"
)

var Center = "msg"

func main() {
	nativeC := testpkg.Center
	fmt.Println(nativeC)
	c := Center
	fmt.Println(c)
}
