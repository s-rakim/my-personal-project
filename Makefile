# Build and test everything.
#
# Five languages, one entry point. `make` builds them all; `make test` runs every
# suite including the cross-language conformance check that keeps the Go and
# Python link budgets from drifting apart.

SHELL := /bin/sh
DOTNET ?= $(shell command -v dotnet 2>/dev/null || echo /usr/lib/dotnet/dotnet)
BIN := bin
CONFIG ?= var/config.json

export DOTNET_CLI_TELEMETRY_OPTOUT := 1
export DOTNET_NOLOGO := 1

.PHONY: all build test clean run seed demo stop \
        scheduler controlplane planning oss noc mobile cache \
        test-scheduler test-controlplane test-planning test-oss test-noc test-mobile test-cache conformance \
        fmt vet help

all: build

help:
	@echo "bswisp -- fixed wireless base station"
	@echo
	@echo "  make build       build every component"
	@echo "  make test        run every test suite"
	@echo "  make seed        create an example network and test credentials"
	@echo "  make run         start the control plane in the foreground"
	@echo "  make demo        start everything and drive it with simulated terminals"
	@echo "  make stop        stop anything started by demo"
	@echo "  make clean       remove build output (leaves var/ alone)"
	@echo
	@echo "  Components: scheduler (C++), controlplane (Go), planning (Python),"
	@echo "              oss (Java), noc (C#), mobile (Go), cache (Go)"

build: scheduler controlplane oss noc mobile cache
	@echo "all components built"

# ---- C++ airtime solver ----------------------------------------------------

scheduler: scheduler/build/bswisp-solver

scheduler/build/bswisp-solver: $(wildcard scheduler/src/*.cpp scheduler/include/bswisp/*.hpp)
	cmake -S scheduler -B scheduler/build -DCMAKE_BUILD_TYPE=Release
	cmake --build scheduler/build -j

test-scheduler: scheduler
	cmake --build scheduler/build --target test_airtime -j
	./scheduler/build/test_airtime

# ---- Go control plane ------------------------------------------------------

controlplane: $(BIN)/basestationd $(BIN)/termsim

$(BIN)/basestationd: $(shell find controlplane -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN)
	cd controlplane && go build -o ../$(BIN)/basestationd ./cmd/basestationd

$(BIN)/termsim: $(shell find controlplane -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN)
	cd controlplane && go build -o ../$(BIN)/termsim ./cmd/termsim

test-controlplane:
	cd controlplane && go vet ./... && go test ./...

# ---- Python planning -------------------------------------------------------

planning:
	@python3 -c "import sys; sys.path.insert(0,'planning'); import bsplan" \
	  && echo "planning toolkit importable"

test-planning:
	cd planning && python3 -m unittest discover -s tests -q

# ---- Java OSS --------------------------------------------------------------

oss: oss/build/bswisp-oss.jar

oss/build/bswisp-oss.jar: $(shell find oss/src -name '*.java' 2>/dev/null)
	./oss/build.sh

test-oss: oss
	@echo "(oss/build.sh runs the tests as part of the build)"

# ---- C# NOC ----------------------------------------------------------------

noc:
	$(DOTNET) build noc/BswispNoc.csproj -v q --nologo

test-noc: noc
	$(DOTNET) run --project noc/BswispNoc.csproj --no-build -- --self-test

# ---- Go multi-link manager (vehicle / remote site) ----

mobile: $(BIN)/mobilelinkd $(BIN)/usagewatch

$(BIN)/mobilelinkd: $(shell find mobile -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN)
	cd mobile && go build -o ../$(BIN)/mobilelinkd ./cmd/mobilelinkd

$(BIN)/usagewatch: $(shell find mobile -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN)
	cd mobile && go build -o ../$(BIN)/usagewatch ./cmd/usagewatch

test-mobile:
	cd mobile && go vet ./... && go test ./...

# ---- Go local content cache ----

cache: $(BIN)/cached

$(BIN)/cached: $(shell find cache -name '*.go' 2>/dev/null)
	@mkdir -p $(BIN)
	cd cache && go build -o ../$(BIN)/cached ./cmd/cached

test-cache:
	cd cache && go vet ./... && go test ./...

# ---- everything ------------------------------------------------------------

# The conformance target is the one that catches cross-language drift: both the
# Go and Python link budgets must reproduce schema/fixtures/linkbudget-cases.json
# exactly. A planner that disagrees with the live scheduler produces confident
# numbers the network will not honour.
conformance:
	cd controlplane && go test ./internal/radio/ -run Conformance -v 2>&1 | tail -5
	cd planning && python3 -m unittest tests.test_conformance -q

test: test-scheduler test-controlplane test-planning test-oss test-noc test-mobile test-cache conformance
	@echo
	@echo "all suites passed"

fmt:
	cd controlplane && gofmt -w .
	@echo "formatted"

vet:
	cd controlplane && go vet ./...

# ---- running ---------------------------------------------------------------

seed: controlplane scheduler
	@mkdir -p var
	@test -f $(CONFIG) || (echo "no $(CONFIG); copy deploy/config.example.json to it first" && exit 1)
	./$(BIN)/basestationd -config $(CONFIG) seed
	java -jar oss/build/bswisp-oss.jar seed --store var/oss.json

run: controlplane scheduler
	./$(BIN)/basestationd -config $(CONFIG) run

demo: build
	@./deploy/demo.sh

stop:
	@./deploy/demo.sh stop

clean:
	rm -rf scheduler/build $(BIN) oss/build noc/bin noc/obj mobile/var cache/var
	find . -name '__pycache__' -type d -prune -exec rm -rf {} + 2>/dev/null || true
	@echo "cleaned (var/ left alone; remove it by hand to reset the network)"
