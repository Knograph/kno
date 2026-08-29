#!/bin/sh
# Prints the environment, sorted, so a test can assert exactly what the child
# received — nothing more than the allowlist plus the grants.
env | sort
