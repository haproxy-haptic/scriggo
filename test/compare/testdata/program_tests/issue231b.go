// skip : needs some synchronization mechanism. https://gitlab.com/haproxy-haptic/scriggo/issues/420

// run

package main

import (
	"fmt"
	"time"
)

func f(i int) {
	fmt.Print(i)
}

func main() {
	go f(42)
	time.Sleep(3 * time.Millisecond)
}
