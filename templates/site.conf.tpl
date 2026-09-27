# Managed by edge — generated from edge.json. Change it with the edge CLI; edits here are overwritten.
# edge: {{TARGET}} -> {{DESC}}
server {
    listen 443 ssl;
    http2 on;
    server_name {{SERVER_NAMES}};

    include /etc/nginx/edge/snippets/ssl/{{CERT}}.conf;
    include /etc/nginx/edge/snippets/security-headers.conf;
    # Let's Encrypt HTTP-01 renewals that arrive over HTTPS (Cloudflare "Always Use HTTPS")
    include /etc/nginx/edge/snippets/acme-challenge.conf;

    # Path-mounted projects on this host (edge path add {{HOST}}/<path>)
    include /etc/nginx/edge/locations/{{HOST}}/*.conf;

{{BODY}}
}
