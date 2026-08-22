package main

// version is the build version, injected at link time.
//
//	go build -ldflags "-X main.version=$(cat VERSION)" ./cmd/orchestrator
//
// It used to be a string literal inside the /healthz handler, which meant the version
// the binary REPORTED and the version in ./VERSION drifted apart silently — /healthz
// still answered "0.2.1" long after VERSION moved on. "dev" is the correct answer for
// an un-stamped local build and is visibly not a release.
var version = "dev"

// Version returns the build version.
func Version() string { return version }
