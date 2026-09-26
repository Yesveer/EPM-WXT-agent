//go:build !windows

package main

// describeElevation is Windows-only; elsewhere there is no UIPI equivalent
// that silently drops synthetic input.
func describeElevation() string { return "n/a" }
