module gitlab.com/haproxy-haptic/scriggo/test

go 1.27.0

replace gitlab.com/haproxy-haptic/scriggo => ../

require (
	github.com/google/go-cmp v0.7.0
	github.com/rogpeppe/go-internal v1.16.0
	gitlab.com/haproxy-haptic/scriggo v0.0.0
	golang.org/x/tools v0.50.0
)

require golang.org/x/sync v0.23.0 // indirect
