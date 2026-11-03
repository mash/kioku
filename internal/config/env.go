package config

import "os"

// Env returns a nonempty environment setting or the fallback.
func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
