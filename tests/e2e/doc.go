// Package e2e contains scenario-driven integration tests for Fabrik.
//
// These tests drive a real Fabrik instance against real GitHub repositories.
// They are slow, costly (Claude tokens), and gated behind the e2e build tag.
//
// See README.md in this directory for setup, prerequisites, and conventions.
//
//go:build e2e

package e2e
