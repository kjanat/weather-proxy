// Package main implements weather-proxy, a small HTTP service that exposes a
// cached plain-text weather value for one configured location.
//
// The public contract is intentionally small: GET / returns the cached weather
// value, GET /healthz reports process health, and all other paths return 404.
// Weather data is fetched from wttr.in, normalized for status-bar use, and kept
// in memory for WEATHER_TTL.
package main
