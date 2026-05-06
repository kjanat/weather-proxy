FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod go.sum ./
COPY *.go ./
COPY favicon.ico ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /weather-proxy .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /weather-proxy /weather-proxy
EXPOSE 8080
ENTRYPOINT ["/weather-proxy"]
