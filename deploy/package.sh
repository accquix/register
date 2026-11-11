#!/bin/sh
# Packs the deployment files for a release (OQ-33): deploy/compose, deploy/paas and deploy/demo,
# with the register image pinned to the released version and the `build:` sections removed, so
# that they run without the source code:
#
#   deploy/package.sh 1.0.0-rc.1 dist-extra    ->  dist-extra/accquix-register-deploy_1.0.0-rc.1.tar.gz
#
# The release workflow attaches the archive to the GitHub release.
set -eu

version=${1:?usage: deploy/package.sh VERSION OUTDIR}
out=${2:?usage: deploy/package.sh VERSION OUTDIR}
version=${version#v}
image="ghcr.io/accquix/register:${version}"
root=$(cd "$(dirname "$0")/.." && pwd)
name="accquix-register-deploy_${version}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/$name" "$out"
for dir in compose paas demo; do
	mkdir -p "$work/$name/$dir"
	# the files a deployment needs; never the local secrets/ directory
	(cd "$root/deploy/$dir" && find . -type f ! -path './secrets/*' ! -name '.DS_Store' -print) | while read -r f; do
		mkdir -p "$work/$name/$dir/$(dirname "$f")"
		cp -p "$root/deploy/$dir/$f" "$work/$name/$dir/$f"
	done
	# pin the image and drop the build section (it needs the source tree)
	awk -v image="$image" '
		skip && match($0, /^ */) && RLENGTH <= indent && $0 !~ /^ *$/ { skip = 0 }
		skip { next }
		/^ *image: \$\{REGISTER_IMAGE:-accquix-register:local\}/ {
			sub(/accquix-register:local/, image); print; next
		}
		/^ *build: *$/ { match($0, /^ */); indent = RLENGTH; skip = 1; next }
		{ print }
	' "$root/deploy/$dir/compose.yaml" > "$work/$name/$dir/compose.yaml"
	if grep -q 'build:' "$work/$name/$dir/compose.yaml" || ! grep -q "$image" "$work/$name/$dir/compose.yaml"; then
		echo "package: $dir/compose.yaml was not rewritten as expected" >&2
		exit 1
	fi
done
cp -p "$root/LICENSE" "$root/NOTICE" "$work/$name/"
cat > "$work/$name/README.txt" <<EOF
Accquix Register ${version}: deployment files.

  compose/  the reference deployment: run ./make-secrets.sh, then docker compose up -d
  paas/     the variant for Coolify, Dokploy and similar platforms (environment variables only)
  demo/     fictional data in seconds: docker compose -f demo/compose.yaml up -d --wait serve

Every compose file runs the image ${image}.
Documentation: https://github.com/accquix/register
EOF
tar -C "$work" -czf "$out/$name.tar.gz" "$name"
echo "$out/$name.tar.gz"
