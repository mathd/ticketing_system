//go:build smoke

package store

// Export the smoke helpers to external-package tests that need the real store without
// creating an import cycle through recovery.
var OutboxDBForTest = outboxDB
var SeedStuckForTest = seedStuck
