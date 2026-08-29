#!/bin/sh
# Prints its arguments, one per line, as received — quote characters and all.
i=1
for a in "$@"; do
  echo "arg$i=$a"
  i=$((i + 1))
done
