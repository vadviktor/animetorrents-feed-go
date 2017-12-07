#!/usr/bin/env bash

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -a -o ./animetorrents-feed
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "-s -w" -a -o ./animetorrents-feed.linux-armv7
