// run

package main

import (
	"fmt"

	"github.com/haproxy-haptic/scriggo/test/compare/testpkg"
)

func main() {
	a := 2
	a = testpkg.G11(9)
	fmt.Print(a)
	return
}
