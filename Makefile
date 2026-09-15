# `make help` is the canonical source of truth for every target this repo
# supports. Run it before adding anything new. Lint, build, test, deadcode,
# release, baseline, and service-install all live in the central go-makefile
# pipeline fetched at parse time. Do NOT add project-local lint, deadcode,
# audit, fmt, vet, or staticcheck targets here. They duplicate the central
# pipeline and let agents bypass strict rules.

# Identity
BINARY     := opnsensectl
CMD        := ./cmd/opnsensectl
GKLOG_VPKG := goodkind.io/gklog/version

# Pipeline modules.
GO_MK_MODULES := go-build.mk go-release.mk

# One pure-Go binary for each machine the tooling runs on: the Proxmox host
# (the bridge, the drainer, and the upgrade orchestration) and the OPNsense
# guest (the daemon). No release entry sets cgo=1, so every artifact builds
# with cgo disabled.
#
# RELEASE_PLATFORMS is a default, not an override. Each release job passes its
# own single platform in the environment, and this must yield to it.
RELEASE_PLATFORMS ?= linux/amd64 freebsd/amd64
RELEASE_BINS      := $(BINARY):$(CMD)

# Every lint gate runs once per shipped platform, so the freebsd serial path
# and the linux host paths are both checked.
GO_MK_PLATFORMS := linux/amd64 freebsd/amd64

include bootstrap.mk

.DEFAULT_GOAL := check

# ---------------------------------------------------------------------------
# Protobuf
# ---------------------------------------------------------------------------

# Requires buf, protoc-gen-go, protoc-gen-go-grpc on PATH. The generated code is
# committed; the proto package stays mwan.v1 so the wire matches daemons built
# before the move.
BUF   ?= buf
GOBIN ?= $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)

.PHONY: proto
proto:
	$(BUF) generate
