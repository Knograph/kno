#!/bin/sh
# Emits far more than any test cap: pure sh, no external dependencies.
i=0
while [ $i -lt 2000 ]; do
  echo "line $i of padded output xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
  i=$((i + 1))
done
