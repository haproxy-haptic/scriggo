// run

package main

import "fmt"

type values []any

func missing() any { return nil }

func main() {
	var empty any
	var pointer *int
	for _, capacity := range []int{0, 8} {
		a := make([]any, 0, capacity)
		a = append(a, empty, missing(), nil, pointer, "value")
		fmt.Println(len(a), a[0] == nil, a[1] == nil, a[2] == nil, a[3] == nil, a[4])
		b := make(values, 0, capacity)
		b = append(b, empty, missing(), nil, pointer, "value")
		fmt.Println(len(b), b[0] == nil, b[1] == nil, b[2] == nil, b[3] == nil, b[4])
		fmt.Println(len(append(a, nil...)), len(append(b, nil...)))
	}
	var integers []int
	fmt.Println(append(integers, nil...) == nil)
	fmt.Println(append([]*int{}, nil)[0] == nil)
}
