// skip : needs some synchronization mechanism. https://gitlab.com/haproxy-haptic/scriggo/issues/420

// run

package main

import (
	"fmt"
	"time"
)

func f() {
	fmt.Print("package function")
}

func main() {
	go f()
	time.Sleep(1 * time.Millisecond)
}
