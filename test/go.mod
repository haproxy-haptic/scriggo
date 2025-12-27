module gitlab.com/haproxy-haptic/scriggo/test

go 1.24.0

replace gitlab.com/haproxy-haptic/scriggo => ../

require (
	github.com/google/go-cmp v0.6.0
	github.com/rogpeppe/go-internal v1.13.1
	gitlab.com/haproxy-haptic/scriggo v0.0.0
	golang.org/x/tools v0.40.0
)

require golang.org/x/sync v0.19.0 // indirect
