// run

package main

import (
	"fmt"

	"github.com/haproxy-haptic/scriggo/test/compare/testpkg"
)

func main() {
	a := 5
	b, c := testpkg.Pair()
	fmt.Println(a, b, c)
	return
}
