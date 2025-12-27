// paniccheck

package main

import (
	"gitlab.com/haproxy-haptic/scriggo/test/compare/testpkg"
)

func main() {
	testpkg.Fatal("some error occurred")
}
