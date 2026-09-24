# Managed by edge — generated from edge.json. Change it with the edge CLI; edits here are overwritten.
# edge: {{TARGET}} -> {{DESC}}
server {
    listen 443 ssl;
    http2 on;
    server_name {{SERVER_NAMES}};

    include /etc/nginx/edge/snippets/ssl/{{CERT}}.conf;
    include /etc/nginx/edge/snippets/security-headers.conf;

    # Path-mounted projects on this host (edge path add {{HOST}}/<path>)
    include /etc/nginx/edge/locations/{{HOST}}/*.conf;

{{BODY}}
}
