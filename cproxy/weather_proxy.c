#define _POSIX_C_SOURCE 200809L

#include <ctype.h>
#include <errno.h>
#include <netdb.h>
#include <signal.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

#define DEFAULT_LISTEN_ADDR ":8080"
#define DEFAULT_LOCATION "Amsterdam"
#define DEFAULT_TTL_SECONDS 900L
#define DEFAULT_UPSTREAM "http://wttr.in"
#define UPSTREAM_TIMEOUT_SECONDS 5L
#define MAX_REQUEST 4096
#define MAX_RESPONSE 8192
#define MAX_UPSTREAM_BODY 1024
#define MAX_WEATHER_VALUE 64

typedef enum {
    CACHE_HIT,
    CACHE_MISS,
    CACHE_STALE,
} cache_status;

typedef struct {
    char value[MAX_WEATHER_VALUE];
    time_t fetched_at;
} weather_cache;

typedef struct {
    char host[256];
    char port[16];
    char path[512];
} upstream_config;

static volatile sig_atomic_t stop_requested = 0;

static void handle_signal(int signum) {
    (void)signum;
    stop_requested = 1;
}

static const char *env_or_default(const char *key, const char *fallback) {
    const char *value = getenv(key);
    return value && value[0] ? value : fallback;
}

static long parse_duration_seconds(const char *value, long fallback) {
    if (!value || !value[0]) {
        return fallback;
    }

    errno = 0;
    char *end = NULL;
    double amount = strtod(value, &end);
    if (errno != 0 || end == value || amount <= 0) {
        return fallback;
    }

    while (*end && isspace((unsigned char)*end)) {
        end++;
    }

    double multiplier = 1;
    if (*end == '\0' || strcmp(end, "s") == 0) {
        multiplier = 1;
    } else if (strcmp(end, "m") == 0) {
        multiplier = 60;
    } else if (strcmp(end, "h") == 0) {
        multiplier = 3600;
    } else {
        return fallback;
    }

    long seconds = (long)(amount * multiplier);
    return seconds > 0 ? seconds : fallback;
}

static void copy_trimmed_upstream(char *dest, size_t dest_size, const char *src) {
    size_t len = strlen(src);
    while (len > 0 && src[len - 1] == '/') {
        len--;
    }
    if (len >= dest_size) {
        len = dest_size - 1;
    }
    memcpy(dest, src, len);
    dest[len] = '\0';
}

static int parse_http_upstream(const char *raw, upstream_config *cfg) {
    const char prefix[] = "http://";
    if (strncmp(raw, prefix, sizeof(prefix) - 1) != 0 || strchr(raw, '?') || strchr(raw, '#')) {
        return -1;
    }

    const char *rest = raw + sizeof(prefix) - 1;
    const char *path_start = strchr(rest, '/');
    const char *host_end = path_start ? path_start : raw + strlen(raw);
    size_t host_len = (size_t)(host_end - rest);
    if (host_len == 0 || host_len >= sizeof(cfg->host)) {
        return -1;
    }

    char hostport[sizeof(cfg->host)];
    memcpy(hostport, rest, host_len);
    hostport[host_len] = '\0';

    char *colon = strrchr(hostport, ':');
    if (colon && strchr(hostport, ':') == colon) {
        *colon = '\0';
        if (colon[1] == '\0') {
            return -1;
        }
        snprintf(cfg->port, sizeof(cfg->port), "%s", colon + 1);
    } else {
        snprintf(cfg->port, sizeof(cfg->port), "80");
    }

    if (hostport[0] == '\0') {
        return -1;
    }

    snprintf(cfg->host, sizeof(cfg->host), "%s", hostport);
    snprintf(cfg->path, sizeof(cfg->path), "%s", path_start ? path_start : "");
    return 0;
}

static int parse_listen_addr(const char *raw, char *host, size_t host_size, char *port, size_t port_size) {
    const char *colon = strrchr(raw, ':');
    if (!colon) {
        host[0] = '\0';
        snprintf(port, port_size, "%s", raw);
        return port[0] ? 0 : -1;
    }

    if (colon == raw) {
        host[0] = '\0';
    } else {
        size_t host_len = (size_t)(colon - raw);
        if (host_len >= host_size) {
            host_len = host_size - 1;
        }
        memcpy(host, raw, host_len);
        host[host_len] = '\0';
    }

    snprintf(port, port_size, "%s", colon + 1);
    return port[0] ? 0 : -1;
}

static int listen_socket(const char *listen_addr) {
    char host[256];
    char port[32];
    if (parse_listen_addr(listen_addr, host, sizeof(host), port, sizeof(port)) != 0) {
        fprintf(stderr, "invalid LISTEN_ADDR=%s\n", listen_addr);
        return -1;
    }

    struct addrinfo hints;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    hints.ai_flags = AI_PASSIVE;

    struct addrinfo *result = NULL;
    int gai = getaddrinfo(host[0] ? host : NULL, port, &hints, &result);
    if (gai != 0) {
        fprintf(stderr, "getaddrinfo: %s\n", gai_strerror(gai));
        return -1;
    }

    int fd = -1;
    for (struct addrinfo *rp = result; rp; rp = rp->ai_next) {
        fd = socket(rp->ai_family, rp->ai_socktype, rp->ai_protocol);
        if (fd == -1) {
            continue;
        }

        int yes = 1;
        (void)setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &yes, sizeof(yes));

        if (bind(fd, rp->ai_addr, rp->ai_addrlen) == 0 && listen(fd, 64) == 0) {
            break;
        }

        close(fd);
        fd = -1;
    }

    freeaddrinfo(result);
    return fd;
}

static int connect_upstream(const upstream_config *upstream) {
    struct addrinfo hints;
    memset(&hints, 0, sizeof(hints));
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;

    struct addrinfo *result = NULL;
    int gai = getaddrinfo(upstream->host, upstream->port, &hints, &result);
    if (gai != 0) {
        return -1;
    }

    int fd = -1;
    for (struct addrinfo *rp = result; rp; rp = rp->ai_next) {
        fd = socket(rp->ai_family, rp->ai_socktype, rp->ai_protocol);
        if (fd == -1) {
            continue;
        }

        struct timeval timeout;
        timeout.tv_sec = UPSTREAM_TIMEOUT_SECONDS;
        timeout.tv_usec = 0;
        (void)setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout));
        (void)setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout));

        if (connect(fd, rp->ai_addr, rp->ai_addrlen) == 0) {
            break;
        }

        close(fd);
        fd = -1;
    }

    freeaddrinfo(result);
    return fd;
}

static int append_escaped_path_segment(char *dest, size_t dest_size, const char *value) {
    static const char hex[] = "0123456789ABCDEF";
    size_t out = strlen(dest);

    for (size_t i = 0; value[i]; i++) {
        unsigned char ch = (unsigned char)value[i];
        bool unreserved = isalnum(ch) || ch == '-' || ch == '.' || ch == '_' || ch == '~';
        if (unreserved) {
            if (out + 1 >= dest_size) {
                return -1;
            }
            dest[out++] = (char)ch;
        } else {
            if (out + 3 >= dest_size) {
                return -1;
            }
            dest[out++] = '%';
            dest[out++] = hex[ch >> 4];
            dest[out++] = hex[ch & 0x0F];
        }
    }

    dest[out] = '\0';
    return 0;
}

static int send_all(int fd, const char *buf, size_t len) {
    while (len > 0) {
        ssize_t sent = send(fd, buf, len, MSG_NOSIGNAL);
        if (sent < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -1;
        }
        buf += sent;
        len -= (size_t)sent;
    }
    return 0;
}

static void trim_ascii(char *value) {
    char *start = value;
    while (*start && isspace((unsigned char)*start)) {
        start++;
    }

    if (start != value) {
        memmove(value, start, strlen(start) + 1);
    }

    size_t len = strlen(value);
    while (len > 0 && isspace((unsigned char)value[len - 1])) {
        value[--len] = '\0';
    }
}

static bool contains_unknown(const char *value) {
    const char needle[] = "unknown";
    size_t needle_len = sizeof(needle) - 1;
    size_t value_len = strlen(value);

    if (value_len < needle_len) {
        return false;
    }

    for (size_t i = 0; i <= value_len - needle_len; i++) {
        size_t j = 0;
        for (; j < needle_len; j++) {
            if (tolower((unsigned char)value[i + j]) != needle[j]) {
                break;
            }
        }
        if (j == needle_len) {
            return true;
        }
    }
    return false;
}

static int normalize_weather_value(const char *input, char *output, size_t output_size) {
    char trimmed[MAX_UPSTREAM_BODY + 1];
    snprintf(trimmed, sizeof(trimmed), "%s", input);
    trim_ascii(trimmed);

    size_t out = 0;
    for (size_t i = 0; trimmed[i] && out + 1 < output_size; i++) {
        unsigned char ch = (unsigned char)trimmed[i];
        if (ch == '+' || isspace(ch)) {
            continue;
        }
        if (ch == 0xC2 && (unsigned char)trimmed[i + 1] == 0xB0) {
            i++;
            continue;
        }
        output[out++] = (char)ch;
    }
    output[out] = '\0';

    if (out == 0 || contains_unknown(output)) {
        return -1;
    }
    return 0;
}

static int read_upstream_body(int fd, char *body, size_t body_size, int *status_code) {
    char response[MAX_RESPONSE + 1];
    size_t response_len = 0;
    char *body_start = NULL;

    while (response_len < MAX_RESPONSE) {
        ssize_t n = recv(fd, response + response_len, MAX_RESPONSE - response_len, 0);
        if (n < 0) {
            if (errno == EINTR) {
                continue;
            }
            return -1;
        }
        if (n == 0) {
            break;
        }

        response_len += (size_t)n;
        response[response_len] = '\0';

        if (!body_start) {
            char *sep = strstr(response, "\r\n\r\n");
            if (sep) {
                body_start = sep + 4;
            }
        }
        if (body_start && response_len - (size_t)(body_start - response) >= body_size - 1) {
            break;
        }
    }

    response[response_len] = '\0';
    if (sscanf(response, "HTTP/%*s %d", status_code) != 1) {
        return -1;
    }

    if (!body_start) {
        char *sep = strstr(response, "\r\n\r\n");
        if (!sep) {
            return -1;
        }
        body_start = sep + 4;
    }

    size_t offset = (size_t)(body_start - response);
    size_t available = response_len > offset ? response_len - offset : 0;
    if (available >= body_size) {
        available = body_size - 1;
    }
    memcpy(body, body_start, available);
    body[available] = '\0';
    return 0;
}

static int fetch_weather(const upstream_config *upstream, const char *location, char *value, size_t value_size) {
    char path[1536];
    snprintf(path, sizeof(path), "%s/", upstream->path);
    if (append_escaped_path_segment(path, sizeof(path), location) != 0) {
        return -1;
    }

    size_t path_len = strlen(path);
    if (path_len + strlen("?m&format=%t") + 1 >= sizeof(path)) {
        return -1;
    }
    strcat(path, "?m&format=%t");

    int fd = connect_upstream(upstream);
    if (fd < 0) {
        return -1;
    }

    char request[2048];
    int n = snprintf(request, sizeof(request),
        "GET %s HTTP/1.1\r\n"
        "Host: %s\r\n"
        "User-Agent: weather-proxy-c/experimental\r\n"
        "Accept: text/plain\r\n"
        "Accept-Encoding: identity\r\n"
        "Connection: close\r\n"
        "\r\n",
        path,
        upstream->host);
    if (n < 0 || (size_t)n >= sizeof(request) || send_all(fd, request, (size_t)n) != 0) {
        close(fd);
        return -1;
    }

    char body[MAX_UPSTREAM_BODY + 1];
    int status_code = 0;
    int rc = read_upstream_body(fd, body, sizeof(body), &status_code);
    close(fd);

    if (rc != 0 || status_code < 200 || status_code > 299) {
        return -1;
    }
    return normalize_weather_value(body, value, value_size);
}

static const char *cache_status_string(cache_status status) {
    switch (status) {
    case CACHE_HIT:
        return "HIT";
    case CACHE_MISS:
        return "MISS";
    case CACHE_STALE:
        return "STALE";
    }
    return "MISS";
}

static int get_weather(weather_cache *cache, const upstream_config *upstream, const char *location, long ttl_seconds, char *value, size_t value_size, cache_status *status) {
    time_t now = time(NULL);
    if (cache->value[0] && now - cache->fetched_at < ttl_seconds) {
        snprintf(value, value_size, "%s", cache->value);
        *status = CACHE_HIT;
        return 0;
    }

    char fetched[MAX_WEATHER_VALUE];
    if (fetch_weather(upstream, location, fetched, sizeof(fetched)) == 0) {
        snprintf(cache->value, sizeof(cache->value), "%s", fetched);
        cache->fetched_at = now;
        snprintf(value, value_size, "%s", cache->value);
        *status = CACHE_MISS;
        return 0;
    }

    if (cache->value[0]) {
        snprintf(value, value_size, "%s", cache->value);
        *status = CACHE_STALE;
        return 0;
    }

    return -1;
}

static void send_response(int fd, int status, const char *reason, const char *content_type, const char *extra_headers, const char *body, bool head_only) {
    size_t body_len = body ? strlen(body) : 0;
    char headers[1024];
    int n = snprintf(headers, sizeof(headers),
        "HTTP/1.1 %d %s\r\n"
        "Content-Length: %zu\r\n"
        "Connection: close\r\n"
        "%s%s%s"
        "%s"
        "\r\n",
        status,
        reason,
        body_len,
        content_type ? "Content-Type: " : "",
        content_type ? content_type : "",
        content_type ? "\r\n" : "",
        extra_headers ? extra_headers : "");

    if (n < 0 || (size_t)n >= sizeof(headers)) {
        return;
    }

    (void)send_all(fd, headers, (size_t)n);
    if (!head_only && body_len > 0) {
        (void)send_all(fd, body, body_len);
    }
}

static void handle_client(int client_fd, weather_cache *cache, const upstream_config *upstream, const char *location, long ttl_seconds) {
    char request[MAX_REQUEST + 1];
    ssize_t got = recv(client_fd, request, MAX_REQUEST, 0);
    if (got <= 0) {
        return;
    }
    request[got] = '\0';

    char method[16];
    char path[1024];
    if (sscanf(request, "%15s %1023s", method, path) != 2) {
        send_response(client_fd, 400, "Bad Request", "text/plain; charset=utf-8", NULL, "bad request\n", false);
        return;
    }

    bool head_only = strcmp(method, "HEAD") == 0;
    if (strcmp(method, "GET") != 0 && !head_only) {
        send_response(client_fd, 405, "Method Not Allowed", "text/plain; charset=utf-8", "Allow: GET, HEAD\r\n", "method not allowed\n", false);
        return;
    }

    if (strcmp(path, "/healthz") == 0) {
        send_response(client_fd, 204, "No Content", NULL, NULL, NULL, head_only);
        return;
    }

    if (strcmp(path, "/") != 0) {
        send_response(client_fd, 404, "Not Found", "text/plain; charset=utf-8", NULL, "not found\n", head_only);
        return;
    }

    char value[MAX_WEATHER_VALUE];
    cache_status cache_state;
    if (get_weather(cache, upstream, location, ttl_seconds, value, sizeof(value), &cache_state) != 0) {
        send_response(client_fd, 502, "Bad Gateway", "text/plain; charset=utf-8", NULL, "bad gateway\n", head_only);
        return;
    }

    char headers[256];
    snprintf(headers, sizeof(headers),
        "Cache-Control: public, max-age=%ld\r\n"
        "X-Weather-Cache: %s\r\n",
        ttl_seconds,
        cache_status_string(cache_state));
    send_response(client_fd, 200, "OK", "text/plain; charset=utf-8", headers, value, head_only);
}

int main(void) {
    const char *listen_addr = env_or_default("LISTEN_ADDR", DEFAULT_LISTEN_ADDR);
    const char *location = env_or_default("WEATHER_LOCATION", DEFAULT_LOCATION);
    long ttl_seconds = parse_duration_seconds(getenv("WEATHER_TTL"), DEFAULT_TTL_SECONDS);

    char upstream_raw[1024];
    copy_trimmed_upstream(upstream_raw, sizeof(upstream_raw), env_or_default("WEATHER_UPSTREAM", DEFAULT_UPSTREAM));

    upstream_config upstream;
    if (parse_http_upstream(upstream_raw, &upstream) != 0) {
        fprintf(stderr, "invalid WEATHER_UPSTREAM=%s; C proxy supports http:// upstreams only\n", upstream_raw);
        return 1;
    }

    signal(SIGINT, handle_signal);
    signal(SIGTERM, handle_signal);

    int server_fd = listen_socket(listen_addr);
    if (server_fd < 0) {
        return 1;
    }

    fprintf(stderr, "weather-proxy-c listening on %s location=%s ttl=%lds upstream=%s\n", listen_addr, location, ttl_seconds, upstream_raw);

    weather_cache cache;
    memset(&cache, 0, sizeof(cache));

    while (!stop_requested) {
        int client_fd = accept(server_fd, NULL, NULL);
        if (client_fd < 0) {
            if (errno == EINTR) {
                continue;
            }
            perror("accept");
            break;
        }

        handle_client(client_fd, &cache, &upstream, location, ttl_seconds);
        close(client_fd);
    }

    close(server_fd);
    return 0;
}
