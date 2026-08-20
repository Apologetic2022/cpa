#!/bin/sh
# Pick up the renewed certificate without dropping in-flight streaming turns.
systemctl reload nginx
