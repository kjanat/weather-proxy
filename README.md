# weather-proxy

Small HTTP service that returns a cached, normalized current temperature for a configured location.

It fetches upstream weather from `wttr.in`, normalizes the response to a compact plain-text value such as `10C`, and keeps the value in memory for the configured TTL. If the upstream request fails after a value has already been cached, the service returns the stale cached value instead of failing.

## Public API

Public base URL: `https://weather.api.kjanat.com/`

### `GET /`

Returns the current cached weather value as plain text.

Example response body:

```text
10C
```

Response headers:

```http
Content-Type: text/plain; charset=utf-8
Cache-Control: public, max-age=900
X-Weather-Cache: HIT
```

`X-Weather-Cache` values:

- `HIT`: returned the current cached value or a freshly fetched value
- `STALE`: upstream fetch failed, so a previous cached value was returned

Status codes:

- `200 OK`: weather value returned
- `502 Bad Gateway`: upstream fetch failed and no cached value exists

### `GET /healthz`

Health check endpoint.

Status codes:

- `204 No Content`: service is running

### `GET /favicon.ico`

Returns the service favicon as SVG image data.

Status codes:

- `200 OK`: favicon returned

### Other Paths

All other paths return `404 Not Found`.
