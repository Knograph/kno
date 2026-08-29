#!/bin/sh
# Fails with a useful message on stderr and a nonzero exit.
echo "calculation failed: division by zero at line 3" >&2
exit 2
