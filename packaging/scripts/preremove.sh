#!/bin/sh
set -e

if command -v systemctl >/dev/null 2>&1; then
    systemctl stop airspace.service 2>/dev/null || true
    systemctl disable airspace.service 2>/dev/null || true
fi
