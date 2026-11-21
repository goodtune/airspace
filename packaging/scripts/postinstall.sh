#!/bin/sh
set -e

# Make systemd see the new or updated unit. The unit ships disabled: Airspace
# needs OpenSky credentials in /etc/airspace/airspace.env before it can
# start. Once they are there:
#   systemctl enable --now airspace.service
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
fi
