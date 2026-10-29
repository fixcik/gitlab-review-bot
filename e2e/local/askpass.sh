#!/bin/sh
# GIT_ASKPASS helper for the e2e/local scripts: the user name is in the
# remote URL; the password is the token from RB_GIT_TOKEN (never in argv).
case "$1" in
  Username*) printf '%s\n' "${RB_GIT_USER:-root}" ;;
  *) printf '%s\n' "$RB_GIT_TOKEN" ;;
esac
