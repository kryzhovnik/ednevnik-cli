# Cookie jar provenance

This package is a focused fork of `go.nhat.io/cookiejar` v0.4.0, tag commit
`7eb94a2037228a28019312c45a0f9c6332cb688c`. The upstream package is BSD-3-Clause;
the license is retained in this directory.

The fork contains the upstream matching engine and public-suffix behavior. It
adds explicit full-state `Snapshot` and validated `Restore` methods. This keeps
filesystem writes in eDnevnik's atomic account-state layer and preserves the
`Quoted` field omitted by the upstream persistent adapter. Upstream matching
tests remain represented by the application's scope, path, secure,
public-suffix, deletion, expiry, replacement, and ordering regression matrix.

When updating Go or this package, compare against the current upstream jar and
rerun `go test -race ./internal/client/...`.
