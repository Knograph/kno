#!/bin/sh
# Hangs while a sentinel file exists; answers immediately once it is gone.
# A test that cannot wait out a full timeout uses this: arm the sentinel,
# cancel the run, and the SAME invocation answers after being killed with
# TERM — proving TERM alone ends the group instead of being ignored.
#
# argv: 1 = sentinel path.
if [ -f "$1" ]; then
  while true; do
    sleep 60
  done
fi
cat
