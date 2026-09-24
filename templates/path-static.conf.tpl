# Managed by edge — generated from edge.json. Change it with the edge CLI; edits here are overwritten.
# edge: {{TARGET}} -> {{DESC}}
location = {{PATH}} { return 301 {{PATH}}/$is_args$args; }

location {{PATH}}/ {
    alias /var/www/{{DIR}}/;
    # SPA fallback; for a multi-page site use: try_files $uri $uri/ =404;
    try_files $uri $uri/ {{PATH}}/index.html;
}
