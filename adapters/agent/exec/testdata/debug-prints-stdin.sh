#!/bin/sh
# Debug-prints its whole stdin to stderr, then fails — the shape the stderr
# cap exists for. A script like this must not be able to turn Case content
# into stored error text.
cat >&2
echo "something went wrong" >&2
exit 1
