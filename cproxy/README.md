# C Weather Proxy

Experimental C implementation of the Go weather proxy. It is intentionally kept in this subdirectory so it can be built and tested without replacing the live service.

## Build

```sh
make -C cproxy
```

Requires a C compiler. This version has no external runtime dependencies beyond libc, but the upstream URL must use plain HTTP.

## Run

```sh
LISTEN_ADDR=:18080 WEATHER_LOCATION=Amsterdam WEATHER_TTL=15m ./cproxy/weather-proxy-c
```

Supported environment variables:

- `LISTEN_ADDR`: bind address, default `:8080`
- `WEATHER_LOCATION`: wttr.in location or coordinates, default `Amsterdam`
- `WEATHER_TTL`: cache duration with `s`, `m`, or `h` suffix, default `15m`
- `WEATHER_UPSTREAM`: upstream base URL, default `http://wttr.in` HTTP only

Implemented endpoints:

- `GET /` and `HEAD /`: cached plain-text temperature
- `GET /healthz`: `204 No Content`
