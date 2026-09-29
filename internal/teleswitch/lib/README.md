`make teleswitch` builds `teleswitch-<GOARCH>.so` here, and `make build`
embeds it into tele. The built libraries are not committed; this file keeps
the directory, so that `go build` and `go test` work without make.
