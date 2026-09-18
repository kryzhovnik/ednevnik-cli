#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
	echo "usage: $0 VERSION OUTPUT_DIRECTORY" >&2
	exit 2
fi

version=$1
output_dir=$2
case "$version" in
	*[!0-9A-Za-z.-]*|'')
		echo "version must contain only letters, digits, dots, and hyphens" >&2
		exit 2
		;;
esac

repo_root=$(git rev-parse --show-toplevel)
if ! git -C "$repo_root" diff-index --quiet HEAD --; then
	echo "candidate tracked source must match HEAD" >&2
	exit 1
fi
commit=$(git -C "$repo_root" rev-parse HEAD)
source_date_epoch=$(git -C "$repo_root" show -s --format=%ct "$commit")
temporary=$(mktemp -d "${TMPDIR:-/tmp}/ednevnik-candidate.XXXXXX")
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
source_dir=$temporary/source
mkdir -p "$source_dir" "$output_dir"
output_dir=$(cd "$output_dir" && pwd)
git -C "$repo_root" archive "$commit" | tar -x -C "$source_dir"

artifact_prefix=ednevnik-$version
git -C "$repo_root" archive --format=tar.gz --prefix="$artifact_prefix/" -o "$output_dir/$artifact_prefix-source.tar.gz" "$commit"

for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
	goos=${target%/*}
	goarch=${target#*/}
	artifact=$output_dir/$artifact_prefix-$goos-$goarch
	(
		cd "$source_dir"
		CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch SOURCE_DATE_EPOCH=$source_date_epoch \
			go build -mod=readonly -trimpath -ldflags="-s -w -X main.version=$version -buildid=" -o "$artifact" ./cmd/ednevnik
	)
done

{
	echo "version=$version"
	echo "source_commit=$commit"
	echo "source_date_epoch=$source_date_epoch"
	echo "go_version=$(go version)"
	echo "go_env_GOOS=$(go env GOOS)"
	echo "go_env_GOARCH=$(go env GOARCH)"
	echo "module=$(cd "$source_dir" && go list -m)"
	echo "dependencies_begin"
	(cd "$source_dir" && go list -m all)
	echo "dependencies_end"
} > "$output_dir/BUILD-METADATA.txt"
(
	cd "$output_dir"
	shasum -a 256 "$artifact_prefix"-* BUILD-METADATA.txt > SHA256SUMS
)

echo "candidate artifacts: $output_dir"
