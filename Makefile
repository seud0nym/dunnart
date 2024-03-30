# SPDX-FileCopyrightText: 2019 Kent Gibson <warthog618@gmail.com>
#
# SPDX-License-Identifier: MIT

GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean

VERSION ?= $(shell git describe --tags --always --dirty 2> /dev/null )
LDFLAGS=-ldflags "-s -w -X=main.version=$(VERSION)"

all: dunnart

dunnart : % : %.go
	$(GOBUILD) $(LDFLAGS)

clean:
	$(GOCLEAN) ./...
	rm -rf build/

pi0:
	GOARCH=arm GOARM=6 $(GOBUILD) $(LDFLAGS) -o build/armv6/

pi2:
	GOARCH=arm GOARM=7 $(GOBUILD) $(LDFLAGS) -o build/armv7/

pi4:
	GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o build/arm64/

mips:
	GOARCH=mips GOMIPS=softfloat $(GOBUILD) $(LDFLAGS) -o build/mips/

mipsle:
	GOARCH=mipsle GOMIPS=softfloat $(GOBUILD) $(LDFLAGS) -o build/mipsle/

amd64:
	GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o build/amd64/

arm5:
	GOARCH=arm GOARM=5 $(GOBUILD) $(LDFLAGS) -o build/armv5/
