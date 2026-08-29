#!/bin/sh
# Answers "grant-visible" when the environment grants KNO_CLI_GRANT,
# "grant-absent" otherwise. A CLI test runs this against cases whose expected
# answer is "grant-visible", so the run's score proves the grant reached the
# child through the whole pipeline.
#
# printf, not echo: the exact-match goal compares strings exactly, and the
# fixture's answer must carry no trailing newline.
if [ -n "$KNO_CLI_GRANT" ]; then
  printf grant-visible
else
  printf grant-absent
fi
