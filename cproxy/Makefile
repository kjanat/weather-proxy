CC ?= cc
CFLAGS ?= -std=c11 -Wall -Wextra -O2

.PHONY: all clean run

all: weather-proxy-c

weather-proxy-c: weather_proxy.c
	$(CC) $(CFLAGS) -o $@ $<

run: weather-proxy-c
	./weather-proxy-c

clean:
	rm -f weather-proxy-c
