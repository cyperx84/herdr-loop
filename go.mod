module github.com/cyperx84/herdr-loop

go 1.26.5

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/cyperx84/herdr-api v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	golang.org/x/sys v0.10.0 // indirect
)

// herdr-api is not published yet; this replace is removed once it is.
replace github.com/cyperx84/herdr-api => ../herdr-api
