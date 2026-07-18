// The occurrence queue (ADR-025 §D3) makes IndexedDB part of the scan path;
// jsdom has none, so every suite runs over fake-indexeddb.
import 'fake-indexeddb/auto'
