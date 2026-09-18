# Secure build baseline

The supported source-build baseline is Go 1.26.8 or a later patched Go 1.26
release. The `go` directive pins the minimum patch release, and `go.sum` pins
the resolved module content. Keep the default public Go module proxy and
checksum database enabled when resolving dependencies. Do not build a release
with `GONOSUMDB`, `GOSUMDB=off`, or a modified dependency graph.

Go 1.26.8 was the current Go 1.26 patch release when this baseline was checked
on 2026-09-18. The Go project lists the release and supported archives on its
[release history](https://go.dev/doc/devel/release) and
[download page](https://go.dev/dl/). Dependency versions were resolved through
the official Go module proxy. `go list -m -u all` reported no newer module
versions on the verification date.

## Reproduce the build

Run these commands from a fresh checkout. `GOTOOLCHAIN=auto` lets the Go
command fetch the exact patch toolchain declared by `go.mod` when the host has
an older Go toolchain.

```sh
GOTOOLCHAIN=auto go version
GOTOOLCHAIN=auto go mod download
GOTOOLCHAIN=auto go mod verify
GOTOOLCHAIN=auto go test ./...
GOTOOLCHAIN=auto go vet ./...
GOTOOLCHAIN=auto go test -race ./...
GOTOOLCHAIN=auto go build -trimpath -o ednevnik ./cmd/ednevnik
```

The race detector command is a native-platform check. It requires a supported
host toolchain and C compiler. It is not a substitute for process-level or
target-platform tests.

Install the scanner at the version used for this baseline and scan all root
packages against the official Go vulnerability database:

```sh
go install golang.org/x/vuln/cmd/govulncheck@v1.8.0
"$(go env GOPATH)/bin/govulncheck" -show verbose ./...
```

`govulncheck` uses the official database at
[vuln.go.dev](https://vuln.go.dev/). A release build must have no unreviewed
reachable findings. A package- or module-level finding without a reachable
symbol still requires a recorded assessment; do not suppress it to make the
scan pass.

## Cross-compilation check

The CLI is pure Go at this baseline. Compile the intended macOS and Linux CPU
targets with CGO disabled:

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o ednevnik-darwin-arm64 ./cmd/ednevnik
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -o ednevnik-darwin-amd64 ./cmd/ednevnik
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o ednevnik-linux-arm64 ./cmd/ednevnik
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ednevnik-linux-amd64 ./cmd/ednevnik
```

Successful cross-compilation proves that these target tuples compile. It does
not prove runtime support on those platforms. Runtime evidence belongs in the
release-candidate verification.

## Baseline evidence

The initial scan on 2026-09-18 used Go 1.26.2, `govulncheck` 1.8.0, and the
official database updated at 2026-09-16 18:00:43 UTC. It found 14 unique
reachable vulnerabilities. The standard-library findings were GO-2026-6218,
GO-2026-6090, GO-2026-5972, GO-2026-5856, GO-2026-5039, GO-2026-5037,
GO-2026-5026, GO-2026-4971, and GO-2026-4918. The `golang.org/x/net` 0.47.0
findings were GO-2026-5030, GO-2026-5029, GO-2026-5028, GO-2026-5027,
GO-2026-5026, GO-2026-5025, and GO-2026-4918; two IDs affected both the module
and standard library. The scanner also reported five vulnerabilities in
imported packages and eight in required modules for which no vulnerable symbol
was reachable from this program.

The final scan used Go 1.26.8 and these resolved direct dependencies:

- `github.com/PuerkitoBio/goquery` 1.13.0
- `golang.org/x/term` 0.46.0

The resolved graph includes `golang.org/x/net` 0.59.0. The verbose final scan
covered all seven project packages, six modules, and the Go 1.26.8 standard
library and reported `No vulnerabilities found.` There are therefore no
residual findings to assess or carry as a release gate for this tree.

On a Darwin 25.3.0 arm64 host, the existing tests, `go vet`, and the race suite
passed with Go 1.26.8. Cross-builds produced Mach-O arm64 and x86_64 binaries
and statically linked ELF aarch64 and x86-64 binaries. Verification did not use
portal calls, credentials, saved sessions, or private records.
