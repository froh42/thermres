#!/bin/sh
# install.sh — Build thermres and apply setuid-root for RAPL access.
set -euo

echo "==> Building thermres (CGO_ENABLED=0)..."
CGO_ENABLED=0 go build -o thermres .

echo "==> Setting setuid-root..."
sudo chown root:root thermres
sudo chmod u+s thermres

echo "==> Done.  Run ./thermres --verbose to test."
