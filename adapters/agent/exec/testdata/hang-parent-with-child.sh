#!/bin/sh
# Hangs forever, AND spawns a child that hangs forever in its own right.
# Each writes ticks to its own file, so a test can tell WHO is still alive.
#
# A kill that stops only the parent leaves the child ticking — this is the
# fixture that proves the process GROUP died, not just the direct process.
#
# argv: 1 = parent tick file, 2 = child tick file.
while true; do
  echo parent-tick >> "$1"
  sleep 0.05
done &
child=$!
while true; do
  echo child-tick >> "$2"
  sleep 0.05
done
# The child's pid is unreachable here: the parent never exits, so the
# wait below never runs — it exists only to keep sh from complaining.
wait $child
