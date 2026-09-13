#!/bin/bash
pkill -f "postra" 2>/dev/null || true
sleep 1
exec ./bin/postra serve
