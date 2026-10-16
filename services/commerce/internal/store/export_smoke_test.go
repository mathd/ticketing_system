//go:build smoke

package store

// Export the smoke helpers to external-package tests that need the real store without
// creating an import cycle through recovery.
var OutboxDBForTest = outboxDB
var SeedStuckForTest = seedStuck

// SeedZeroStuckForTest is seedZeroStuck for external-package tests (TKT-285).
var SeedZeroStuckForTest = seedZeroStuck
