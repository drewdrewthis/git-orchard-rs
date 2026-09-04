#!/usr/bin/env bats
# #766: ExecStart must name orchard-daemon (owns `daemon start`), not the
# `orchard` dispatcher which routes to a nonexistent daemon-start.sh.

setup() {
  UNIT="$BATS_TEST_DIRNAME/orchard.service"
}

@test "ExecStart names orchard-daemon, not the orchard dispatcher" {
  line="$(grep '^ExecStart=' "$UNIT")"
  [ "$line" = "ExecStart=/usr/local/bin/orchard-daemon daemon start" ]
}

@test "no tilde in a systemd directive (systemd uses %h, not ~; comments may still show ~ in shell install steps)" {
  directives="$(grep -vE '^\s*#' "$UNIT")"
  [[ "$directives" != *'~'* ]]
}

@test "no reference to the orchard dispatcher binary" {
  run grep -F '/usr/local/bin/orchard ' "$UNIT"
  [ "$status" -ne 0 ]
}
