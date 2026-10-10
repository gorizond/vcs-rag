package main

// version is overridden at build time via:
//
//	go build -ldflags "-X main.version=v1.2.3"
var version = "dev"
