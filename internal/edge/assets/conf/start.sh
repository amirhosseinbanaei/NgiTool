#!/bin/sh
# Container command: reload nginx every 6h (picks up renewed certs), run nginx in foreground.
CONF=/etc/nginx/edge/nginx.conf
(
    while :; do
        sleep 21600
        nginx -c "$CONF" -t -q && nginx -c "$CONF" -s reload
    done
) &
exec nginx -c "$CONF" -g 'daemon off;'
