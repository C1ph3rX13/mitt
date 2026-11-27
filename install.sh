#!/bin/sh
set -eu

BIN="${1:-./mitt}"
DESTDIR="${DESTDIR:-}"
PREFIX="${PREFIX:-/usr/local}"
BINDIR="${DESTDIR}${PREFIX}/bin"

if [ ! -f "$BIN" ]; then
	echo "install.sh: binary not found: $BIN (run make build first)" >&2
	exit 1
fi

install -d -m 0755 "$BINDIR"
install -m 0755 "$BIN" "$BINDIR/mitt"
