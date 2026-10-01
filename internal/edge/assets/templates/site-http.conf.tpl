
# Plain HTTP as well (edge http {{HOST}} serve) — port 80 is not redirected to HTTPS.
server {
    listen 80;
    server_name {{SERVER_NAMES}};

    include /etc/nginx/edge/snippets/security-headers.conf;
    include /etc/nginx/edge/snippets/acme-challenge.conf;
    include /etc/nginx/edge/locations/{{HOST}}/*.conf;

{{BODY}}
}
