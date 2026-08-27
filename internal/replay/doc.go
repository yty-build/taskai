// Package replay reads bounded ranges from exact provider-native Session histories.
//
// Replay content is transient. The package does not persist provider records,
// discover Sessions, or expose provider paths. Connectors inject exact provider
// roots and supported history APIs, while TaskAI stores only an opaque reference
// that the same connector also uses for Resume.
package replay
